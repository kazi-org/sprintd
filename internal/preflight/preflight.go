// Package preflight verifies that every machine in a sprint can actually run
// lanes, before any lane fans out.
//
// The class of failure this catches is a lane that burns its whole deadline
// doing nothing because ssh was not reachable, the repo path was wrong, or the
// credentials behind an account could not be unlocked. Twenty lanes discover
// that twenty times; one preflight discovers it once.
package preflight

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/kazi-org/sprintd/internal/gitrepo"
	"github.com/kazi-org/sprintd/internal/machine"
	"github.com/kazi-org/sprintd/internal/sprint"
)

// DefaultBudget is the per-machine wall-clock budget for the whole check set.
const DefaultBudget = 60 * time.Second

// probeMarker is echoed by the reachability probe and by the trivial agent
// call, so a check passes on observed output rather than on exit status alone.
const probeMarker = "sprintd-ok"

// Executor runs a command on a machine.
type Executor interface {
	Run(ctx context.Context, cmd machine.Command, out io.Writer) (machine.Result, error)
}

// Check is one verification and its result.
type Check struct {
	Name     string
	OK       bool
	Detail   string
	Duration time.Duration
}

// Report is every check for one machine.
type Report struct {
	Machine string
	Host    string
	Checks  []Check
}

// OK reports whether every check on the machine passed.
func (r Report) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return len(r.Checks) > 0
}

// AllOK reports whether every machine passed.
func AllOK(reports []Report) bool {
	for _, r := range reports {
		if !r.OK() {
			return false
		}
	}
	return len(reports) > 0
}

// Options tunes a preflight run.
type Options struct {
	// Budget bounds each machine's whole check set. Zero means DefaultBudget.
	Budget time.Duration
}

// Run checks every machine the sprint's lanes actually use, in parallel.
func Run(ctx context.Context, s *sprint.Sprint, exec Executor, opts Options) []Report {
	budget := opts.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	machineRepos := s.MachineRepos()
	names := make([]string, 0, len(machineRepos))
	for name := range machineRepos {
		names = append(names, name)
	}
	sort.Strings(names)

	reports := make([]Report, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			mctx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			reports[i] = checkMachine(mctx, s, exec, name, machineRepos[name])
		}(i, name)
	}
	wg.Wait()
	return reports
}

// checkRepoFreshness fetches the repo and grades where the checkout sits
// relative to the base lanes branch from.
//
// A repo path in a sprint file is not necessarily a usable checkout. One left
// on an old branch, far behind its base, feeds lanes stale source and stale CI
// config, and they report success while reading it. That is why being behind
// is a failure here and not a warning, and why the message carries the actual
// counts instead of the words "stale checkout".
func checkRepoFreshness(ctx context.Context, exec Executor, host string, repo sprint.RepoTarget) []Check {
	start := time.Now()
	if err := gitrepo.Fetch(ctx, exec, host, repo.Path); err != nil {
		return []Check{{
			Name:     "git fetch " + repo.Name,
			Detail:   err.Error(),
			Duration: time.Since(start),
		}}
	}
	checks := []Check{{
		Name: "git fetch " + repo.Name, OK: true, Duration: time.Since(start),
	}}

	start = time.Now()
	status, err := gitrepo.Inspect(ctx, exec, host, repo.Path, repo.Base)
	check := Check{Name: "checkout " + repo.Name, Duration: time.Since(start)}
	switch {
	case err != nil:
		check.Detail = err.Error()
	case !status.BaseResolved:
		check.Detail = fmt.Sprintf("%s; fetch it or correct the repo's base", status.Describe(repo.Base))
	case status.Stale(repo.StaleThreshold):
		check.Detail = fmt.Sprintf("%s; more than %d behind, so lanes would read stale source",
			status.Describe(repo.Base), repo.StaleThreshold)
	case status.Diverged():
		check.Detail = fmt.Sprintf("%s; it carries commits the base does not", status.Describe(repo.Base))
	default:
		check.OK = true
		check.Detail = status.Describe(repo.Base)
	}
	return append(checks, check)
}

