package results_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kazi-org/sprintd/internal/results"
)

func TestRecorderRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), results.FileName)
	rec, err := results.NewRecorder(path)
	if err != nil {
		t.Fatalf("NewRecorder() error = %v, want nil", err)
	}

	exit := 0
	want := []results.Record{
		{Sprint: "s", Lane: "L1", State: results.StateDispatched, Attempt: 1, Machine: "mini", Account: "primary", Model: "opus"},
		{Sprint: "s", Lane: "L1", State: results.StatePredicateFailed, Attempt: 1, PredicateOutput: "FAIL: still two prompts", ExitCode: &exit},
		{Sprint: "s", Lane: "L1", State: results.StateComplete, Attempt: 2, DurationMS: 1500},
	}
	for _, r := range want {
		if err := rec.Record(r); err != nil {
			t.Fatalf("Record() error = %v, want nil", err)
		}
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	got, err := results.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Load() returned %d records, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].State != want[i].State || got[i].Lane != want[i].Lane || got[i].Attempt != want[i].Attempt {
			t.Errorf("record %d = %+v, want lane %s state %s attempt %d",
				i, got[i], want[i].Lane, want[i].State, want[i].Attempt)
		}
		if got[i].Time.IsZero() {
			t.Errorf("record %d has no timestamp", i)
		}
	}
	if got[1].PredicateOutput != "FAIL: still two prompts" {
		t.Errorf("predicate output = %q, want it preserved", got[1].PredicateOutput)
	}
	if got[1].ExitCode == nil || *got[1].ExitCode != 0 {
		t.Error("exit code 0 was not preserved; it must be distinguishable from absent")
	}
}

func TestRecorderAppends(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), results.FileName)
	for i := 0; i < 2; i++ {
		rec, err := results.NewRecorder(path)
		if err != nil {
			t.Fatalf("NewRecorder() error = %v, want nil", err)
		}
		if err := rec.Record(results.Record{Lane: "L1", State: results.StateDispatched, Attempt: i + 1}); err != nil {
			t.Fatalf("Record() error = %v, want nil", err)
		}
		if err := rec.Close(); err != nil {
			t.Fatalf("Close() error = %v, want nil", err)
		}
	}
	got, err := results.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Errorf("Load() returned %d records, want 2: reopening must append, never truncate", len(got))
	}
}

func TestSummarise(t *testing.T) {
	t.Parallel()

	recs := []results.Record{
		{Lane: "L1", State: results.StateDispatched, Attempt: 1},
		{Lane: "L2", State: results.StateDispatched, Attempt: 1},
		{Lane: "L1", State: results.StateStalled, Attempt: 1},
		{Lane: "L1", State: results.StateComplete, Attempt: 2, DurationMS: 2000},
		{Lane: "L2", State: results.StateEscalated, Attempt: 3, Reason: "retries exhausted\nsecond line"},
	}
	got := results.Summarise(recs)
	if len(got) != 2 {
		t.Fatalf("Summarise() returned %d rows, want one per lane", len(got))
	}
	if got[0].Lane != "L1" || got[0].State != results.StateComplete {
		t.Errorf("row 0 = %+v, want L1 complete (last transition wins)", got[0])
	}
	if got[0].Duration != 2*time.Second {
		t.Errorf("row 0 duration = %v, want 2s", got[0].Duration)
	}
	if got[1].Reason != "retries exhausted" {
		t.Errorf("row 1 reason = %q, want only the first line", got[1].Reason)
	}
}

