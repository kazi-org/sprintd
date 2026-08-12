// Command sprintd dispatches deadline-bounded agent lanes across machines and
// verifies each one with an acceptance predicate run by a separate process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kazi-org/sprintd/internal/allocator"
	"github.com/kazi-org/sprintd/internal/machine"
	"github.com/kazi-org/sprintd/internal/preflight"
	"github.com/kazi-org/sprintd/internal/results"
	"github.com/kazi-org/sprintd/internal/runner"
	"github.com/kazi-org/sprintd/internal/sprint"
)

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
  sprintd status    --run <dir>       show the state of a run
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
	stall := fs.Duration("stall", runner.DefaultStall, "kill a lane that produces no output for this long")
	predicateTimeout := fs.Duration("predicate-timeout", runner.DefaultPredicateTimeout, "bound on a single predicate run")
	poll := fs.Duration("poll", runner.DefaultPollInterval, "how often the watchdog samples lane activity")
	force := fs.Bool("force", false, "dispatch even if preflight fails")
	skipPreflight := fs.Bool("skip-preflight", false, "do not run preflight at all")
	ccusageCmd := fs.String("ccusage", "", "ccusage command to read account usage (default: ccusage on PATH, else npx)")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
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
		dir = filepath.Join(".sprintd", fmt.Sprintf("%s-%s", s.Name, time.Now().UTC().Format("20060102T150405Z")))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return exitError, fmt.Errorf("creating run directory %s: %w", dir, err)
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
	})
	if err != nil {
		return exitError, err
	}

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

func cmdStatus(args []string) (int, error) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dir := fs.String("run", "", "run directory (required)")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	if *dir == "" {
		return exitError, errors.New("status: --run is required")
	}
	recs, err := results.Load(filepath.Join(*dir, results.FileName))
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
