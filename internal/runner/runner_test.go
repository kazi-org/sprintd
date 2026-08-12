package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kazi-org/sprintd/internal/allocator"
	"github.com/kazi-org/sprintd/internal/gitrepo"
	"github.com/kazi-org/sprintd/internal/machine"
	"github.com/kazi-org/sprintd/internal/results"
	"github.com/kazi-org/sprintd/internal/sprint"
)

// call is one command the runner asked the executor to run.
type call struct {
	Lane      string
	Script    string
	Dir       string
	Env       map[string]string
	Predicate bool
	Setup     bool
}

// fakeExec records every command and answers according to handler.
type fakeExec struct {
	mu      sync.Mutex
	calls   []call
	handler func(ctx context.Context, c call, attempt int, out io.Writer) (machine.Result, error)
	// dispatches counts prior dispatches of each lane, so a handler can make
	// attempt N behave differently from attempt N+1.
	dispatches map[string]int
	// failWorktree, when set, makes worktree setup fail with this message.
	failWorktree string
}

func newFakeExec(h func(ctx context.Context, c call, attempt int, out io.Writer) (machine.Result, error)) *fakeExec {
	return &fakeExec{handler: h, dispatches: map[string]int{}}
}

func (f *fakeExec) Run(ctx context.Context, cmd machine.Command, out io.Writer) (machine.Result, error) {
	// Lane setup is answered here rather than by the handler: every lane needs
	// a worktree before it can be dispatched, and no test is about that.
	if strings.Contains(cmd.Script, "git worktree add") {
		f.mu.Lock()
		f.calls = append(f.calls, call{Script: cmd.Script, Setup: true, Dir: cmd.Dir})
		fail := f.failWorktree
		f.mu.Unlock()
		if fail != "" {
			fmt.Fprintln(out, fail)
			return machine.Result{ExitCode: 128}, nil
		}
		fmt.Fprintln(out, "worktree=created")
		return machine.Result{ExitCode: 0}, nil
	}

	// Classify on the fixture's predicate shape rather than on the dispatch
	// shape: a lane that supplies its own command does not start with
	// "claude -p", and keying off that would file it as a predicate.
	isPredicate := strings.HasPrefix(cmd.Script, "verify ")
	c := call{Script: cmd.Script, Env: cmd.Env, Predicate: isPredicate, Dir: cmd.Dir}
	c.Lane = laneFromScript(cmd.Script)

	f.mu.Lock()
	f.calls = append(f.calls, c)
	attempt := 0
	if !isPredicate {
		f.dispatches[c.Lane]++
		attempt = f.dispatches[c.Lane]
	} else {
		attempt = f.dispatches[c.Lane]
	}
	f.mu.Unlock()

	return f.handler(ctx, c, attempt, out)
}

func (f *fakeExec) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// laneFromScript recovers the lane id, which every fixture embeds in both its
// prompt and its predicate as "lane=<id>".
func laneFromScript(script string) string {
	i := strings.Index(script, "lane=")
	if i < 0 {
		return ""
	}
	rest := script[i+len("lane="):]
	for j := 0; j < len(rest); j++ {
		if !isIDByte(rest[j]) {
			return rest[:j]
		}
	}
	return rest
}

func isIDByte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

// memRecorder collects records in memory.
type memRecorder struct {
	mu    sync.Mutex
	lines []byte
}

func (m *memRecorder) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lines = append(m.lines, p...)
	return len(p), nil
}

func (m *memRecorder) Close() error { return nil }

func (m *memRecorder) records(t *testing.T) []results.Record {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	recs, err := results.Read(strings.NewReader(string(m.lines)))
	if err != nil {
		t.Fatalf("reading recorded results: %v", err)
	}
	return recs
}

func states(recs []results.Record, lane string) []results.State {
	var out []results.State
	for _, r := range recs {
		if r.Lane == lane {
			out = append(out, r.State)
		}
	}
	return out
}

