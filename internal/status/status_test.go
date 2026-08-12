package status_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kazi-org/sprintd/internal/results"
	"github.com/kazi-org/sprintd/internal/status"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

var now = at("2026-08-12T12:00:00Z")

// writeRun lays out a run directory with the given manifest and events.
func writeRun(t *testing.T, m *status.Manifest, recs []results.Record) string {
	t.Helper()
	dir := t.TempDir()
	if m != nil {
		if err := status.WriteManifest(dir, *m); err != nil {
			t.Fatalf("WriteManifest() error = %v", err)
		}
	}
	if recs != nil {
		rec, err := results.NewRecorder(filepath.Join(dir, results.FileName))
		if err != nil {
			t.Fatalf("NewRecorder() error = %v", err)
		}
		for _, r := range recs {
			if err := rec.Record(r); err != nil {
				t.Fatalf("Record() error = %v", err)
			}
		}
		if err := rec.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
	return dir
}

func baseManifest() status.Manifest {
	opened := at("2026-08-12T08:00:00Z")
	closes := at("2026-08-12T20:00:00Z")
	pct := 42.5
	sampled := at("2026-08-12T11:00:00Z")
	return status.Manifest{
		Sprint:    "demo",
		Opened:    &opened,
		Closes:    &closes,
		StartedAt: at("2026-08-12T11:00:00Z"),
		PID:       os.Getpid(),
		Machines: []status.Machine{
			{Name: "mini", Host: "local"},
			{Name: "dgx", Host: "user@10.0.0.2"},
		},
		Lanes: []status.LaneSpec{
			{ID: "L1", Repo: "app", Machine: "mini", Goal: "the thing", Model: "opus"},
			{ID: "L2", Repo: "app", Machine: "dgx", Goal: "other", Command: "kazi apply goals/x.toml"},
		},
		Accounts: []status.AccountSample{
			{Name: "primary", ReserveFloorPct: 30, WeeklyTokens: 425, WeeklyLimit: 1000,
				WeeklyPct: &pct, Metered: true, SampledAt: &sampled, SampledOnce: true},
		},
	}
}

// TestNoRun is the state the dashboard has to be able to render honestly:
// nothing has run, which is not an error.
func TestNoRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dir  func(t *testing.T) string
	}{
		{name: "empty directory", dir: func(t *testing.T) string { return t.TempDir() }},
		{name: "no directory given", dir: func(t *testing.T) string { return "" }},
		{name: "directory does not exist", dir: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "never-created")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := status.Build(tc.dir(t), now)
			if err != nil {
				t.Fatalf("Build() error = %v, want nil: no run is a healthy answer, not a failure", err)
			}
			if got.Status != status.StatusNoRun {
				t.Errorf("Status = %q, want %q", got.Status, status.StatusNoRun)
			}
			if got.SchemaVersion != status.SchemaVersion {
				t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, status.SchemaVersion)
			}
			if got.Sprint != nil {
				t.Errorf("Sprint = %+v, want nil", got.Sprint)
			}
			// Collections must be empty, never null: a consumer should be able
			// to range over them without a nil check.
			body, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			for _, field := range []string{`"lanes":[]`, `"machines":[]`, `"accounts":[]`} {
				if !strings.Contains(string(body), field) {
					t.Errorf("payload missing %s; got %s", field, body)
				}
			}
		})
	}
}

