package runner

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func TestRetryPrompt(t *testing.T) {
	t.Parallel()

	const base = "Remove the double Face ID prompt on cold launch."

	tests := []struct {
		name     string
		failure  failure
		wantSubs []string
		wantOut  []string
	}{
		{
			name: "predicate failure quotes the check's own words",
			failure: failure{
				Kind:   failPredicate,
				Detail: "predicate did not pass for lane L1",
				Output: "FAIL: saw 2 biometric prompts, expected 1",
			},
			wantSubs: []string{
				base,
				"Attempt 1 of this lane did not complete: predicate failed",
				"acceptance predicate ran after that attempt and failed",
				"your own report that the work is done does not count",
				"FAIL: saw 2 biometric prompts, expected 1",
			},
		},
		{
			name:    "stall failure names the silence",
			failure: failure{Kind: failStall, Detail: "no output for 10m0s", Output: "last line before the freeze"},
			wantSubs: []string{
				"stalled: no output for 10m0s",
				"produced no output for the",
				"last line before the freeze",
			},
		},
		{
			name:    "deadline failure tells the next attempt to prioritise",
			failure: failure{Kind: failDeadline, Detail: "exceeded 90m0s"},
			wantSubs: []string{
				"deadline exceeded: exceeded 90m0s",
				"Prioritise making the",
			},
			wantOut: []string{"```"},
		},
		{
			name:     "non-zero exit falls back to the generic framing",
			failure:  failure{Kind: failExit, Detail: "claude exited 2", Output: "panic: nope"},
			wantSubs: []string{"non-zero exit: claude exited 2", "panic: nope"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := retryPrompt(base, 1, tc.failure)
			for _, want := range tc.wantSubs {
				if !strings.Contains(got, want) {
					t.Errorf("retryPrompt() missing %q; got:\n%s", want, got)
				}
			}
			for _, unwanted := range tc.wantOut {
				if strings.Contains(got, unwanted) {
					t.Errorf("retryPrompt() contained %q with no output to quote; got:\n%s", unwanted, got)
				}
			}
		})
	}
}

func TestRetryPromptAlwaysKeepsTheOriginalAsk(t *testing.T) {
	t.Parallel()

	const base = "the original instruction"
	got := retryPrompt(base, 3, failure{Kind: failPredicate, Output: "nope"})
	if !strings.HasPrefix(got, base) {
		t.Errorf("retryPrompt() = %q, want it to start with the original prompt", got)
	}
	if !strings.Contains(got, "Attempt 3") {
		t.Error("retryPrompt() did not name the attempt number")
	}
}

func TestFailureString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		f    failure
		want string
	}{
		{"with detail", failure{Kind: failStall, Detail: "no output for 1m0s"}, "stalled: no output for 1m0s"},
		{"without detail", failure{Kind: failPredicate}, "predicate failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.f.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTailBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		max     int
		want    string
		wantSub string
	}{
		{name: "shorter than the cap is untouched", in: "hello", max: 10, want: "hello"},
		{name: "exactly the cap is untouched", in: "hello", max: 5, want: "hello"},
		{name: "zero cap disables truncation", in: "hello", max: 0, want: "hello"},
		{name: "longer than the cap keeps the tail", in: "abcdefghij", max: 4, wantSub: "ghij"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tailBytes(tc.in, tc.max)
			if tc.want != "" && got != tc.want {
				t.Errorf("tailBytes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if tc.wantSub != "" {
				if !strings.HasSuffix(got, tc.wantSub) {
					t.Errorf("tailBytes(%q, %d) = %q, want it to end with %q", tc.in, tc.max, got, tc.wantSub)
				}
				if !strings.Contains(got, "truncated") {
					t.Error("tailBytes() dropped content without saying so")
				}
			}
		})
	}
}

// TestTailBytesDoesNotSplitRunes guards against a truncated prompt that starts
// with an invalid UTF-8 fragment.
func TestTailBytesDoesNotSplitRunes(t *testing.T) {
	t.Parallel()

	in := strings.Repeat("é", 20) // two bytes per rune
	got := tailBytes(in, 9)
	body := strings.TrimPrefix(got, "[earlier output truncated]\n")
	for i, r := range body {
		if r == '�' {
			t.Fatalf("tailBytes() produced an invalid rune at byte %d: %q", i, body)
		}
	}
}

func TestActivityWriterTracksSilence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var sink strings.Builder
	w := newActivityWriter(&sink, clock)

	if got := w.IdleFor(now); got != 0 {
		t.Errorf("IdleFor() at creation = %v, want 0", got)
	}

	now = now.Add(30 * time.Second)
	if got, want := w.IdleFor(now), 30*time.Second; got != want {
		t.Errorf("IdleFor() after silence = %v, want %v", got, want)
	}

	if _, err := w.Write([]byte("output\n")); err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}
	if got := w.IdleFor(now); got != 0 {
		t.Errorf("IdleFor() right after a write = %v, want 0", got)
	}
	if got, want := w.Bytes(), int64(7); got != want {
		t.Errorf("Bytes() = %d, want %d", got, want)
	}
	if got, want := sink.String(), "output\n"; got != want {
		t.Errorf("forwarded output = %q, want %q", got, want)
	}

	now = now.Add(2 * time.Minute)
	if got, want := w.IdleFor(now), 2*time.Minute; got != want {
		t.Errorf("IdleFor() after a later silence = %v, want %v", got, want)
	}
}

func TestActivityWriterIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	w := newActivityWriter(&syncBuffer{}, time.Now)
	const writers = 16
	done := make(chan struct{}, writers)
	for i := 0; i < writers; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				if _, err := w.Write([]byte("x")); err != nil {
					t.Errorf("Write() error = %v", err)
				}
				_ = w.IdleFor(time.Now())
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < writers; i++ {
		<-done
	}
	if got, want := w.Bytes(), int64(writers*50); got != want {
		t.Errorf("Bytes() = %d, want %d", got, want)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	return len(p), nil
}
