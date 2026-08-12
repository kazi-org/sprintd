// Package runner dispatches lanes, watchdogs them, and verifies each one with
// its acceptance predicate before calling it complete.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/kazi-org/sprintd/internal/allocator"
	"github.com/kazi-org/sprintd/internal/gitrepo"
	"github.com/kazi-org/sprintd/internal/machine"
	"github.com/kazi-org/sprintd/internal/results"
	"github.com/kazi-org/sprintd/internal/sprint"
	"github.com/kazi-org/sprintd/internal/status"
)

// Defaults for the watchdog and the predicate check.
const (
	DefaultStall            = 10 * time.Minute
	DefaultPollInterval     = 5 * time.Second
	DefaultPredicateTimeout = 10 * time.Minute
)

var (
	errStalled       = errors.New("stalled")
	errDeadline      = errors.New("deadline exceeded")
	errSprintClosed  = errors.New("sprint window closed")
	errRunCancelled  = errors.New("run cancelled")
	errNothingToRun  = errors.New("no lane could be started")
	errLaneUnreached = errors.New("dependency never completed")
)

// Executor runs a command on a machine, streaming its output.
type Executor interface {
	Run(ctx context.Context, cmd machine.Command, out io.Writer) (machine.Result, error)
}

// Config configures a Runner.
type Config struct {
	Sprint    *sprint.Sprint
	RunDir    string
	Exec      Executor
	Allocator *allocator.Allocator
	Recorder  *results.Recorder
	// Stall is how long a lane may produce no output before it is killed.
	Stall time.Duration
	// PollInterval is how often the watchdog samples lane activity.
	PollInterval time.Duration
	// PredicateTimeout bounds a single predicate run.
	PredicateTimeout time.Duration
	Log              *slog.Logger
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// StartedAt is when the run began, from the caller's point of view. It is
	// passed in because the run directory -- and its manifest -- exist before
	// the Runner does, and both writes must report the same start.
	StartedAt time.Time
}

// Runner executes one sprint.
type Runner struct {
	cfg Config
	// repoLocks serialises the repo-mutating half of lane setup. Concurrent
	// lanes in one repo would otherwise run git fetch and git worktree add at
	// the same time and collide on the repo's ref and index locks.
	repoLocks sync.Map
	// worktrees caches each lane's prepared tree so a retry continues in the
	// tree the previous attempt left rather than starting over.
	worktrees sync.Map
	// startedAt is fixed when the Runner is built so the manifest reports one
	// start time across both writes.
	startedAt time.Time
}