func TestCompletedRun(t *testing.T) {
	t.Parallel()

	m := baseManifest()
	completed := at("2026-08-12T11:30:00Z")
	m.CompletedAt = &completed
	dir := writeRun(t, &m, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "demo", Lane: "L1", State: results.StateDispatched, Attempt: 1, Machine: "mini", Account: "primary", Model: "opus"},
		{Time: at("2026-08-12T11:10:00Z"), Sprint: "demo", Lane: "L1", State: results.StateComplete, Attempt: 1, Machine: "mini", Account: "primary", Model: "opus", PredicateOutput: "OK"},
		{Time: at("2026-08-12T11:00:12Z"), Sprint: "demo", Lane: "L2", State: results.StateDispatched, Attempt: 1, Machine: "dgx", Account: "primary"},
		{Time: at("2026-08-12T11:20:00Z"), Sprint: "demo", Lane: "L2", State: results.StateComplete, Attempt: 1, Machine: "dgx", Account: "primary"},
	})

	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if got.Status != status.StatusFinished {
		t.Fatalf("Status = %q, want %q", got.Status, status.StatusFinished)
	}
	if got.Sprint == nil || got.Sprint.Name != "demo" {
		t.Fatalf("Sprint = %+v, want the demo sprint", got.Sprint)
	}
	if got.Sprint.ElapsedSeconds == nil || *got.Sprint.ElapsedSeconds != 1800 {
		t.Errorf("ElapsedSeconds = %v, want 1800 (start to recorded completion, not to now)",
			got.Sprint.ElapsedSeconds)
	}
	if got.Totals.Converged != 2 || got.Totals.Lanes != 2 || got.Totals.Unresolved != 0 {
		t.Errorf("Totals = %+v, want 2 lanes both converged", got.Totals)
	}
	if got.Totals.ByState["complete"] != 2 {
		t.Errorf("ByState = %v, want two complete", got.Totals.ByState)
	}
	// A finished run has nothing running, so no machine is busy.
	for _, machine := range got.Machines {
		if machine.Busy || len(machine.RunningLanes) > 0 {
			t.Errorf("machine %s reads busy on a finished run: %+v", machine.Name, machine)
		}
	}
	if len(got.Machines) != 2 {
		t.Errorf("machines = %d, want both declared machines listed even when idle", len(got.Machines))
	}
	// The command lane's command comes from the manifest; the log never had it.
	var l2 status.Lane
	for _, lane := range got.Lanes {
		if lane.ID == "L2" {
			l2 = lane
		}
	}
	if l2.Command != "kazi apply goals/x.toml" {
		t.Errorf("L2 Command = %q, want it carried from the manifest", l2.Command)
	}
}

// TestRunningRun uses this test process's own pid, which is by definition
// alive, to stand in for a live sprintd.
func TestRunningRun(t *testing.T) {
	t.Parallel()

	m := baseManifest()
	m.PID = os.Getpid()
	host, _ := os.Hostname()
	m.Hostname = host
	dir := writeRun(t, &m, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "demo", Lane: "L1", State: results.StateDispatched, Attempt: 1, Machine: "mini", Account: "primary", Model: "opus"},
		{Time: at("2026-08-12T11:10:00Z"), Sprint: "demo", Lane: "L1", State: results.StateComplete, Attempt: 1, Machine: "mini"},
		{Time: at("2026-08-12T11:00:12Z"), Sprint: "demo", Lane: "L2", State: results.StateDispatched, Attempt: 1, Machine: "dgx", Account: "primary"},
	})

	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if got.Status != status.StatusRunning {
		t.Fatalf("Status = %q, want %q", got.Status, status.StatusRunning)
	}
	if got.Totals.Unresolved != 1 || got.Totals.Converged != 1 {
		t.Errorf("Totals = %+v, want one converged and one still in flight", got.Totals)
	}
	byName := map[string]status.MachineStatus{}
	for _, machine := range got.Machines {
		byName[machine.Name] = machine
	}
	if !byName["dgx"].Busy || len(byName["dgx"].RunningLanes) != 1 {
		t.Errorf("dgx = %+v, want busy with L2", byName["dgx"])
	}
	// This is the founder-facing point: a machine with nothing on it reads as
	// idle rather than being absent from the payload.
	if byName["mini"].Busy {
		t.Errorf("mini = %+v, want idle once its lane completed", byName["mini"])
	}
	if got.Sprint.ElapsedSeconds == nil || *got.Sprint.ElapsedSeconds != 3600 {
		t.Errorf("ElapsedSeconds = %v, want 3600 (start to now, for a live run)", got.Sprint.ElapsedSeconds)
	}
}

