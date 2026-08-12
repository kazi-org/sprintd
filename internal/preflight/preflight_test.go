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
	fmt.Fprint(out, "sprintd-ok")
	return machine.Result{ExitCode: 0}, nil
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
		"git fetch --dry-run",
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
			wantDetail: "exit 128",
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
