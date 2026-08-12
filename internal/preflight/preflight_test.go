package preflight_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/kazi-org/sprintd/internal/machine"
	"github.com/kazi-org/sprintd/internal/preflight"
	"github.com/kazi-org/sprintd/internal/sprint"
)

const sprintFile = `
sprint: demo
defaults: { model: sonnet, deadline: 10m, retries: 1 }
machines:
  mini: { host: local }
  dgx:  { host: user@10.0.0.2 }
accounts:
  - { name: primary, reserve_floor_pct: 30, weekly_token_limit: 100, config_dir: /tmp/primary }
  - { name: secondary, reserve_floor_pct: 0, weekly_token_limit: 100, config_dir: /tmp/secondary }
repos:
  app: { path: /srv/app, max_concurrent: 2 }
lanes:
  - { id: L1, repo: app, goal: g, prompt: p, predicate: "./check.sh", machine: mini }
  - { id: L2, repo: app, goal: g, prompt: p, predicate: "./check.sh", machine: dgx }
`

// scriptedExec answers commands by the first matching substring rule.
type scriptedExec struct {
	mu    sync.Mutex
	calls []machine.Command
	rules []rule
}

type rule struct {
	match  string
	host   string
	output string
	exit   int
	err    error
}

func (s *scriptedExec) Run(_ context.Context, cmd machine.Command, out io.Writer) (machine.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, cmd)
	s.mu.Unlock()

	for _, r := range s.rules {
		if !strings.Contains(cmd.Script, r.match) {
			continue
		}
		if r.host != "" && r.host != cmd.Host {
			continue
		}
		if r.err != nil {
			return machine.Result{ExitCode: -1}, r.err
		}
		fmt.Fprint(out, r.output)
		return machine.Result{ExitCode: r.exit}, nil
	}
	if strings.Contains(cmd.Script, "rev-parse") {
		fmt.Fprint(out, healthyCheckout)
		return machine.Result{ExitCode: 0}, nil
	}
	fmt.Fprint(out, "sprintd-ok")
	return machine.Result{ExitCode: 0}, nil
}

// healthyCheckout is what the status probe prints for a tree sitting exactly
// on its base.
const healthyCheckout = "branch=main\nbase=ok\nbehind=0\nahead=0\nancestor=yes\n"

// checkoutOutput renders the probe output for an arbitrary position.
func checkoutOutput(branch string, behind, ahead int, ancestor bool) string {
	yn := "no"
	if ancestor {
		yn = "yes"
	}
	return fmt.Sprintf("branch=%s\nbase=ok\nbehind=%d\nahead=%d\nancestor=%s\n", branch, behind, ahead, yn)
}

func (s *scriptedExec) scripts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, c.Script)
	}
	return out
}

func load(t *testing.T) *sprint.Sprint {
	t.Helper()
	s, err := sprint.Parse([]byte(sprintFile))
	if err != nil {
		t.Fatalf("parsing fixture sprint: %v", err)
	}
	return s
}

func TestAllChecksPass(t *testing.T) {
	t.Parallel()

	exec := &scriptedExec{rules: []rule{
		{match: "claude --version", output: "2.1.0\n"},
	}}
	reports := preflight.Run(context.Background(), load(t), exec, preflight.Options{})
	if !preflight.AllOK(reports) {
		t.Fatalf("AllOK() = false, want true; reports = %+v", reports)
	}
	if len(reports) != 2 {
		t.Fatalf("got %d machine reports, want 2", len(reports))
	}

	scripts := strings.Join(exec.scripts(), "\n")
	for _, want := range []string{
		"echo sprintd-ok",
		"test -d '/srv/app'/.git",
		"git fetch",
		"claude --version",
		"claude -p ",
	} {
		if !strings.Contains(scripts, want) {
			t.Errorf("preflight never ran a check containing %q; ran:\n%s", want, scripts)
		}
	}
}

func TestUnreachableMachineSkipsTheRest(t *testing.T) {
	t.Parallel()

	exec := &scriptedExec{rules: []rule{
		{match: "echo sprintd-ok", host: "user@10.0.0.2", err: errors.New("ssh: connect: host unreachable")},
	}}
	reports := preflight.Run(context.Background(), load(t), exec, preflight.Options{})
	if preflight.AllOK(reports) {
		t.Fatal("AllOK() = true, want false when a machine is unreachable")
	}

	var dgx preflight.Report
	for _, r := range reports {
		if r.Machine == "dgx" {
			dgx = r
		}
	}
	if dgx.OK() {
		t.Error("dgx report OK() = true, want false")
	}
	if len(dgx.Checks) != 2 {
		t.Fatalf("dgx ran %d checks, want the reachability probe plus a skip note", len(dgx.Checks))
	}
	if !strings.Contains(dgx.Checks[1].Detail, "skipped") {
		t.Errorf("second dgx check = %+v, want a skip note", dgx.Checks[1])
	}
	for _, script := range exec.scripts() {
		if strings.Contains(script, "git fetch") && strings.Contains(script, "10.0.0.2") {
			t.Error("preflight kept probing an unreachable machine")
		}
	}
}

func TestFailingChecksAreReported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rule       rule
		wantDetail string
	}{
		{
			name:       "missing repo",
			rule:       rule{match: "test -d", exit: 1},
			wantDetail: "exit 1",
		},
		{
			name:       "git fetch cannot authenticate",
			rule:       rule{match: "git fetch", exit: 128, output: "fatal: could not read from remote"},
			wantDetail: "could not read from remote",
		},
		{
			name:       "claude is not installed",
			rule:       rule{match: "claude --version", exit: 127, output: "bash: claude: command not found"},
			wantDetail: "exit 127",
		},
		{
			name: "the agent exits zero but answers nothing",
			// This is the locked-keychain shape: the process is fine, the
			// work never happens.
			rule:       rule{match: "claude -p", exit: 0, output: ""},
			wantDetail: "did not produce",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			exec := &scriptedExec{rules: []rule{tc.rule, {match: "claude --version", output: "2.1.0"}}}
			reports := preflight.Run(context.Background(), load(t), exec, preflight.Options{})
			if preflight.AllOK(reports) {
				t.Fatal("AllOK() = true, want false")
			}
			var detail string
			for _, r := range reports {
				for _, c := range r.Checks {
					if !c.OK {
						detail = c.Detail
					}
				}
			}
			if !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("failure detail = %q, want it to contain %q", detail, tc.wantDetail)
			}
		})
	}
}

func TestEveryAccountWithItsOwnConfigDirIsProbed(t *testing.T) {
	t.Parallel()

	exec := &scriptedExec{rules: []rule{{match: "claude --version", output: "2.1.0"}}}
	reports := preflight.Run(context.Background(), load(t), exec, preflight.Options{})

	var probed []string
	for _, r := range reports {
		if r.Machine != "mini" {
			continue
		}
		for _, c := range r.Checks {
			if strings.HasPrefix(c.Name, "claude -p") {
				probed = append(probed, c.Name)
			}
		}
	}
	if len(probed) != 2 {
		t.Fatalf("probed %v, want one agent probe per account with its own config dir", probed)
	}
	for _, want := range []string{"account primary", "account secondary"} {
		if !strings.Contains(strings.Join(probed, " "), want) {
			t.Errorf("agent probes %v, want one naming %q", probed, want)
		}
	}
}

func TestKilledCheckIsReportedAsTimeout(t *testing.T) {
	t.Parallel()

	exec := &killingExec{}
	reports := preflight.Run(context.Background(), load(t), exec, preflight.Options{})
	if preflight.AllOK(reports) {
		t.Fatal("AllOK() = true, want false")
	}
	if detail := reports[0].Checks[0].Detail; detail != "timed out" {
		t.Errorf("detail = %q, want \"timed out\"", detail)
	}
}

type killingExec struct{}

func (killingExec) Run(context.Context, machine.Command, io.Writer) (machine.Result, error) {
	return machine.Result{ExitCode: -1, Killed: true}, nil
}