// TestInterruptedRun is the killed-mid-flight case: the manifest never
// recorded completion and the process is gone.
func TestInterruptedRun(t *testing.T) {
	t.Parallel()

	m := baseManifest()
	// A pid that cannot be running. Kernel pid 0 is never a user process.
	m.PID = 0
	dir := writeRun(t, &m, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "demo", Lane: "L1", State: results.StateDispatched, Attempt: 1, Machine: "mini"},
		{Time: at("2026-08-12T11:00:12Z"), Sprint: "demo", Lane: "L2", State: results.StateDispatched, Attempt: 1, Machine: "dgx"},
	})

	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil: a killed run must still report", err)
	}
	if got.Status == status.StatusRunning {
		t.Fatalf("Status = %q, want anything but running: the process is gone", got.Status)
	}
	if got.Totals.Unresolved != 2 {
		t.Errorf("Unresolved = %d, want 2 abandoned lanes", got.Totals.Unresolved)
	}
	// Nothing is running, so nothing may read as busy, even though two lanes
	// are mid-flight in the log.
	for _, machine := range got.Machines {
		if machine.Busy || len(machine.RunningLanes) > 0 {
			t.Errorf("machine %s reads busy on a dead run: %+v", machine.Name, machine)
		}
		if len(machine.UnresolvedLanes) != 1 {
			t.Errorf("machine %s = %+v, want its abandoned lane listed", machine.Name, machine)
		}
	}
	if len(got.Notes) == 0 {
		t.Error("an interrupted run carried no note explaining itself")
	}
}

func TestEscalatedLane(t *testing.T) {
	t.Parallel()

	m := baseManifest()
	completed := at("2026-08-12T11:40:00Z")
	m.CompletedAt = &completed
	dir := writeRun(t, &m, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "demo", Lane: "L1", State: results.StateDispatched, Attempt: 1, Machine: "mini"},
		{Time: at("2026-08-12T11:10:00Z"), Sprint: "demo", Lane: "L1", State: results.StatePredicateFailed, Attempt: 1, Machine: "mini", PredicateOutput: "FAIL: still two prompts"},
		{Time: at("2026-08-12T11:30:00Z"), Sprint: "demo", Lane: "L1", State: results.StateEscalated, Attempt: 3, Machine: "mini",
			Reason: "3 attempts exhausted", PredicateOutput: "FAIL: still two prompts", Worktree: "/srv/wt/L1", Branch: "sprintd/demo/L1"},
		{Time: at("2026-08-12T11:00:12Z"), Sprint: "demo", Lane: "L2", State: results.StateDispatched, Attempt: 1, Machine: "dgx"},
		{Time: at("2026-08-12T11:20:00Z"), Sprint: "demo", Lane: "L2", State: results.StateComplete, Attempt: 1, Machine: "dgx"},
	})

	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if got.Totals.Escalated != 1 || got.Totals.Converged != 1 {
		t.Errorf("Totals = %+v, want one escalated and one converged", got.Totals)
	}
	var l1 status.Lane
	for _, lane := range got.Lanes {
		if lane.ID == "L1" {
			l1 = lane
		}
	}
	if l1.State != "escalated" || !l1.Terminal {
		t.Fatalf("L1 = %+v, want a terminal escalated lane", l1)
	}
	if l1.Attempt != 3 {
		t.Errorf("L1 Attempt = %d, want 3", l1.Attempt)
	}
	// The failure has to be readable from the payload; that is the point of
	// preserving it.
	if !strings.Contains(l1.PredicateOutput, "still two prompts") {
		t.Errorf("L1 PredicateOutput = %q, want the captured predicate failure", l1.PredicateOutput)
	}
	if l1.Branch != "sprintd/demo/L1" || l1.Worktree != "/srv/wt/L1" {
		t.Errorf("L1 = %+v, want its worktree and branch so the work can be found", l1)
	}
	if l1.DispatchedAt == nil || l1.ResolvedAt == nil {
		t.Errorf("L1 = %+v, want both dispatched and resolved timestamps", l1)
	}
}

// TestEventsWithoutManifest covers a run directory written before manifests
// existed: report what is known and say what is not.
func TestEventsWithoutManifest(t *testing.T) {
	t.Parallel()

	dir := writeRun(t, nil, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "legacy", Lane: "L1", State: results.StateDispatched, Attempt: 1, Machine: "mini"},
		{Time: at("2026-08-12T11:10:00Z"), Sprint: "legacy", Lane: "L1", State: results.StateComplete, Attempt: 1, Machine: "mini"},
	})

	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if got.Status != status.StatusFinished {
		t.Errorf("Status = %q, want finished: every lane is terminal", got.Status)
	}
	if got.Sprint == nil || got.Sprint.Name != "legacy" {
		t.Errorf("Sprint = %+v, want the name recovered from the events", got.Sprint)
	}
	if len(got.Notes) == 0 {
		t.Error("no note explained that the verdict was inferred without a manifest")
	}
}