// fixture builds a runnable sprint with the given lanes.
func fixture(t *testing.T, laneYAML string) *sprint.Sprint {
	t.Helper()
	src := `
sprint: test
defaults:
  model: sonnet
  deadline: 5s
  retries: 1
machines:
  mini: { host: local }
accounts:
  - { name: primary, reserve_floor_pct: 0, weekly_token_limit: 1000, config_dir: /tmp/primary }
repos:
  app: { path: /srv/app, max_concurrent: 2 }
lanes:
` + laneYAML
	s, err := sprint.Parse([]byte(src))
	if err != nil {
		t.Fatalf("building fixture sprint: %v", err)
	}
	return s
}

// commandLane is a lane that supplies its own command instead of a prompt.
func commandLane(id, command string) string {
	return fmt.Sprintf(`  - id: %s
    repo: app
    goal: "goal lane=%s"
    command: "%s"
    predicate: "verify lane=%s"
    machine: mini
`, id, id, command, id)
}

func lane(id string, extra ...string) string {
	out := fmt.Sprintf(`  - id: %s
    repo: app
    goal: "goal lane=%s"
    prompt: "work lane=%s"
    predicate: "verify lane=%s"
    machine: mini
`, id, id, id, id)
	for _, e := range extra {
		out += "    " + e + "\n"
	}
	return out
}

type harness struct {
	runner   *Runner
	exec     *fakeExec
	recorder *memRecorder
	sprint   *sprint.Sprint
}

func newHarness(t *testing.T, s *sprint.Sprint, exec *fakeExec, tune func(*Config)) *harness {
	t.Helper()
	rec := &memRecorder{}
	cfg := Config{
		Sprint:           s,
		RunDir:           t.TempDir(),
		Exec:             exec,
		Allocator:        testAllocator(s),
		Recorder:         results.NewRecorderTo(rec),
		Stall:            50 * time.Millisecond,
		PollInterval:     5 * time.Millisecond,
		PredicateTimeout: 2 * time.Second,
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tune != nil {
		tune(&cfg)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return &harness{runner: r, exec: exec, recorder: rec, sprint: s}
}

type staticUsage struct{}

func (staticUsage) Read(context.Context, sprint.Account) (allocator.Usage, error) {
	return allocator.Usage{WeeklyTokens: 10}, nil
}

func testAllocator(s *sprint.Sprint) *allocator.Allocator {
	return allocator.New(context.Background(), s.Accounts, staticUsage{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// succeed is the handler for a lane that works and verifies first time.
func succeed(_ context.Context, _ call, _ int, out io.Writer) (machine.Result, error) {
	fmt.Fprintln(out, "done")
	return machine.Result{ExitCode: 0}, nil
}

func TestLaneCompletesOnlyWhenPredicatePasses(t *testing.T) {
	t.Parallel()

	h := newHarness(t, fixture(t, lane("L1")), newFakeExec(succeed), nil)
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Complete) != 1 || sum.Complete[0] != "L1" {
		t.Fatalf("Complete = %v, want [L1]", sum.Complete)
	}
	want := []results.State{results.StateDispatched, results.StateComplete}
	if got := states(h.recorder.records(t), "L1"); !equalStates(got, want) {
		t.Errorf("L1 transitions = %v, want %v", got, want)
	}
	// The predicate must have been a separate command, not the lane's own run.
	var sawPredicate bool
	for _, c := range h.exec.snapshot() {
		if c.Predicate && !c.Setup && c.Script == "verify lane=L1" {
			sawPredicate = true
		}
	}
	if !sawPredicate {
		t.Error("predicate was never run as its own command")
	}
}

// TestCleanExitWithFailingPredicateIsNotComplete is the defect the tool exists
// to prevent: the lane's own process reports success, and the lane is still
// not complete because nothing independent observed the goal.
func TestCleanExitWithFailingPredicateIsNotComplete(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, _ int, out io.Writer) (machine.Result, error) {
		if c.Predicate {
			fmt.Fprintln(out, "double Face ID prompt still present on cold launch")
			return machine.Result{ExitCode: 1}, nil
		}
		fmt.Fprintln(out, "I have fixed the double prompt. All done!")
		return machine.Result{ExitCode: 0}, nil
	})

	h := newHarness(t, fixture(t, lane("L1", "retries: 0")), exec, nil)
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Complete) != 0 {
		t.Errorf("Complete = %v, want none: the process exited 0 but the predicate failed", sum.Complete)
	}
	if len(sum.Escalated) != 1 || sum.Escalated[0] != "L1" {
		t.Fatalf("Escalated = %v, want [L1]", sum.Escalated)
	}
	recs := h.recorder.records(t)
	want := []results.State{results.StateDispatched, results.StatePredicateFailed, results.StateEscalated}
	if got := states(recs, "L1"); !equalStates(got, want) {
		t.Errorf("L1 transitions = %v, want %v", got, want)
	}
	if !strings.Contains(terminalRecord(t, recs, "L1").PredicateOutput, "still present") {
		t.Error("escalated record did not preserve the predicate's output")
	}
}

// TestRequeueCarriesFailureContext pins the retry path: the second attempt's
// prompt must contain what the predicate said, not just the original ask.
func TestRequeueCarriesFailureContext(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, attempt int, out io.Writer) (machine.Result, error) {
		if c.Predicate {
			if attempt == 1 {
				fmt.Fprintln(out, "FAIL: expected one prompt, saw two")
				return machine.Result{ExitCode: 1}, nil
			}
			fmt.Fprintln(out, "OK")
			return machine.Result{ExitCode: 0}, nil
		}
		fmt.Fprintln(out, "working")
		return machine.Result{ExitCode: 0}, nil
	})

	h := newHarness(t, fixture(t, lane("L1")), exec, nil)
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Complete) != 1 {
		t.Fatalf("Complete = %v, want [L1]", sum.Complete)
	}

	want := []results.State{
		results.StateDispatched, results.StatePredicateFailed,
		results.StateRequeued, results.StateDispatched, results.StateComplete,
	}
	if got := states(h.recorder.records(t), "L1"); !equalStates(got, want) {
		t.Errorf("L1 transitions = %v, want %v", got, want)
	}

	var dispatches []string
	for _, c := range h.exec.snapshot() {
		if !c.Predicate && !c.Setup {
			dispatches = append(dispatches, c.Script)
		}
	}
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count = %d, want 2", len(dispatches))
	}
	if strings.Contains(dispatches[0], "expected one prompt") {
		t.Error("first dispatch already contained failure context, which cannot exist yet")
	}
	if !strings.Contains(dispatches[1], "FAIL: expected one prompt, saw two") {
		t.Errorf("retry prompt did not carry the predicate output; got:\n%s", dispatches[1])
	}
	if !strings.Contains(dispatches[1], "work lane=L1") {
		t.Error("retry prompt dropped the original prompt")
	}
}

func TestStalledLaneIsKilledAndRequeued(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	var once sync.Once
	exec := newFakeExec(func(ctx context.Context, c call, attempt int, out io.Writer) (machine.Result, error) {
		if c.Predicate {
			fmt.Fprintln(out, "OK")
			return machine.Result{ExitCode: 0}, nil
		}
		if attempt == 1 {
			// Produce nothing at all and wait to be killed by the watchdog.
			<-ctx.Done()
			once.Do(func() { close(released) })
			return machine.Result{ExitCode: -1, Killed: true}, nil
		}
		fmt.Fprintln(out, "working")
		return machine.Result{ExitCode: 0}, nil
	})

	// A deadline far longer than the stall window, so a stall is the only
	// thing that can kill attempt one.
	h := newHarness(t, fixture(t, lane("L1", "deadline: 30s")), exec, nil)
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	select {
	case <-released:
	default:
		t.Fatal("the stalled attempt was never cancelled")
	}
	if len(sum.Complete) != 1 {
		t.Fatalf("Complete = %v, want [L1]", sum.Complete)
	}
	want := []results.State{
		results.StateDispatched, results.StateStalled, results.StateKilled,
		results.StateRequeued, results.StateDispatched, results.StateComplete,
	}
	if got := states(h.recorder.records(t), "L1"); !equalStates(got, want) {
		t.Errorf("L1 transitions = %v, want %v", got, want)
	}
	for _, c := range h.exec.snapshot() {
		if !c.Predicate && !c.Setup && strings.Contains(c.Script, "no output for") {
			return
		}
	}
	t.Error("retry prompt did not mention the stall")
}

func TestDeadlineExceededIsKilledAndRequeued(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(ctx context.Context, c call, attempt int, out io.Writer) (machine.Result, error) {
		if c.Predicate {
			fmt.Fprintln(out, "OK")
			return machine.Result{ExitCode: 0}, nil
		}
		if attempt == 1 {
			// Keep producing output so the stall watchdog stays quiet, and let
			// the deadline be what ends it.
			for {
				select {
				case <-ctx.Done():
					return machine.Result{ExitCode: -1, Killed: true}, nil
				case <-time.After(2 * time.Millisecond):
					fmt.Fprintln(out, "still going")
				}
			}
		}
		fmt.Fprintln(out, "working")
		return machine.Result{ExitCode: 0}, nil
	})

	h := newHarness(t, fixture(t, lane("L1", "deadline: 60ms")), exec, func(c *Config) {
		c.Stall = time.Hour
	})
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Complete) != 1 {
		t.Fatalf("Complete = %v, want [L1]", sum.Complete)
	}
	want := []results.State{
		results.StateDispatched, results.StateKilled,
		results.StateRequeued, results.StateDispatched, results.StateComplete,
	}
	if got := states(h.recorder.records(t), "L1"); !equalStates(got, want) {
		t.Errorf("L1 transitions = %v, want %v", got, want)
	}
}

func TestRetriesExhaustedEscalates(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, _ int, out io.Writer) (machine.Result, error) {
		if c.Predicate {
			fmt.Fprintln(out, "never satisfied")
			return machine.Result{ExitCode: 1}, nil
		}
		fmt.Fprintln(out, "trying")
		return machine.Result{ExitCode: 0}, nil
	})

	h := newHarness(t, fixture(t, lane("L1", "retries: 2")), exec, nil)
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Escalated) != 1 || sum.Escalated[0] != "L1" {
		t.Fatalf("Escalated = %v, want [L1]", sum.Escalated)
	}
	recs := h.recorder.records(t)
	dispatched := 0
	for _, r := range recs {
		if r.Lane == "L1" && r.State == results.StateDispatched {
			dispatched++
		}
	}
	if dispatched != 3 {
		t.Errorf("dispatch records = %d, want 3 (retries 2 + 1)", dispatched)
	}
	last := terminalRecord(t, recs, "L1")
	if last.State != results.StateEscalated {
		t.Errorf("terminal state = %s, want escalated", last.State)
	}
	if !strings.Contains(last.PredicateOutput, "never satisfied") {
		t.Error("escalated record did not preserve the last failure output")
	}
}

func TestNonZeroExitIsNeverVerified(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, _ int, out io.Writer) (machine.Result, error) {
		if c.Predicate {
			t.Error("predicate ran even though the lane exited non-zero")
			return machine.Result{ExitCode: 0}, nil
		}
		fmt.Fprintln(out, "boom")
		return machine.Result{ExitCode: 3}, nil
	})

	h := newHarness(t, fixture(t, lane("L1", "retries: 0")), exec, nil)
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Escalated) != 1 {
		t.Fatalf("Escalated = %v, want [L1]", sum.Escalated)
	}
	if reason := terminalRecord(t, h.recorder.records(t), "L1").Reason; !strings.Contains(reason, "exited 3") {
		t.Errorf("escalation reason = %q, want it to name exit 3", reason)
	}
}

func TestNeedsOrderingIsRespected(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var order []string
	exec := newFakeExec(func(_ context.Context, c call, _ int, out io.Writer) (machine.Result, error) {
		if !c.Predicate {
			mu.Lock()
			order = append(order, c.Lane)
			mu.Unlock()
		}
		fmt.Fprintln(out, "ok")
		return machine.Result{ExitCode: 0}, nil
	})

	s := fixture(t, lane("L1")+lane("L2", "needs: [L1]"))
	h := newHarness(t, s, exec, nil)
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "L1" || order[1] != "L2" {
		t.Errorf("dispatch order = %v, want [L1 L2]", order)
	}
}