func (r *Runner) repoLock(repo string) *sync.Mutex {
	lock, _ := r.repoLocks.LoadOrStore(repo, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// Summary reports how a sprint ended.
type Summary struct {
	Complete  []string
	Escalated []string
}

// New validates the configuration and returns a Runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Sprint == nil {
		return nil, errors.New("building runner: sprint is nil")
	}
	if cfg.Exec == nil {
		return nil, errors.New("building runner: executor is nil")
	}
	if cfg.Recorder == nil {
		return nil, errors.New("building runner: recorder is nil")
	}
	if cfg.Allocator == nil {
		return nil, errors.New("building runner: allocator is nil")
	}
	if cfg.RunDir == "" {
		return nil, errors.New("building runner: run directory is empty")
	}
	if cfg.Stall <= 0 {
		cfg.Stall = DefaultStall
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.PredicateTimeout <= 0 {
		cfg.PredicateTimeout = DefaultPredicateTimeout
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	for _, sub := range []string{"logs", "heartbeats"} {
		if err := os.MkdirAll(filepath.Join(cfg.RunDir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("creating run directory %s: %w", sub, err)
		}
	}
	startedAt := cfg.StartedAt
	if startedAt.IsZero() {
		startedAt = cfg.Now().UTC()
	}
	return &Runner{cfg: cfg, startedAt: startedAt.UTC()}, nil
}

// writeManifest records what the event log cannot carry: the sprint window,
// the machines and accounts in play, and whether this run has ended.
//
// It is rewritten with completedAt set when the run finishes; a manifest with
// no completion plus a dead process is how an interrupted run is told from a
// live one.
func (r *Runner) writeManifest(completedAt *time.Time) error {
	s := r.cfg.Sprint
	m := status.Manifest{
		Sprint:    s.Name,
		StartedAt: r.startedAt,
		PID:       os.Getpid(),
	}
	if host, err := os.Hostname(); err == nil {
		m.Hostname = host
	}
	if !s.Opened.IsZero() {
		opened := s.Opened
		m.Opened = &opened
	}
	if !s.Closes.IsZero() {
		closes := s.Closes
		m.Closes = &closes
	}
	m.CompletedAt = completedAt

	for _, name := range sortedNames(s.Machines) {
		m.Machines = append(m.Machines, status.Machine{Name: name, Host: s.Machines[name].Host})
	}
	for _, ln := range s.ResolvedLanes() {
		m.Lanes = append(m.Lanes, status.LaneSpec{
			ID: ln.ID, Repo: ln.Repo, Machine: ln.Machine,
			Goal: ln.Goal, Model: ln.Model, Command: ln.Command,
		})
	}
	sampledAt := r.cfg.Allocator.SampledAt()
	for _, sample := range r.cfg.Allocator.Snapshot() {
		acct := status.AccountSample{
			Name:            sample.Name,
			ReserveFloorPct: sample.ReserveFloorPct,
			WeeklyTokens:    sample.WeeklyTokens,
			WeeklyLimit:     sample.WeeklyLimit,
			Metered:         sample.Metered,
			SampledOnce:     true,
		}
		if sample.Metered {
			pct := sample.WeeklyPct
			acct.WeeklyPct = &pct
			at := sampledAt
			acct.SampledAt = &at
		}
		m.Accounts = append(m.Accounts, acct)
	}
	return status.WriteManifest(r.cfg.RunDir, m)
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// laneState is a lane's position in the schedule.
type laneState int

const (
	lanePending laneState = iota
	laneRunning
	laneComplete
	laneEscalated
)

type laneOutcome struct {
	lane  sprint.ResolvedLane
	state laneState
}

// Run drives the sprint to completion and returns which lanes landed.
//
// The scheduler is deliberately plain: it launches every lane whose
// dependencies have completed and whose repo has a free slot, waits for one to
// finish, and repeats. Concurrency is bounded per repo because the scarce
// resource is mergeable changes in one checkout, not CPU.
func (r *Runner) Run(ctx context.Context) (Summary, error) {
	// The manifest is what lets anything outside this process answer "is a
	// sprint running", and it has to exist before the first lane does, so a
	// run killed seconds in is still reportable.
	if err := r.writeManifest(nil); err != nil {
		r.cfg.Log.Error("writing run manifest", "error", err)
	}
	defer func() {
		done := r.cfg.Now().UTC()
		if err := r.writeManifest(&done); err != nil {
			r.cfg.Log.Error("finalising run manifest", "error", err)
		}
	}()

	lanes := r.cfg.Sprint.ResolvedLanes()
	byID := make(map[string]sprint.ResolvedLane, len(lanes))
	state := make(map[string]laneState, len(lanes))
	for _, ln := range lanes {
		byID[ln.ID] = ln
		state[ln.ID] = lanePending
	}

	// The sprint's own close time bounds the whole run: a sprint file that
	// declares a window means it.
	if !r.cfg.Sprint.Closes.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadlineCause(ctx, r.cfg.Sprint.Closes, errSprintClosed)
		defer cancel()
	}

	repoInUse := map[string]int{}
	done := make(chan laneOutcome)
	inflight := 0

	for {
		launched := r.launchReady(ctx, lanes, state, repoInUse, done)
		inflight += launched

		if inflight == 0 {
			if r.allSettled(state) {
				break
			}
			// Nothing is running and nothing can start: whatever is left is
			// waiting on a dependency that never completed.
			r.escalateStuck(lanes, state)
			break
		}

		outcome := <-done
		inflight--
		state[outcome.lane.ID] = outcome.state
		repoInUse[outcome.lane.Repo]--
	}

	return r.summarise(lanes, state), nil
}

// launchReady starts every lane that is currently startable and returns how
// many it started.
func (r *Runner) launchReady(
	ctx context.Context,
	lanes []sprint.ResolvedLane,
	state map[string]laneState,
	repoInUse map[string]int,
	done chan<- laneOutcome,
) int {
	launched := 0
	for _, ln := range lanes {
		if state[ln.ID] != lanePending {
			continue
		}
		ready, blocked := dependencyStatus(ln, state)
		if blocked != "" {
			r.record(results.Record{
				Lane: ln.ID, State: results.StateEscalated, Attempt: 0, Repo: ln.Repo,
				Machine: ln.Machine,
				Reason:  fmt.Sprintf("dependency %s escalated, so this lane can never be verified", blocked),
			})
			state[ln.ID] = laneEscalated
			continue
		}
		if !ready {
			continue
		}
		if repoInUse[ln.Repo] >= ln.MaxConcurrent {
			continue
		}
		repoInUse[ln.Repo]++
		state[ln.ID] = laneRunning
		launched++
		go func(ln sprint.ResolvedLane) {
			done <- laneOutcome{lane: ln, state: r.runLane(ctx, ln)}
		}(ln)
	}
	return launched
}

// dependencyStatus reports whether a lane may start, and names a dependency
// that escalated if one did.
func dependencyStatus(ln sprint.ResolvedLane, state map[string]laneState) (ready bool, blockedBy string) {
	for _, need := range ln.Needs {
		switch state[need] {
		case laneEscalated:
			return false, need
		case laneComplete:
			continue
		default:
			return false, ""
		}
	}
	return true, ""
}

func (r *Runner) allSettled(state map[string]laneState) bool {
	for _, s := range state {
		if s == lanePending || s == laneRunning {
			return false
		}
	}
	return true
}

// escalateStuck marks lanes that can never start, which happens when a
// dependency ended in a state that is neither complete nor escalated.
func (r *Runner) escalateStuck(lanes []sprint.ResolvedLane, state map[string]laneState) {
	for _, ln := range lanes {
		if state[ln.ID] != lanePending {
			continue
		}
		r.record(results.Record{
			Lane: ln.ID, State: results.StateEscalated, Repo: ln.Repo, Machine: ln.Machine,
			Reason: fmt.Sprintf("%v: lane never became startable", errLaneUnreached),
		})
		state[ln.ID] = laneEscalated
	}
}

func (r *Runner) summarise(lanes []sprint.ResolvedLane, state map[string]laneState) Summary {
	var sum Summary
	for _, ln := range lanes {
		switch state[ln.ID] {
		case laneComplete:
			sum.Complete = append(sum.Complete, ln.ID)
		default:
			sum.Escalated = append(sum.Escalated, ln.ID)
		}
	}
	sort.Strings(sum.Complete)
	sort.Strings(sum.Escalated)
	return sum
}

// ensureWorktree gives the lane its own working tree, branched from a freshly
// fetched base, and returns its path.
//
// Every lane gets one. Without it a repo's max_concurrent above 1 is not
// merely risky but incoherent: concurrent lanes would share one working tree,
// one index and one checked-out branch, and silently overwrite each other. It
// also means the primary checkout's own branch and staleness cannot reach a
// lane, since the lane never runs in it.
func (r *Runner) ensureWorktree(ctx context.Context, ln sprint.ResolvedLane) (string, error) {
	if cached, ok := r.worktrees.Load(ln.ID); ok {
		return cached.(string), nil
	}
	worktree := gitrepo.WorktreePath(ln.RepoPath, r.cfg.Sprint.Name, ln.ID)
	branch := gitrepo.BranchName(r.cfg.Sprint.Name, ln.ID)

	lock := r.repoLock(ln.Repo)
	lock.Lock()
	defer lock.Unlock()
	if cached, ok := r.worktrees.Load(ln.ID); ok {
		return cached.(string), nil
	}

	created, err := gitrepo.EnsureWorktree(ctx, r.cfg.Exec, ln.Host, ln.RepoPath, worktree, branch, ln.Base)
	if err != nil {
		return "", err
	}
	r.cfg.Log.Info("lane worktree ready", "lane", ln.ID, "path", worktree,
		"branch", branch, "base", ln.Base, "created", created)
	r.worktrees.Store(ln.ID, worktree)
	return worktree, nil
}

// runLane dispatches one lane, retrying until it verifies or runs out of
// attempts.
func (r *Runner) runLane(ctx context.Context, ln sprint.ResolvedLane) laneState {
	prompt := ln.Prompt
	var last failure
	// prev is nil on the first attempt; it is what a command lane reads to
	// tell a retry from a fresh run.
	var prev *failure

	worktree, err := r.ensureWorktree(ctx, ln)
	if err != nil {
		r.record(results.Record{
			Lane: ln.ID, State: results.StateEscalated, Attempt: 0, Repo: ln.Repo,
			Machine: ln.Machine, Model: ln.Model,
			Reason: fmt.Sprintf("could not prepare a worktree from %s: %v", ln.Base, err),
		})
		return laneEscalated
	}

	for attempt := 1; attempt <= ln.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			r.record(results.Record{
				Lane: ln.ID, State: results.StateEscalated, Attempt: attempt, Repo: ln.Repo,
				Machine: ln.Machine, Model: ln.Model,
				Reason: fmt.Sprintf("%v: %v", errRunCancelled, context.Cause(ctx)),
			})
			return laneEscalated
		}

		account, err := r.cfg.Allocator.Assign()
		if err != nil {
			r.record(results.Record{
				Lane: ln.ID, State: results.StateEscalated, Attempt: attempt, Repo: ln.Repo,
				Machine: ln.Machine, Model: ln.Model,
				Reason: fmt.Sprintf("no account available: %v", err),
			})
			return laneEscalated
		}

		r.record(results.Record{
			Lane: ln.ID, State: results.StateDispatched, Attempt: attempt, Repo: ln.Repo,
			Machine: ln.Machine, Account: account, Model: ln.Model, Reason: ln.Goal,
			Worktree: worktree, Branch: gitrepo.BranchName(r.cfg.Sprint.Name, ln.ID),
		})

		outcome := r.attempt(ctx, ln, attempt, account, prompt, worktree, prev)
		if outcome.complete {
			r.record(results.Record{
				Lane: ln.ID, State: results.StateComplete, Attempt: attempt, Repo: ln.Repo,
				Machine: ln.Machine, Account: account, Model: ln.Model,
				ExitCode: outcome.exitCode, PredicateOutput: outcome.predicateOutput,
				DurationMS: outcome.duration.Milliseconds(),
				Reason:     "predicate passed",
				Worktree:   worktree, Branch: gitrepo.BranchName(r.cfg.Sprint.Name, ln.ID),
			})
			return laneComplete
		}

		last = outcome.failure
		failed := outcome.failure
		prev = &failed
		r.recordFailure(ln, attempt, account, worktree, outcome)

		if attempt == ln.Attempts {
			break
		}
		r.record(results.Record{
			Lane: ln.ID, State: results.StateRequeued, Attempt: attempt + 1, Repo: ln.Repo,
			Machine: ln.Machine, Account: account, Model: ln.Model,
			Reason: fmt.Sprintf("retrying after %s", last),
		})
		if ln.Composed() {
			// A verbatim command has nowhere to put the failure context, so it
			// is re-run unchanged; the failure is still in results.jsonl.
			prompt = retryPrompt(ln.Prompt, attempt, last)
		}
	}

	r.record(results.Record{
		Lane: ln.ID, State: results.StateEscalated, Attempt: ln.Attempts, Repo: ln.Repo,
		Machine: ln.Machine, Model: ln.Model,
		Reason:          fmt.Sprintf("%d attempts exhausted; last failure: %s", ln.Attempts, last),
		PredicateOutput: last.Output,
		// The tree is left in place: someone is going to want to read what
		// this lane actually did.
		Worktree: worktree, Branch: gitrepo.BranchName(r.cfg.Sprint.Name, ln.ID),
	})
	return laneEscalated
}

// recordFailure writes the transitions that describe one failed attempt.
func (r *Runner) recordFailure(ln sprint.ResolvedLane, attempt int, account, worktree string, o attemptOutcome) {
	base := results.Record{
		Lane: ln.ID, Attempt: attempt, Repo: ln.Repo, Machine: ln.Machine,
		Account: account, Model: ln.Model, ExitCode: o.exitCode,
		DurationMS: o.duration.Milliseconds(), Reason: o.failure.String(),
		Worktree: worktree, Branch: gitrepo.BranchName(r.cfg.Sprint.Name, ln.ID),
	}
	switch o.failure.Kind {
	case failStall:
		stalled := base
		stalled.State = results.StateStalled
		r.record(stalled)
		killed := base
		killed.State = results.StateKilled
		r.record(killed)
	case failDeadline:
		killed := base
		killed.State = results.StateKilled
		r.record(killed)
	case failPredicate:
		failed := base
		failed.State = results.StatePredicateFailed
		failed.PredicateOutput = o.failure.Output
		r.record(failed)
	default:
		// A launch error or a non-zero exit has no dedicated state; it is
		// visible as the reason on the requeue or escalation that follows.
	}
}

// laneScript is the command line for one attempt.
//
// A lane that declares a command runs it verbatim; sprintd supplies only what
// that command has no concept of -- which machine, which account, the deadline
// and the watchdog. Otherwise sprintd composes the claude invocation, which is
// the original prompt on the first attempt and the prompt plus the previous
// failure on a retry.
func laneScript(ln sprint.ResolvedLane, prompt string) string {
	if !ln.Composed() {
		return ln.Command
	}
	return fmt.Sprintf("claude -p %s --model %s", machine.Quote(prompt), machine.Quote(ln.Model))
}

// attemptOutcome is the result of a single dispatch plus its verification.
type attemptOutcome struct {
	complete        bool
	failure         failure
	exitCode        *int
	predicateOutput string
	duration        time.Duration
}

// attempt runs the lane once and, if the process exits cleanly, verifies it.
func (r *Runner) attempt(ctx context.Context, ln sprint.ResolvedLane, attempt int, account, prompt, worktree string, prev *failure) attemptOutcome {
	logPath := filepath.Join(r.cfg.RunDir, "logs", fmt.Sprintf("%s.attempt-%d.log", ln.ID, attempt))
	logFile, err := os.Create(logPath)
	if err != nil {
		return attemptOutcome{failure: failure{Kind: failLaunch, Detail: fmt.Sprintf("creating lane log: %v", err)}}
	}
	defer logFile.Close()

	env := map[string]string{EnvAttempt: strconv.Itoa(attempt)}
	if prev != nil {
		env[EnvLastFailure] = lastFailureEnv(*prev)
	}
	if acct, ok := r.cfg.Allocator.Account(account); ok && acct.ConfigDir != "" {
		// This is how Claude Code separates credentials, so it is how a lane
		// is pinned to one account.
		env["CLAUDE_CONFIG_DIR"] = acct.ConfigDir
	}

	cmd := machine.Command{
		Host: ln.Host,
		// The lane's own worktree, never the primary checkout.
		Dir:    worktree,
		Env:    env,
		Script: laneScript(ln, prompt),
	}

	// The deadline and the stall watchdog cancel the same context with
	// different causes, so the outcome can say which one fired.
	cancellable, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	laneCtx, cancelDeadline := context.WithTimeoutCause(cancellable, ln.Timeout, errDeadline)
	defer cancelDeadline()

	writer := newActivityWriter(logFile, r.cfg.Now)
	stopWatchdog := r.watch(laneCtx, ln, attempt, writer, cancelCause)

	start := r.cfg.Now()
	res, runErr := r.cfg.Exec.Run(laneCtx, cmd, writer)
	elapsed := r.cfg.Now().Sub(start)
	stopWatchdog()

	exitCode := res.ExitCode
	out := attemptOutcome{exitCode: &exitCode, duration: elapsed}

	switch cause := context.Cause(laneCtx); {
	case errors.Is(cause, errStalled):
		out.failure = failure{
			Kind:   failStall,
			Detail: fmt.Sprintf("no output for %s", r.cfg.Stall),
			Output: tailOfFile(logPath, maxContextBytes),
		}
		return out
	case errors.Is(cause, errDeadline):
		out.failure = failure{
			Kind:   failDeadline,
			Detail: fmt.Sprintf("exceeded %s", ln.Timeout),
			Output: tailOfFile(logPath, maxContextBytes),
		}
		return out
	case cause != nil && ctx.Err() != nil:
		out.failure = failure{Kind: failLaunch, Detail: fmt.Sprintf("%v: %v", errRunCancelled, cause)}
		return out
	}

	if runErr != nil {
		out.failure = failure{Kind: failLaunch, Detail: runErr.Error(), Output: tailOfFile(logPath, maxContextBytes)}
		return out
	}
	if res.ExitCode != 0 {
		out.failure = failure{
			Kind:   failExit,
			Detail: fmt.Sprintf("claude exited %d", res.ExitCode),
			Output: tailOfFile(logPath, maxContextBytes),
		}
		return out
	}

	// The process exited cleanly. That is not completion. Completion is the
	// predicate passing, observed by a process that did none of the work.
	predOut, predOK := r.verify(ctx, ln, worktree)
	out.predicateOutput = predOut
	if predOK {
		out.complete = true
		return out
	}
	out.failure = failure{
		Kind:   failPredicate,
		Detail: fmt.Sprintf("predicate did not pass for lane %s", ln.ID),
		Output: predOut,
	}
	return out
}

// verify runs the lane's acceptance predicate as its own process and returns
// its combined output.
func (r *Runner) verify(ctx context.Context, ln sprint.ResolvedLane, worktree string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.PredicateTimeout)
	defer cancel()

	var buf bytes.Buffer
	// No account environment is applied: the predicate is a check, not agent
	// work, and must not consume the quota it is meant to protect.
	// The predicate runs in the lane's worktree, so it judges the work the
	// lane actually did. A predicate written with an absolute path into the
	// primary checkout would escape it and verify the wrong tree.
	res, err := r.cfg.Exec.Run(ctx, machine.Command{
		Host:   ln.Host,
		Dir:    worktree,
		Script: ln.Predicate,
	}, &buf)
	output := tailBytes(buf.String(), maxContextBytes)
	if err != nil {
		return fmt.Sprintf("predicate could not run: %v\n%s", err, output), false
	}
	if res.Killed {
		return fmt.Sprintf("predicate exceeded %s\n%s", r.cfg.PredicateTimeout, output), false
	}
	if res.ExitCode != 0 {
		return fmt.Sprintf("predicate exited %d\n%s", res.ExitCode, output), false
	}
	return output, true
}

// watch kills a lane that has produced no output for the stall window, and
// keeps the lane's heartbeat file current so an outside observer can see
// liveness without reading the log.
func (r *Runner) watch(
	ctx context.Context,
	ln sprint.ResolvedLane,
	attempt int,
	writer *activityWriter,
	cancelCause context.CancelCauseFunc,
) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(r.cfg.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := r.cfg.Now()
				idle := writer.IdleFor(now)
				r.writeHeartbeat(ln, attempt, writer, now, idle)
				if idle >= r.cfg.Stall {
					r.cfg.Log.Warn("lane stalled; killing",
						"lane", ln.ID, "attempt", attempt, "idle", idle.Round(time.Second))
					cancelCause(errStalled)
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// heartbeat is the per-lane liveness file's contents.
type heartbeat struct {
	Lane         string    `json:"lane"`
	Attempt      int       `json:"attempt"`
	Machine      string    `json:"machine"`
	Observed     time.Time `json:"observed"`
	LastActivity time.Time `json:"last_activity"`
	IdleSeconds  float64   `json:"idle_seconds"`
	OutputBytes  int64     `json:"output_bytes"`
}

func (r *Runner) writeHeartbeat(ln sprint.ResolvedLane, attempt int, w *activityWriter, now time.Time, idle time.Duration) {
	path := filepath.Join(r.cfg.RunDir, "heartbeats", ln.ID+".json")
	hb := heartbeat{
		Lane: ln.ID, Attempt: attempt, Machine: ln.Machine,
		Observed: now.UTC(), LastActivity: w.LastActivity().UTC(),
		IdleSeconds: idle.Seconds(), OutputBytes: w.Bytes(),
	}
	body, err := jsonLine(hb)
	if err != nil {
		r.cfg.Log.Warn("encoding heartbeat", "lane", ln.ID, "error", err)
		return
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		// A heartbeat is observability, not control flow; losing one must not
		// take the lane down with it.
		r.cfg.Log.Warn("writing heartbeat", "lane", ln.ID, "error", err)
	}
}

func (r *Runner) record(rec results.Record) {
	rec.Sprint = r.cfg.Sprint.Name
	rec.Time = r.cfg.Now().UTC()
	if err := r.cfg.Recorder.Record(rec); err != nil {
		r.cfg.Log.Error("recording lane transition", "lane", rec.Lane, "state", rec.State, "error", err)
	}
}

// tailOfFile returns the last max bytes of a file, or an explanation of why it
// could not be read.
func tailOfFile(path string, max int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(could not read lane log %s: %v)", path, err)
	}
	return tailBytes(string(data), max)
}
