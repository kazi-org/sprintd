// Command sprintd dispatches deadline-bounded agent lanes across machines and
// verifies each one with an acceptance predicate run by a separate process.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/kazi-org/sprintd/internal/allocator"
	"github.com/kazi-org/sprintd/internal/machine"
	"github.com/kazi-org/sprintd/internal/preflight"
	"github.com/kazi-org/sprintd/internal/results"
	"github.com/kazi-org/sprintd/internal/runner"
	"github.com/kazi-org/sprintd/internal/sprint"
	"github.com/kazi-org/sprintd/internal/status"
)

// defaultRunRoot is where run directories are created, and so where status
// looks when it is not told which run to read.
const defaultRunRoot = ".sprintd"

// version is set at build time by the release pipeline.
var version = "dev"

// Exit statuses. Anything other than exitOK means the sprint did not fully
// land, which is what a supervisor should act on.
const (
	exitOK        = 0
	exitError     = 1
	exitEscalated = 2
	exitPreflight = 3
)

const usage = `sprintd dispatches deadline-bounded agent lanes and verifies them with predicates.

Usage:
  sprintd preflight --sprint <file>   check every machine can run lanes
  sprintd run       --sprint <file>   dispatch the sprint
  sprintd status  [--run <dir>] [--json]   show the state of a run
  sprintd version                     print the version

Run "sprintd <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitError)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	code := exitOK
	switch os.Args[1] {
	case "preflight":
		code, err = cmdPreflight(ctx, os.Args[2:])
	case "run":
		code, err = cmdRun(ctx, os.Args[2:])
	case "status":
		code, err = cmdStatus(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("sprintd %s\n", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "sprintd: unknown command %q\n\n%s", os.Args[1], usage)
		code = exitError
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sprintd: %v\n", err)
		if code == exitOK {
			code = exitError
		}
	}
	os.Exit(code)
}

func cmdPreflight(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("preflight", flag.ExitOnError)
	path := fs.String("sprint", "", "path to the sprint file (required)")
	budget := fs.Duration("budget", preflight.DefaultBudget, "per-machine time budget")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	if *path == "" {
		return exitError, errors.New("preflight: --sprint is required")
	}
	s, err := sprint.Load(*path)
	if err != nil {
		return exitError, err
	}
	reports := preflight.Run(ctx, s, machine.NewExecutor(), preflight.Options{Budget: *budget})
	if err := preflight.Render(os.Stdout, reports); err != nil {
		return exitError, err
	}
	if !preflight.AllOK(reports) {
		return exitPreflight, errors.New("preflight failed; fix the machines above or re-run with --force")
	}
	return exitOK, nil
}

func cmdRun(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("sprint", "", "path to the sprint file (required)")
	runDir := fs.String("run-dir", "", "directory for logs and results (default .sprintd/<sprint>-<timestamp>)")
	stall := fs.Duration("stall", runner.DefaultStall,
		"kill a lane that produces no output for this long; passing it explicitly also applies it to prompt lanes")
	predicateTimeout := fs.Duration("predicate-timeout", runner.DefaultPredicateTimeout, "bound on a single predicate run")
	poll := fs.Duration("poll", runner.DefaultPollInterval, "how often the watchdog samples lane activity")
	force := fs.Bool("force", false, "dispatch even if preflight fails")
	skipPreflight := fs.Bool("skip-preflight", false, "do not run preflight at all")
	ccusageCmd := fs.String("ccusage", "", "ccusage command to read account usage (default: ccusage on PATH, else npx)")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	// An explicitly passed --stall is the operator saying they want the
	// watchdog on prompt lanes too, where it is otherwise off by default.
	stallExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "stall" {
			stallExplicit = true
		}
	})
	if *path == "" {
		return exitError, errors.New("run: --sprint is required")
	}
	s, err := sprint.Load(*path)
	if err != nil {
		return exitError, err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	exec := machine.NewExecutor()

	if !*skipPreflight {
		reports := preflight.Run(ctx, s, exec, preflight.Options{})
		if err := preflight.Render(os.Stderr, reports); err != nil {
			return exitError, err
		}
		if !preflight.AllOK(reports) {
			if !*force {
				return exitPreflight, errors.New("preflight failed; nothing dispatched (re-run with --force to override)")
			}
			log.Warn("preflight failed but --force was given; dispatching anyway")
		}
	}

	dir := *runDir
	if dir == "" {
		dir = filepath.Join(defaultRunRoot, fmt.Sprintf("%s-%s", s.Name, time.Now().UTC().Format("20060102T150405Z")))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return exitError, fmt.Errorf("creating run directory %s: %w", dir, err)
	}

	// The manifest goes down before the allocator reads usage, which shells
	// out to ccusage and is slow: otherwise a watcher sees no manifest, and so
	// no answer to "is this running", for the first minute of every sprint.
	startedAt := time.Now().UTC()
	if err := status.WriteManifest(dir, runSkeleton(s, startedAt)); err != nil {
		log.Error("writing run manifest", "error", err)
	}

	recorder, err := results.NewRecorder(filepath.Join(dir, results.FileName))
	if err != nil {
		return exitError, err
	}
	defer func() {
		if err := recorder.Close(); err != nil {
			log.Error("closing results file", "error", err)
		}
	}()

	reader := allocator.DetectCCUsage()
	if *ccusageCmd != "" {
		reader = &allocator.CCUsage{Argv: []string{*ccusageCmd}}
	}
	alloc := allocator.New(ctx, s.Accounts, reader, log)

	r, err := runner.New(runner.Config{
		Sprint:           s,
		RunDir:           dir,
		Exec:             exec,
		Allocator:        alloc,
		Recorder:         recorder,
		Stall:            *stall,
		PollInterval:     *poll,
		PredicateTimeout: *predicateTimeout,
		Log:              log,
		StartedAt:        startedAt,
		StallComposed:    stallExplicit,
	})
	if err != nil {
		return exitError, err
	}

	// A safety mechanism that is off has to be visibly off.
	log.Info("watchdog policy", "policy", r.StallPolicy().String())
	log.Info("dispatching sprint", "sprint", s.Name, "lanes", len(s.Lanes), "run_dir", dir)
	summary, err := r.Run(ctx)
	if err != nil {
		return exitError, err
	}

	recs, err := results.Load(filepath.Join(dir, results.FileName))
	if err != nil {
		return exitError, err
	}
	if err := results.RenderStatus(os.Stdout, s.Name, recs); err != nil {
		return exitError, err
	}
	fmt.Printf("\nrun directory: %s\n", dir)

	if len(summary.Escalated) > 0 {
		return exitEscalated, fmt.Errorf("%d lane(s) escalated: %v", len(summary.Escalated), summary.Escalated)
	}
	return exitOK, nil
}

// runSkeleton describes the sprint for the manifest written at run start.
func runSkeleton(s *sprint.Sprint, startedAt time.Time) status.Manifest {
	var machines []status.Machine
	names := make([]string, 0, len(s.Machines))
	for name := range s.Machines {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		machines = append(machines, status.Machine{Name: name, Host: s.Machines[name].Host})
	}
	var lanes []status.LaneSpec
	for _, ln := range s.ResolvedLanes() {
		lanes = append(lanes, status.LaneSpec{
			ID: ln.ID, Repo: ln.Repo, Machine: ln.Machine,
			Goal: ln.Goal, Model: ln.Model, Command: ln.Command,
		})
	}
	var opened, closes *time.Time
	if !s.Opened.IsZero() {
		o := s.Opened
		opened = &o
	}
	if !s.Closes.IsZero() {
		c := s.Closes
		closes = &c
	}
	return status.Skeleton(s.Name, opened, closes, startedAt, machines, lanes)
}

func cmdStatus(args []string) (int, error) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dir := fs.String("run", "", "run directory (default: the most recent under .sprintd)")
	asJSON := fs.Bool("json", false, "emit the machine-readable report instead of a table")
	root := fs.String("run-root", defaultRunRoot, "where to look for the most recent run")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}

	resolved := *dir
	if resolved == "" {
		found, ok, err := status.FindLatestRun(*root)
		if err != nil {
			return exitError, err
		}
		if ok {
			resolved = found
		}
	}

	if *asJSON {
		return statusJSON(resolved)
	}
	if resolved == "" {
		return exitError, errors.New("status: no run found; pass --run <dir>")
	}
	recs, err := results.Load(filepath.Join(resolved, results.FileName))
	if err != nil {
		return exitError, err
	}
	if err := results.RenderStatus(os.Stdout, "", recs); err != nil {
		return exitError, err
	}
	if escalated := results.Escalated(recs); len(escalated) > 0 {
		return exitEscalated, fmt.Errorf("%d lane(s) escalated: %v", len(escalated), escalated)
	}
	return exitOK, nil
}

// statusJSON emits the machine-readable report.
//
// The report is always written, including when there is no run at all: a
// consumer's honest empty state depends on getting the no-run shape and exit
// 0, not an error it has to interpret.
func statusJSON(runDir string) (int, error) {
	report, err := status.Build(runDir, time.Now())
	if err != nil {
		return exitError, err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return exitError, fmt.Errorf("writing status json: %w", err)
	}
	if report.Totals.Escalated > 0 {
		return exitEscalated, nil
	}
	return exitOK, nil
}
