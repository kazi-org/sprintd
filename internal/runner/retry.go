package runner

import (
	"fmt"
	"strings"
)

// maxContextBytes caps how much prior output is pasted into a retry prompt.
// The tail is what matters -- the error a lane died on is at the end -- so the
// head is dropped when the cap bites.
const maxContextBytes = 6000

// failureKind classifies why an attempt did not produce a verified lane.
type failureKind string

const (
	failStall     failureKind = "stalled"
	failDeadline  failureKind = "deadline exceeded"
	failExit      failureKind = "non-zero exit"
	failPredicate failureKind = "predicate failed"
	failLaunch    failureKind = "could not launch"
)

// failure is everything the next attempt should be told about the last one.
type failure struct {
	Kind failureKind
	// Detail is a single line naming what went wrong.
	Detail string
	// Output is the log tail or the predicate's combined output.
	Output string
}

// String renders the failure for a results record's reason field.
func (f failure) String() string {
	if f.Detail == "" {
		return string(f.Kind)
	}
	return fmt.Sprintf("%s: %s", f.Kind, f.Detail)
}

// retryPrompt appends the previous attempt's failure to the lane's prompt.
//
// This is the difference between a retry and a rerun. A bare rerun gives the
// agent the same instructions that already failed; attaching the predicate's
// own output tells it exactly which observable condition it has to make true,
// in the words of the check that will judge it again.
func retryPrompt(base string, attempt int, f failure) string {
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n---\n")
	fmt.Fprintf(&b, "Attempt %d of this lane did not complete: %s.\n", attempt, f)
	switch f.Kind {
	case failPredicate:
		b.WriteString("The acceptance predicate ran after that attempt and failed. " +
			"Its combined output is below. The lane is complete only when this " +
			"predicate exits zero, checked by a separate process -- your own " +
			"report that the work is done does not count.\n")
	case failStall:
		b.WriteString("That attempt was killed because it produced no output for the " +
			"stall window. Avoid whatever blocked it -- a prompt waiting on input, " +
			"a command waiting on a lock, a network call with no timeout.\n")
	case failDeadline:
		b.WriteString("That attempt was killed on its deadline. Prioritise making the " +
			"acceptance predicate pass over any broader cleanup.\n")
	default:
		b.WriteString("Output from that attempt is below.\n")
	}
	if trimmed := strings.TrimSpace(f.Output); trimmed != "" {
		b.WriteString("\n```\n")
		b.WriteString(tailBytes(trimmed, maxContextBytes))
		b.WriteString("\n```\n")
	}
	return b.String()
}

// tailBytes keeps the last max bytes of s, marking that it was cut.
func tailBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := s[len(s)-max:]
	// Avoid slicing a multi-byte rune in half.
	for len(cut) > 0 && !isUTF8Start(cut[0]) {
		cut = cut[1:]
	}
	return "[earlier output truncated]\n" + cut
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }
