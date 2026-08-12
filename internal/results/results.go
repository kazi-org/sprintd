// Package results records lane state transitions to an append-only JSONL file
// and renders them as a human table.
package results

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// FileName is the run-directory-relative name of the transition log.
const FileName = "results.jsonl"

// State is a lane's state at a transition.
type State string

// The lane state machine. Only Complete and Escalated are terminal.
const (
	StateDispatched      State = "dispatched"
	StateStalled         State = "stalled"
	StateKilled          State = "killed"
	StateRequeued        State = "requeued"
	StatePredicateFailed State = "predicate_failed"
	StateComplete        State = "complete"
	StateEscalated       State = "escalated"
)

// Terminal reports whether no further transition can follow s.
func (s State) Terminal() bool { return s == StateComplete || s == StateEscalated }

// Record is one lane state transition.
type Record struct {
	Time    time.Time `json:"ts"`
	Sprint  string    `json:"sprint"`
	Lane    string    `json:"lane"`
	State   State     `json:"state"`
	Attempt int       `json:"attempt"`
	Repo    string    `json:"repo,omitempty"`
	Machine string    `json:"machine,omitempty"`
	Account string    `json:"account,omitempty"`
	Model   string    `json:"model,omitempty"`
	// Reason explains a non-complete transition in one line.
	Reason string `json:"reason,omitempty"`
	// Worktree is the tree the lane ran in, and Branch the branch it was
	// checked out on. Both are recorded so the work an escalated lane left
	// behind can be found without guessing.
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	// ExitCode is the dispatched process's status where one exists.
	ExitCode *int `json:"exit_code,omitempty"`
	// PredicateOutput is the predicate's combined output, preserved on every
	// terminal state so a failure can be read without re-running anything.
	PredicateOutput string `json:"predicate_output,omitempty"`
	// DurationMS is how long the attempt's process ran.
	DurationMS int64 `json:"duration_ms,omitempty"`
}

// Recorder appends records to a JSONL file. It is safe for concurrent use.
type Recorder struct {
	mu sync.Mutex
	w  io.WriteCloser
}

// NewRecorder opens path for appending, creating it if needed.
func NewRecorder(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening results file %s: %w", path, err)
	}
	return &Recorder{w: f}, nil
}

// NewRecorderTo appends records to an arbitrary writer.
func NewRecorderTo(w io.WriteCloser) *Recorder { return &Recorder{w: w} }

// Record appends one transition. Each record is flushed immediately so a
// killed sprintd leaves a complete history behind it.
func (r *Recorder) Record(rec Record) error {
	if rec.Time.IsZero() {
		rec.Time = time.Now().UTC()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding record for lane %s: %w", rec.Lane, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("appending record for lane %s: %w", rec.Lane, err)
	}
	return nil
}

// Close releases the underlying file.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.w.Close(); err != nil {
		return fmt.Errorf("closing results file: %w", err)
	}
	return nil
}

// Load reads every record from a JSONL file, in file order.
func Load(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening results file %s: %w", path, err)
	}
	defer f.Close()
	return Read(f)
}

// Read decodes records from r, in stream order.
func Read(r io.Reader) ([]Record, error) {
	var out []Record
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			return nil, fmt.Errorf("decoding results line %d: %w", line, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading results: %w", err)
	}
	return out, nil
}

// ReadTolerant decodes records like Read, except a malformed final line is
// dropped instead of failing the whole read, and reported via truncated.
//
// This is the exact shape a process leaves behind when it is killed
// mid-write: every earlier record was already flushed by its own Write call,
// so at most the one in progress can be a partial line at EOF. A malformed
// line anywhere else is real corruption, not a partial write, and still
// fails the read.
func ReadTolerant(r io.Reader) (out []Record, truncated bool, err error) {
	var lines []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		lines = append(lines, text)
	}
	if err := sc.Err(); err != nil {
		return nil, false, fmt.Errorf("reading results: %w", err)
	}
	out = make([]Record, 0, len(lines))
	for i, text := range lines {
		var rec Record
		if unmarshalErr := json.Unmarshal([]byte(text), &rec); unmarshalErr != nil {
			if i == len(lines)-1 {
				return out, true, nil
			}
			return nil, false, fmt.Errorf("decoding results line %d: %w", i+1, unmarshalErr)
		}
		out = append(out, rec)
	}
	return out, false, nil
}

// LaneStatus is a lane's latest known state.
type LaneStatus struct {
	Lane     string
	State    State
	Attempt  int
	Machine  string
	Account  string
	Model    string
	Reason   string
	Duration time.Duration
}

// Summarise reduces a transition log to one row per lane, keeping the last
// transition seen for each and preserving first-appearance order.
func Summarise(recs []Record) []LaneStatus {
	latest := map[string]Record{}
	var order []string
	for _, rec := range recs {
		if _, seen := latest[rec.Lane]; !seen {
			order = append(order, rec.Lane)
		}
		latest[rec.Lane] = rec
	}
	out := make([]LaneStatus, 0, len(order))
	for _, lane := range order {
		rec := latest[lane]
		out = append(out, LaneStatus{
			Lane:     rec.Lane,
			State:    rec.State,
			Attempt:  rec.Attempt,
			Machine:  rec.Machine,
			Account:  rec.Account,
			Model:    rec.Model,
			Reason:   firstLine(rec.Reason),
			Duration: time.Duration(rec.DurationMS) * time.Millisecond,
		})
	}
	return out
}

// Escalated returns the ids of lanes whose latest state is escalated.
func Escalated(recs []Record) []string {
	var out []string
	for _, st := range Summarise(recs) {
		if st.State == StateEscalated {
			out = append(out, st.Lane)
		}
	}
	sort.Strings(out)
	return out
}

// RenderStatus writes a compact one-row-per-lane table.
func RenderStatus(w io.Writer, sprintName string, recs []Record) error {
	statuses := Summarise(recs)
	if sprintName == "" && len(recs) > 0 {
		sprintName = recs[0].Sprint
	}
	if _, err := fmt.Fprintf(w, "sprint: %s  lanes: %d\n\n", sprintName, len(statuses)); err != nil {
		return fmt.Errorf("writing status header: %w", err)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LANE\tSTATE\tATTEMPT\tMACHINE\tACCOUNT\tMODEL\tTOOK\tREASON")
	for _, st := range statuses {
		took := "-"
		if st.Duration > 0 {
			took = st.Duration.Round(time.Second).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			st.Lane, st.State, st.Attempt, dash(st.Machine), dash(st.Account),
			dash(st.Model), took, dash(truncate(st.Reason, 60)))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing status table: %w", err)
	}
	counts := map[State]int{}
	for _, st := range statuses {
		counts[st.State]++
	}
	_, err := fmt.Fprintf(w, "\ncomplete: %d  escalated: %d  in flight: %d\n",
		counts[StateComplete], counts[StateEscalated],
		len(statuses)-counts[StateComplete]-counts[StateEscalated])
	if err != nil {
		return fmt.Errorf("writing status summary: %w", err)
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