func TestDependentOfEscalatedLaneEscalates(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, _ int, out io.Writer) (machine.Result, error) {
		if c.Lane == "L2" && !c.Predicate {
			t.Error("L2 was dispatched even though its dependency escalated")
		}
		if c.Predicate {
			fmt.Fprintln(out, "no")
			return machine.Result{ExitCode: 1}, nil
		}
		return machine.Result{ExitCode: 0}, nil
	})

	s := fixture(t, lane("L1", "retries: 0")+lane("L2", "needs: [L1]"))
	h := newHarness(t, s, exec, nil)
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Escalated) != 2 {
		t.Fatalf("Escalated = %v, want both lanes", sum.Escalated)
	}
	if reason := terminalRecord(t, h.recorder.records(t), "L2").Reason; !strings.Contains(reason, "dependency L1 escalated") {
		t.Errorf("L2 escalation reason = %q, want it to name the dependency", reason)
	}
}

func TestPerRepoConcurrencyIsCapped(t *testing.T) {
	t.Parallel()

	// Lanes announce that they have started and then block, so the number in
	// flight at one instant is directly observable rather than sampled.
	started := make(chan string, 8)
	release := make(chan struct{})
	exec := newFakeExec(func(_ context.Context, c call, _ int, out io.Writer) (machine.Result, error) {
		if !c.Predicate {
			started <- c.Lane
			<-release
		}
		fmt.Fprintln(out, "ok")
		return machine.Result{ExitCode: 0}, nil
	})

	// Four lanes in a repo capped at two.
	s := fixture(t, lane("L1")+lane("L2")+lane("L3")+lane("L4"))
	h := newHarness(t, s, exec, nil)

	type runResult struct {
		summary Summary
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		sum, err := h.runner.Run(context.Background())
		done <- runResult{sum, err}
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatalf("only %d lanes started, want the cap of 2 to actually be used", i)
		}
	}
	select {
	case extra := <-started:
		close(release)
		t.Fatalf("lane %s started while 2 were already in flight in a repo capped at 2", extra)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)

	got := <-done
	if got.err != nil {
		t.Fatalf("Run() error = %v, want nil", got.err)
	}
	if len(got.summary.Complete) != 4 {
		t.Fatalf("Complete = %v, want all four lanes", got.summary.Complete)
	}
}

func TestAccountIsPinnedViaConfigDir(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(succeed)
	h := newHarness(t, fixture(t, lane("L1")), exec, nil)
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	for _, c := range exec.snapshot() {
		if c.Setup {
			continue
		}
		if c.Predicate {
			if _, ok := c.Env["CLAUDE_CONFIG_DIR"]; ok {
				t.Error("predicate was given account credentials; it must not consume the quota it protects")
			}
			continue
		}
		if got := c.Env["CLAUDE_CONFIG_DIR"]; got != "/tmp/primary" {
			t.Errorf("dispatch CLAUDE_CONFIG_DIR = %q, want /tmp/primary", got)
		}
	}
}