func TestUnknownWithoutManifestAndUnresolvedLanes(t *testing.T) {
	t.Parallel()

	dir := writeRun(t, nil, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "legacy", Lane: "L1", State: results.StateDispatched, Attempt: 1, Machine: "mini"},
	})
	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if got.Status != status.StatusUnknown {
		t.Errorf("Status = %q, want unknown rather than a guess", got.Status)
	}
}

// TestDeclaredButNeverDispatchedLane keeps a lane that never started from
// vanishing from the payload.
func TestDeclaredButNeverDispatchedLane(t *testing.T) {
	t.Parallel()

	m := baseManifest()
	completed := at("2026-08-12T11:30:00Z")
	m.CompletedAt = &completed
	dir := writeRun(t, &m, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "demo", Lane: "L1", State: results.StateDispatched, Attempt: 1, Machine: "mini"},
		{Time: at("2026-08-12T11:10:00Z"), Sprint: "demo", Lane: "L1", State: results.StateComplete, Attempt: 1, Machine: "mini"},
	})

	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if len(got.Lanes) != 2 {
		t.Fatalf("lanes = %d, want both declared lanes", len(got.Lanes))
	}
	var l2 status.Lane
	for _, lane := range got.Lanes {
		if lane.ID == "L2" {
			l2 = lane
		}
	}
	if l2.State != "pending" || l2.DispatchedAt != nil {
		t.Errorf("L2 = %+v, want a pending lane that was never dispatched", l2)
	}
	if got.Totals.Dispatched != 1 {
		t.Errorf("Dispatched = %d, want 1", got.Totals.Dispatched)
	}
}

func TestAccountsCarrySampledOnce(t *testing.T) {
	t.Parallel()

	m := baseManifest()
	completed := at("2026-08-12T11:30:00Z")
	m.CompletedAt = &completed
	dir := writeRun(t, &m, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "demo", Lane: "L1", State: results.StateComplete, Attempt: 1, Machine: "mini"},
	})

	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(got.Accounts))
	}
	acct := got.Accounts[0]
	if !acct.SampledOnce || acct.SampledAt == nil {
		t.Errorf("account = %+v, want it marked sampled-once with a timestamp", acct)
	}
	if acct.WeeklyPct == nil || *acct.WeeklyPct != 42.5 {
		t.Errorf("WeeklyPct = %v, want 42.5", acct.WeeklyPct)
	}
	var sawNote bool
	for _, note := range got.Notes {
		if strings.Contains(note, "sampled once") {
			sawNote = true
		}
	}
	if !sawNote {
		t.Error("no note warned that account usage is not live")
	}
}

func TestFindLatestRun(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if _, ok, err := status.FindLatestRun(filepath.Join(root, "absent")); err != nil || ok {
		t.Errorf("FindLatestRun(absent) = (%v, %v), want (false, nil)", ok, err)
	}

	older := filepath.Join(root, "demo-20260812T100000Z")
	newer := filepath.Join(root, "demo-20260812T110000Z")
	for _, dir := range []string{older, newer} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, results.FileName), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(older, results.FileName), old, old); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	got, ok, err := status.FindLatestRun(root)
	if err != nil || !ok {
		t.Fatalf("FindLatestRun() = (%q, %v, %v), want the newer run", got, ok, err)
	}
	if got != newer {
		t.Errorf("FindLatestRun() = %q, want %q", got, newer)
	}
}

// TestBuildIsReadOnly pins that reporting never mutates the run it reports on.
func TestBuildIsReadOnly(t *testing.T) {
	t.Parallel()

	m := baseManifest()
	completed := at("2026-08-12T11:30:00Z")
	m.CompletedAt = &completed
	dir := writeRun(t, &m, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "demo", Lane: "L1", State: results.StateComplete, Attempt: 1, Machine: "mini"},
	})

	before := snapshotDir(t, dir)
	if _, err := status.Build(dir, now); err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if after := snapshotDir(t, dir); !equalSnapshots(before, after) {
		t.Errorf("Build() changed the run directory:\nbefore %v\nafter  %v", before, after)
	}
}

type fileState struct {
	size    int64
	modTime time.Time
}