func checkMachine(ctx context.Context, s *sprint.Sprint, exec Executor, name string, repos []sprint.RepoTarget) Report {
	host := s.Machines[name].Host
	report := Report{Machine: name, Host: host}

	reach := runCheck(ctx, exec, "reachable", machine.Command{
		Host:   host,
		Script: "echo " + probeMarker,
	}, probeMarker)
	report.Checks = append(report.Checks, reach)
	if !reach.OK {
		// Every later check would fail the same way and add nothing.
		report.Checks = append(report.Checks, Check{
			Name:   "remaining checks",
			Detail: "skipped: machine is not reachable",
		})
		return report
	}

	for _, repo := range repos {
		present := runCheck(ctx, exec, "repo "+repo.Name, machine.Command{
			Host:   host,
			Script: fmt.Sprintf("test -d %s/.git && echo %s", machine.Quote(repo.Path), probeMarker),
		}, probeMarker)
		report.Checks = append(report.Checks, present)
		if !present.OK {
			continue
		}
		report.Checks = append(report.Checks, checkRepoFreshness(ctx, exec, host, repo)...)
	}

	report.Checks = append(report.Checks, runCheck(ctx, exec, "claude --version",
		machine.Command{Host: host, Script: "claude --version"}, ""))

	for _, acct := range s.Accounts {
		env := map[string]string{}
		label := "claude -p"
		if acct.ConfigDir != "" {
			// Only an account with its own config dir is separately testable;
			// accounts sharing the ambient dir are the same credentials.
			env["CLAUDE_CONFIG_DIR"] = acct.ConfigDir
			label = "claude -p (account " + acct.Name + ")"
		}
		report.Checks = append(report.Checks, runCheck(ctx, exec, label, machine.Command{
			Host: host, Env: env,
			Script: fmt.Sprintf("claude -p %s", machine.Quote("reply with exactly: "+probeMarker)),
		}, probeMarker))
		if acct.ConfigDir == "" {
			// The ambient credentials only need testing once.
			break
		}
	}
	return report
}

// runCheck runs one command and grades it on exit status and, when wantOutput
// is set, on the output actually containing the marker.
func runCheck(ctx context.Context, exec Executor, name string, cmd machine.Command, wantOutput string) Check {
	start := time.Now()
	var buf bytes.Buffer
	res, err := exec.Run(ctx, cmd, &buf)
	check := Check{Name: name, Duration: time.Since(start)}
	output := strings.TrimSpace(buf.String())

	switch {
	case err != nil:
		check.Detail = err.Error()
	case res.Killed:
		check.Detail = "timed out"
	case res.ExitCode != 0:
		check.Detail = fmt.Sprintf("exit %d: %s", res.ExitCode, firstLine(output))
	case wantOutput != "" && !strings.Contains(output, wantOutput):
		check.Detail = fmt.Sprintf("exited 0 but did not produce %q: %s", wantOutput, firstLine(output))
	default:
		check.OK = true
		check.Detail = firstLine(output)
	}
	return check
}

// Render writes the per-machine pass/fail table.
func Render(w io.Writer, reports []Report) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tHOST\tCHECK\tRESULT\tTOOK\tDETAIL")
	for _, r := range reports {
		for _, c := range r.Checks {
			result := "FAIL"
			if c.OK {
				result = "pass"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Machine, r.Host, c.Name, result,
				c.Duration.Round(time.Millisecond), truncate(c.Detail, 70))
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing preflight table: %w", err)
	}
	passed, failed := 0, 0
	for _, r := range reports {
		if r.OK() {
			passed++
		} else {
			failed++
		}
	}
	if _, err := fmt.Fprintf(w, "\nmachines ready: %d  not ready: %d\n", passed, failed); err != nil {
		return fmt.Errorf("writing preflight summary: %w", err)
	}
	return nil
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