func TestNoEligibleAccountEscalatesWithoutDispatching(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, _ int, _ io.Writer) (machine.Result, error) {
		t.Error("a lane was dispatched with no eligible account")
		return machine.Result{ExitCode: 0}, nil
	})
	s := fixture(t, lane("L1"))
	h := newHarness(t, s, exec, func(c *Config) {
		c.Allocator = allocator.New(context.Background(), s.Accounts,
			exhaustedUsage{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Escalated) != 1 {
		t.Fatalf("Escalated = %v, want [L1]", sum.Escalated)
	}
	if reason := terminalRecord(t, h.recorder.records(t), "L1").Reason; !strings.Contains(reason, "no account available") {
		t.Errorf("escalation reason = %q, want it to name the account exhaustion", reason)
	}
}

type exhaustedUsage struct{}

func (exhaustedUsage) Read(context.Context, sprint.Account) (allocator.Usage, error) {
	return allocator.Usage{WeeklyTokens: 999}, nil
}

func TestHeartbeatFileIsWritten(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(ctx context.Context, c call, _ int, out io.Writer) (machine.Result, error) {
		if c.Predicate {
			fmt.Fprintln(out, "OK")
			return machine.Result{ExitCode: 0}, nil
		}
		fmt.Fprintln(out, "start")
		// Stay alive long enough for at least one watchdog tick.
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Millisecond):
		}
		return machine.Result{ExitCode: 0}, nil
	})

	dir := t.TempDir()
	h := newHarness(t, fixture(t, lane("L1", "deadline: 10s")), exec, func(c *Config) {
		c.RunDir = dir
		c.Stall = time.Hour
	})
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if _, err := readFile(dir + "/heartbeats/L1.json"); err != nil {
		t.Errorf("heartbeat file not written: %v", err)
	}
	if _, err := readFile(dir + "/logs/L1.attempt-1.log"); err != nil {
		t.Errorf("lane log not written: %v", err)
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	t.Parallel()

	s := fixture(t, lane("L1"))
	base := func() Config {
		return Config{
			Sprint: s, RunDir: t.TempDir(), Exec: newFakeExec(succeed),
			Allocator: testAllocator(s), Recorder: results.NewRecorderTo(&memRecorder{}),
		}
	}
	tests := map[string]func(*Config){
		"missing sprint":    func(c *Config) { c.Sprint = nil },
		"missing executor":  func(c *Config) { c.Exec = nil },
		"missing recorder":  func(c *Config) { c.Recorder = nil },
		"missing allocator": func(c *Config) { c.Allocator = nil },
		"missing run dir":   func(c *Config) { c.RunDir = "" },
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := base()
			break_(&cfg)
			if _, err := New(cfg); err == nil {
				t.Error("New() error = nil, want an error")
			}
		})
	}
}

func terminalRecord(t *testing.T, recs []results.Record, lane string) results.Record {
	t.Helper()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Lane == lane {
			return recs[i]
		}
	}
	t.Fatalf("no records for lane %s", lane)
	return results.Record{}
}

func equalStates(got, want []results.State) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestLaneRunsInItsOwnWorktree pins the property that makes a per-repo
// concurrency cap above 1 coherent at all, and that keeps the primary
// checkout's branch and staleness away from the lane.
func TestLaneRunsInItsOwnWorktree(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(succeed)
	s := fixture(t, lane("L1")+lane("L2"))
	h := newHarness(t, s, exec, nil)
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	dirs := map[string]map[string]bool{}
	for _, c := range exec.snapshot() {
		if c.Setup {
			if c.Dir != "/srv/app" {
				t.Errorf("worktree setup ran in %q, want the repo /srv/app", c.Dir)
			}
			continue
		}
		if dirs[c.Lane] == nil {
			dirs[c.Lane] = map[string]bool{}
		}
		dirs[c.Lane][c.Dir] = true
	}

	for _, id := range []string{"L1", "L2"} {
		want := gitrepo.WorktreePath("/srv/app", "test", id)
		got := dirs[id]
		if len(got) != 1 || !got[want] {
			t.Errorf("lane %s ran in %v, want only its own worktree %s", id, keys(got), want)
		}
	}
	if a, b := gitrepo.WorktreePath("/srv/app", "test", "L1"), gitrepo.WorktreePath("/srv/app", "test", "L2"); a == b {
		t.Fatal("two lanes resolved to the same worktree")
	}
}

// TestPredicateRunsInTheLanesWorktree guards the trap that a predicate must
// judge the tree the lane actually worked in.
func TestPredicateRunsInTheLanesWorktree(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(succeed)
	h := newHarness(t, fixture(t, lane("L1")), exec, nil)
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	want := gitrepo.WorktreePath("/srv/app", "test", "L1")
	var checked bool
	for _, c := range exec.snapshot() {
		if c.Setup || !c.Predicate {
			continue
		}
		checked = true
		if c.Dir != want {
			t.Errorf("predicate ran in %q, want the lane worktree %q", c.Dir, want)
		}
	}
	if !checked {
		t.Fatal("no predicate ran")
	}
}