func TestAllOKIsFalseForNoReports(t *testing.T) {
	t.Parallel()

	if preflight.AllOK(nil) {
		t.Error("AllOK(nil) = true, want false: nothing checked is not the same as everything passing")
	}
}

func TestRender(t *testing.T) {
	t.Parallel()

	exec := &scriptedExec{rules: []rule{
		{match: "claude --version", output: "2.1.0"},
		{match: "test -d", exit: 1, output: "no such directory"},
	}}
	reports := preflight.Run(context.Background(), load(t), exec, preflight.Options{})

	var out strings.Builder
	if err := preflight.Render(&out, reports); err != nil {
		t.Fatalf("Render() error = %v, want nil", err)
	}
	text := out.String()
	for _, want := range []string{"MACHINE", "mini", "dgx", "FAIL", "pass", "machines ready:"} {
		if !strings.Contains(text, want) {
			t.Errorf("Render() output missing %q; got:\n%s", want, text)
		}
	}
}

// TestStaleCheckoutFailsPreflight is the neglected-checkout case. A tree far
// behind its base feeds lanes stale source and stale CI config while every
// lane reports success, so preflight fails rather than warns.
func TestStaleCheckoutFailsPreflight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		checkout   string
		wantOK     bool
		wantDetail []string
	}{
		{
			name:     "exactly on the base",
			checkout: checkoutOutput("main", 0, 0, true),
			wantOK:   true,
		},
		{
			name:     "a little behind is tolerated",
			checkout: checkoutOutput("main", 10, 0, true),
			wantOK:   true,
		},
		{
			name:     "at the threshold is tolerated",
			checkout: checkoutOutput("main", 50, 0, true),
			wantOK:   true,
		},
		{
			name:       "past the threshold fails",
			checkout:   checkoutOutput("main", 51, 0, true),
			wantDetail: []string{"51 behind", "origin/main", "more than 50 behind"},
		},
		{
			name:       "the real neglected checkout fails with its numbers",
			checkout:   checkoutOutput("develop", 1550, 433, false),
			wantDetail: []string{"develop", "1550 behind", "433 ahead", "not an ancestor"},
		},
		{
			name:       "a diverged but current branch still fails",
			checkout:   checkoutOutput("wip", 0, 7, false),
			wantDetail: []string{"wip", "7 ahead", "commits the base does not"},
		},
		{
			name:       "an unfetched base fails",
			checkout:   "branch=main\nbase=missing\n",
			wantDetail: []string{"origin/main", "does not exist"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			exec := &scriptedExec{rules: []rule{
				{match: "rev-parse", output: tc.checkout},
				{match: "claude --version", output: "2.1.0"},
			}}
			reports := preflight.Run(context.Background(), load(t), exec, preflight.Options{})
			if got := preflight.AllOK(reports); got != tc.wantOK {
				t.Fatalf("AllOK() = %v, want %v", got, tc.wantOK)
			}
			if tc.wantOK {
				return
			}
			var detail string
			for _, r := range reports {
				for _, c := range r.Checks {
					if !c.OK && strings.HasPrefix(c.Name, "checkout") {
						detail = c.Detail
					}
				}
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(detail, want) {
					t.Errorf("checkout failure detail = %q, want it to contain %q", detail, want)
				}
			}
		})
	}
}

// TestStaleThresholdIsPerRepo checks the sprint file can loosen or tighten the
// gate for one repo.
func TestStaleThresholdIsPerRepo(t *testing.T) {
	t.Parallel()

	src := strings.Replace(sprintFile,
		"app: { path: /srv/app, max_concurrent: 2 }",
		"app: { path: /srv/app, max_concurrent: 2, base: origin/trunk, stale_threshold: 2000 }", 1)
	s, err := sprint.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parsing sprint: %v", err)
	}

	exec := &scriptedExec{rules: []rule{
		{match: "rev-parse", output: checkoutOutput("develop", 1550, 0, true)},
		{match: "claude --version", output: "2.1.0"},
	}}
	reports := preflight.Run(context.Background(), s, exec, preflight.Options{})
	if !preflight.AllOK(reports) {
		t.Fatalf("AllOK() = false, want true: 1550 behind is under the repo's 2000 threshold; reports = %+v", reports)
	}
	joined := strings.Join(exec.scripts(), "\n")
	if !strings.Contains(joined, "origin/trunk") {
		t.Errorf("probe did not use the repo's declared base; ran:\n%s", joined)
	}
}