func snapshotDir(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		out[entry.Name()] = fileState{size: info.Size(), modTime: info.ModTime()}
	}
	return out
}

func equalSnapshots(a, b map[string]fileState) bool {
	if len(a) != len(b) {
		return false
	}
	for name, want := range a {
		got, ok := b[name]
		if !ok || got.size != want.size || !got.modTime.Equal(want.modTime) {
			return false
		}
	}
	return true
}

// TestTruncatedEventLog covers a run killed exactly mid-write to
// results.jsonl: the founder-facing requirement is that this reports what is
// known rather than failing to parse.
func TestTruncatedEventLog(t *testing.T) {
	t.Parallel()

	m := baseManifest()
	m.PID = 0 // unreachable pid: the process is gone, same as a real kill
	dir := writeRun(t, &m, nil)

	raw := `{"time":"2026-08-12T11:00:10Z","sprint":"demo","lane":"L1","state":"dispatched","attempt":1,"machine":"mini"}
{"time":"2026-08-12T11:10:00Z","sprint":"demo","lane":"L1","state":"complete","attempt":1,"machine":"mini"}
{"time":"2026-08-12T11:00:12Z","sprint":"demo","lane":"L2","state":"disp`
	if err := os.WriteFile(filepath.Join(dir, results.FileName), []byte(raw), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil: a truncated event log must still report", err)
	}
	if got.Status == status.StatusRunning {
		t.Fatalf("Status = %q, want anything but running: the process is gone", got.Status)
	}
	var l1, l2 status.Lane
	for _, lane := range got.Lanes {
		switch lane.ID {
		case "L1":
			l1 = lane
		case "L2":
			l2 = lane
		}
	}
	if l1.State != "complete" {
		t.Errorf("L1 state = %q, want complete: both its records were intact", l1.State)
	}
	// L2's only record was the truncated one, dropped as incomplete; it must
	// fall back to declared-but-unresolved, not a phantom dispatched state.
	if l2.State != "pending" || l2.DispatchedAt != nil {
		t.Errorf("L2 = %+v, want pending: its only event was the truncated line", l2)
	}
	var sawNote bool
	for _, note := range got.Notes {
		if strings.Contains(note, "incomplete") {
			sawNote = true
		}
	}
	if !sawNote {
		t.Error("no note explained that the log's last line was dropped as incomplete")
	}
}

// TestEscalatedBeforeDispatchIsNotCountedAsDispatched covers the real case
// where a lane reaches a terminal state without ever running: its dependency
// escalated, or no account was eligible. It must not inflate the dispatched
// count, which is what the founder reads as work actually attempted.
func TestEscalatedBeforeDispatchIsNotCountedAsDispatched(t *testing.T) {
	t.Parallel()

	m := baseManifest()
	completed := at("2026-08-12T11:30:00Z")
	m.CompletedAt = &completed
	dir := writeRun(t, &m, []results.Record{
		{Time: at("2026-08-12T11:00:10Z"), Sprint: "demo", Lane: "L1", State: results.StateDispatched, Attempt: 1, Machine: "mini"},
		{Time: at("2026-08-12T11:10:00Z"), Sprint: "demo", Lane: "L1", State: results.StateEscalated, Attempt: 1, Machine: "mini"},
		{Time: at("2026-08-12T11:10:01Z"), Sprint: "demo", Lane: "L2", State: results.StateEscalated, Attempt: 0, Machine: "dgx",
			Reason: "dependency L1 escalated, so this lane can never be verified"},
	})

	got, err := status.Build(dir, now)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if got.Totals.Escalated != 2 {
		t.Errorf("Escalated = %d, want 2", got.Totals.Escalated)
	}
	if got.Totals.Dispatched != 1 {
		t.Errorf("Dispatched = %d, want 1: L2 never ran", got.Totals.Dispatched)
	}
	var l2 status.Lane
	for _, lane := range got.Lanes {
		if lane.ID == "L2" {
			l2 = lane
		}
	}
	if l2.DispatchedAt != nil {
		t.Errorf("L2 DispatchedAt = %v, want nil", l2.DispatchedAt)
	}
	if !strings.Contains(l2.Reason, "dependency L1 escalated") {
		t.Errorf("L2 Reason = %q, want it to explain why it never ran", l2.Reason)
	}
}