// TestWorktreeIsPreparedOncePerLane keeps a retry in the tree the previous
// attempt left, rather than discarding its progress.
func TestWorktreeIsPreparedOncePerLane(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, attempt int, out io.Writer) (machine.Result, error) {
		if c.Predicate {
			if attempt == 1 {
				fmt.Fprintln(out, "not yet")
				return machine.Result{ExitCode: 1}, nil
			}
			fmt.Fprintln(out, "OK")
			return machine.Result{ExitCode: 0}, nil
		}
		fmt.Fprintln(out, "working")
		return machine.Result{ExitCode: 0}, nil
	})
	h := newHarness(t, fixture(t, lane("L1")), exec, nil)
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	setups, dispatches := 0, 0
	for _, c := range exec.snapshot() {
		switch {
		case c.Setup:
			setups++
		case !c.Predicate:
			dispatches++
		}
	}
	if dispatches != 2 {
		t.Fatalf("dispatches = %d, want 2", dispatches)
	}
	if setups != 1 {
		t.Errorf("worktree setups = %d, want 1: a retry must continue in the same tree", setups)
	}
}

// TestWorktreeFailureEscalatesWithoutDispatching keeps a lane from silently
// running in the primary checkout when its worktree could not be made.
func TestWorktreeFailureEscalatesWithoutDispatching(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, _ int, _ io.Writer) (machine.Result, error) {
		t.Errorf("lane work ran despite no worktree: %q", c.Script)
		return machine.Result{ExitCode: 0}, nil
	})
	exec.failWorktree = "fatal: invalid reference: origin/main"

	h := newHarness(t, fixture(t, lane("L1")), exec, nil)
	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Escalated) != 1 || sum.Escalated[0] != "L1" {
		t.Fatalf("Escalated = %v, want [L1]", sum.Escalated)
	}
	reason := terminalRecord(t, h.recorder.records(t), "L1").Reason
	if !strings.Contains(reason, "worktree") || !strings.Contains(reason, "origin/main") {
		t.Errorf("escalation reason = %q, want it to name the worktree and the base", reason)
	}
}