func TestEscalated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		recs []results.Record
		want []string
	}{
		{
			name: "a lane that recovered is not escalated",
			recs: []results.Record{
				{Lane: "L1", State: results.StatePredicateFailed},
				{Lane: "L1", State: results.StateComplete},
			},
		},
		{
			name: "only the latest state counts",
			recs: []results.Record{
				{Lane: "L1", State: results.StateComplete},
				{Lane: "L2", State: results.StateDispatched},
				{Lane: "L2", State: results.StateEscalated},
				{Lane: "L3", State: results.StateEscalated},
			},
			want: []string{"L2", "L3"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := results.Escalated(tc.recs)
			if len(got) != len(tc.want) {
				t.Fatalf("Escalated() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Escalated()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestStateTerminal(t *testing.T) {
	t.Parallel()

	tests := map[results.State]bool{
		results.StateDispatched:      false,
		results.StateStalled:         false,
		results.StateKilled:          false,
		results.StateRequeued:        false,
		results.StatePredicateFailed: false,
		results.StateComplete:        true,
		results.StateEscalated:       true,
	}
	for state, want := range tests {
		if got := state.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", state, got, want)
		}
	}
}

func TestRenderStatus(t *testing.T) {
	t.Parallel()

	recs := []results.Record{
		{Sprint: "demo", Lane: "L1", State: results.StateComplete, Attempt: 1, Machine: "mini", Account: "primary", Model: "opus", DurationMS: 61000},
		{Sprint: "demo", Lane: "L2", State: results.StateEscalated, Attempt: 3, Machine: "dgx", Reason: "retries exhausted"},
	}
	var out strings.Builder
	if err := results.RenderStatus(&out, "demo", recs); err != nil {
		t.Fatalf("RenderStatus() error = %v, want nil", err)
	}
	text := out.String()
	for _, want := range []string{
		"sprint: demo", "LANE", "L1", "complete", "primary", "1m1s",
		"L2", "escalated", "retries exhausted",
		"complete: 1  escalated: 1  in flight: 0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("RenderStatus() output missing %q; got:\n%s", want, text)
		}
	}
}

func TestReadSkipsBlankLinesAndReportsBadOnes(t *testing.T) {
	t.Parallel()

	good := `{"lane":"L1","state":"complete"}

{"lane":"L2","state":"escalated"}
`
	got, err := results.Read(strings.NewReader(good))
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Errorf("Read() returned %d records, want 2", len(got))
	}

	if _, err := results.Read(strings.NewReader("{not json}\n")); err == nil {
		t.Error("Read() error = nil, want an error naming the bad line")
	} else if !strings.Contains(err.Error(), "line 1") {
		t.Errorf("Read() error = %v, want it to name the line number", err)
	}
}

func TestReadTolerantDropsOnlyATruncatedFinalLine(t *testing.T) {
	t.Parallel()

	// The exact shape a kill mid-write leaves: two complete records, and a
	// third cut off partway through its own Write call.
	truncated := `{"lane":"L1","state":"complete"}
{"lane":"L2","state":"dispatched"}
{"lane":"L3","state":"disp`
	recs, wasTruncated, err := results.ReadTolerant(strings.NewReader(truncated))
	if err != nil {
		t.Fatalf("ReadTolerant() error = %v, want nil: a partial final line must not fail the read", err)
	}
	if !wasTruncated {
		t.Error("ReadTolerant() truncated = false, want true")
	}
	if len(recs) != 2 {
		t.Fatalf("ReadTolerant() returned %d records, want the 2 complete ones", len(recs))
	}
	if recs[0].Lane != "L1" || recs[1].Lane != "L2" {
		t.Errorf("ReadTolerant() = %+v, want L1 then L2 in order", recs)
	}

	// A malformed line that is NOT the last one is real corruption, not a
	// partial write, and must still fail.
	corrupt := `{not json}
{"lane":"L2","state":"complete"}
`
	if _, _, err := results.ReadTolerant(strings.NewReader(corrupt)); err == nil {
		t.Error("ReadTolerant() error = nil, want an error: corruption in the middle of the file is not a partial write")
	}

	clean := `{"lane":"L1","state":"complete"}
`
	recs, wasTruncated, err = results.ReadTolerant(strings.NewReader(clean))
	if err != nil {
		t.Fatalf("ReadTolerant() error = %v, want nil", err)
	}
	if wasTruncated {
		t.Error("ReadTolerant() truncated = true, want false: every line here is well-formed")
	}
	if len(recs) != 1 {
		t.Errorf("ReadTolerant() returned %d records, want 1", len(recs))
	}
}