// TestMissingRepoSkipsFreshnessChecks avoids reporting a confusing git error
// on top of the real problem.
func TestMissingRepoSkipsFreshnessChecks(t *testing.T) {
	t.Parallel()

	exec := &scriptedExec{rules: []rule{
		{match: "test -d", exit: 1},
		{match: "claude --version", output: "2.1.0"},
	}}
	reports := preflight.Run(context.Background(), load(t), exec, preflight.Options{})
	if preflight.AllOK(reports) {
		t.Fatal("AllOK() = true, want false")
	}
	for _, r := range reports {
		for _, c := range r.Checks {
			if strings.HasPrefix(c.Name, "checkout ") || strings.HasPrefix(c.Name, "git fetch ") {
				t.Errorf("ran %q against a repo that is not there", c.Name)
			}
		}
	}
}

// TestClaudeProbeSkippedWhenNoLaneUsesIt keeps a sprint of purely
// command-driven lanes from failing on an account probe for a tool it never
// invokes.
func TestClaudeProbeSkippedWhenNoLaneUsesIt(t *testing.T) {
	t.Parallel()

	src := strings.NewReplacer(
		`prompt: p, predicate: "./check.sh", machine: mini }`, `command: "kazi apply goals/a.toml", predicate: "./check.sh", machine: mini }`,
		`prompt: p, predicate: "./check.sh", machine: dgx }`, `command: "make verify", predicate: "./check.sh", machine: dgx }`,
	).Replace(sprintFile)
	s, err := sprint.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parsing sprint: %v", err)
	}
	if s.UsesClaude() {
		t.Fatal("fixture still has a claude-composed lane")
	}

	// Every claude invocation fails; the sprint should still be ready.
	exec := &scriptedExec{rules: []rule{
		{match: "claude", exit: 127, output: "bash: claude: command not found"},
	}}
	reports := preflight.Run(context.Background(), s, exec, preflight.Options{})
	if !preflight.AllOK(reports) {
		t.Fatalf("AllOK() = false, want true; reports = %+v", reports)
	}
	for _, script := range exec.scripts() {
		if strings.Contains(script, "claude") {
			t.Errorf("probed claude for a sprint that never runs it: %q", script)
		}
	}
	var noted bool
	for _, r := range reports {
		for _, c := range r.Checks {
			if c.Name == "claude" && strings.Contains(c.Detail, "skipped") {
				noted = true
			}
		}
	}
	if !noted {
		t.Error("the skip was not reported; it must be visible, not silently dropped")
	}
}

// TestClaudeProbeRunsWhenAnyLaneUsesIt is the other half: one composed lane is
// enough to make the probe blocking again.
func TestClaudeProbeRunsWhenAnyLaneUsesIt(t *testing.T) {
	t.Parallel()

	src := strings.Replace(sprintFile,
		`prompt: p, predicate: "./check.sh", machine: mini }`,
		`command: "kazi apply goals/a.toml", predicate: "./check.sh", machine: mini }`, 1)
	s, err := sprint.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parsing sprint: %v", err)
	}
	if !s.UsesClaude() {
		t.Fatal("fixture should still have one claude-composed lane")
	}
	exec := &scriptedExec{rules: []rule{
		{match: "claude", exit: 127, output: "bash: claude: command not found"},
	}}
	if preflight.AllOK(preflight.Run(context.Background(), s, exec, preflight.Options{})) {
		t.Error("AllOK() = true, want false: a lane still needs claude and it is missing")
	}
}