// TestConcurrentLanesGetDistinctWorktrees is the collision case: four lanes in
// one repo must not share a working tree.
func TestConcurrentLanesGetDistinctWorktrees(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(succeed)
	s := fixture(t, lane("L1")+lane("L2")+lane("L3")+lane("L4"))
	h := newHarness(t, s, exec, nil)
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	seen := map[string]string{}
	for _, c := range exec.snapshot() {
		if c.Setup || c.Predicate {
			continue
		}
		if prev, ok := seen[c.Dir]; ok && prev != c.Lane {
			t.Errorf("lanes %s and %s shared working tree %s", prev, c.Lane, c.Dir)
		}
		seen[c.Dir] = c.Lane
	}
	if len(seen) != 4 {
		t.Errorf("distinct working trees = %d, want 4", len(seen))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestComposedLaneScriptIsUnchanged is the regression guard for the command
// field: a lane that supplies a prompt must still produce byte-for-byte the
// claude invocation it produced before the field existed.
func TestComposedLaneScriptIsUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prompt string
		model  string
		want   string
	}{
		{
			name:   "plain prompt",
			prompt: "fix the thing",
			model:  "sonnet",
			want:   `claude -p 'fix the thing' --model 'sonnet'`,
		},
		{
			name:   "prompt containing a quote",
			prompt: "it's broken",
			model:  "opus",
			want:   `claude -p 'it'\''s broken' --model 'opus'`,
		},
		{
			name:   "prompt containing shell metacharacters stays inert",
			prompt: "$(rm -rf /) `whoami`",
			model:  "sonnet",
			want:   "claude -p '$(rm -rf /) `whoami`' --model 'sonnet'",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ln := sprint.ResolvedLane{Lane: sprint.Lane{Prompt: tc.prompt}}
			ln.Model = tc.model
			if got := laneScript(ln, tc.prompt); got != tc.want {
				t.Errorf("laneScript() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCommandLaneScriptIsVerbatim(t *testing.T) {
	t.Parallel()

	ln := sprint.ResolvedLane{Lane: sprint.Lane{Command: "kazi apply goals/faceid.toml --max-iterations 20"}}
	// The prompt argument is what a retry would have composed; a command lane
	// must ignore it entirely.
	if got, want := laneScript(ln, "some retry prompt"), "kazi apply goals/faceid.toml --max-iterations 20"; got != want {
		t.Errorf("laneScript() = %q, want the command verbatim %q", got, want)
	}
}

// TestCommandLaneDispatchesAndIsVerified runs the whole path for a lane that
// supplies its own command.
func TestCommandLaneDispatchesAndIsVerified(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, _ int, out io.Writer) (machine.Result, error) {
		fmt.Fprintln(out, "ok")
		return machine.Result{ExitCode: 0}, nil
	})
	s := fixture(t, commandLane("L1", "kazi apply goals/lane=L1.toml"))
	h := newHarness(t, s, exec, nil)

	sum, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(sum.Complete) != 1 || sum.Complete[0] != "L1" {
		t.Fatalf("Complete = %v, want [L1]", sum.Complete)
	}

	var dispatched, verified bool
	for _, c := range exec.snapshot() {
		switch {
		case c.Setup:
		case strings.HasPrefix(c.Script, "kazi apply"):
			dispatched = true
			if strings.Contains(c.Script, "claude") {
				t.Errorf("command lane script was wrapped: %q", c.Script)
			}
			// Account pinning must still apply: a command that dispatches
			// agents internally spends that account's quota.
			if got := c.Env["CLAUDE_CONFIG_DIR"]; got != "/tmp/primary" {
				t.Errorf("command lane CLAUDE_CONFIG_DIR = %q, want /tmp/primary", got)
			}
		case c.Predicate:
			verified = true
			if _, ok := c.Env["CLAUDE_CONFIG_DIR"]; ok {
				t.Error("predicate was given account credentials")
			}
		}
	}
	if !dispatched {
		t.Error("the lane's own command never ran")
	}
	if !verified {
		t.Error("the predicate never ran")
	}
}

// TestCommandLaneRetriesVerbatim pins the consequence of a verbatim command:
// there is nowhere to append the previous failure, so it is re-run unchanged
// and the failure lives in the results log instead.
func TestCommandLaneRetriesVerbatim(t *testing.T) {
	t.Parallel()

	exec := newFakeExec(func(_ context.Context, c call, attempt int, out io.Writer) (machine.Result, error) {
		if c.Predicate {
			if attempt == 1 {
				fmt.Fprintln(out, "FAIL: not converged yet")
				return machine.Result{ExitCode: 1}, nil
			}
			fmt.Fprintln(out, "OK")
			return machine.Result{ExitCode: 0}, nil
		}
		fmt.Fprintln(out, "grinding")
		return machine.Result{ExitCode: 0}, nil
	})
	s := fixture(t, commandLane("L1", "kazi apply goals/lane=L1.toml"))
	h := newHarness(t, s, exec, nil)
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	var dispatches []string
	for _, c := range exec.snapshot() {
		if !c.Setup && !c.Predicate {
			dispatches = append(dispatches, c.Script)
		}
	}
	if len(dispatches) != 2 {
		t.Fatalf("dispatches = %d, want 2", len(dispatches))
	}
	if dispatches[0] != dispatches[1] {
		t.Errorf("retry changed the command:\n first: %q\nsecond: %q", dispatches[0], dispatches[1])
	}
	// The failure is not lost, it just lives in the record rather than the
	// command.
	var sawPredicateFailure bool
	for _, rec := range h.recorder.records(t) {
		if rec.State == results.StatePredicateFailed && strings.Contains(rec.PredicateOutput, "not converged yet") {
			sawPredicateFailure = true
		}
	}
	if !sawPredicateFailure {
		t.Error("the predicate failure was not preserved in results.jsonl")
	}
}
