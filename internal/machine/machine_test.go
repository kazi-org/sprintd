package machine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kazi-org/sprintd/internal/machine"
)

func TestQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain word", "hello", `'hello'`},
		{"spaces", "two words", `'two words'`},
		{"single quote", "it's", `'it'\''s'`},
		{"command substitution stays inert", "$(rm -rf /)", `'$(rm -rf /)'`},
		{"backticks stay inert", "`whoami`", "'`whoami`'"},
		{"newlines survive", "a\nb", "'a\nb'"},
		{"empty", "", `''`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := machine.Quote(tc.in); got != tc.want {
				t.Errorf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestQuoteSurvivesTheShell proves the quoting is not merely plausible: the
// shell must hand the original bytes back unchanged. A lane prompt is
// attacker-shaped input by nature -- it contains quotes, backticks and dollar
// signs written by a model.
func TestQuoteSurvivesTheShell(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"it's a $HOME `whoami` \"test\"",
		"$(touch /tmp/sprintd-should-not-exist)",
		"multi\nline\tinput",
		"emoji ok",
	}
	exec := machine.NewExecutor()
	for _, in := range inputs {
		t.Run(in[:min(len(in), 20)], func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			res, err := exec.Run(context.Background(), machine.Command{
				Host:   machine.LocalHost,
				Script: "printf %s " + machine.Quote(in),
			}, &out)
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if !res.OK() {
				t.Fatalf("Run() result = %+v, want a clean exit", res)
			}
			if out.String() != in {
				t.Errorf("shell returned %q, want %q", out.String(), in)
			}
		})
	}
}

func TestRemoteScript(t *testing.T) {
	t.Parallel()

	got := machine.RemoteScript(machine.Command{
		Host:   "user@example",
		Dir:    "/srv/app",
		Env:    map[string]string{"B": "2", "A": "1"},
		Script: "echo hi",
	})
	want := `cd '/srv/app' && export A='1' && export B='2' && echo hi`
	if got != want {
		t.Errorf("RemoteScript() = %q, want %q", got, want)
	}
}

func TestRemoteScriptIsDeterministic(t *testing.T) {
	t.Parallel()

	cmd := machine.Command{Dir: "/x", Env: map[string]string{"A": "1", "B": "2", "C": "3"}, Script: "true"}
	first := machine.RemoteScript(cmd)
	for i := 0; i < 20; i++ {
		if got := machine.RemoteScript(cmd); got != first {
			t.Fatalf("RemoteScript() varied between calls: %q then %q", first, got)
		}
	}
}

func TestRunLocal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		script     string
		env        map[string]string
		wantExit   int
		wantOutput string
	}{
		{name: "clean exit", script: "echo hello", wantExit: 0, wantOutput: "hello"},
		{name: "non-zero exit is reported, not an error", script: "exit 7", wantExit: 7},
		{name: "stderr is captured too", script: "echo oops >&2", wantExit: 0, wantOutput: "oops"},
		{
			name:       "environment is applied",
			script:     `echo "$SPRINTD_TEST_VAR"`,
			env:        map[string]string{"SPRINTD_TEST_VAR": "applied"},
			wantExit:   0,
			wantOutput: "applied",
		},
	}
	exec := machine.NewExecutor()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out strings.Builder
			res, err := exec.Run(context.Background(), machine.Command{
				Host: machine.LocalHost, Script: tc.script, Env: tc.env,
			}, &out)
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if res.ExitCode != tc.wantExit {
				t.Errorf("ExitCode = %d, want %d", res.ExitCode, tc.wantExit)
			}
			if res.Killed {
				t.Error("Killed = true, want false")
			}
			if tc.wantOutput != "" && !strings.Contains(out.String(), tc.wantOutput) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tc.wantOutput)
			}
		})
	}
}

func TestRunWorkingDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var out strings.Builder
	res, err := machine.NewExecutor().Run(context.Background(), machine.Command{
		Host: machine.LocalHost, Dir: dir, Script: "pwd",
	}, &out)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if !res.OK() {
		t.Fatalf("Run() result = %+v, want a clean exit", res)
	}
	// macOS reports /private/var for /var, so compare on the suffix.
	if !strings.HasSuffix(strings.TrimSpace(out.String()), strings.TrimPrefix(dir, "/private")) {
		t.Errorf("pwd = %q, want it to be %q", strings.TrimSpace(out.String()), dir)
	}
}

// TestCancellationKillsTheProcessGroup is the property that makes deadlines
// mean anything: a lane that forked a long-running child must not outlive its
// own cancellation. It also stands in for timeout(1), which macOS does not
// ship.
func TestCancellationKillsTheProcessGroup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	var out strings.Builder
	res, err := machine.NewExecutor().Run(ctx, machine.Command{
		Host: machine.LocalHost,
		// A child that would far outlive the deadline, with the parent
		// waiting on it.
		Script: "sleep 60 & wait",
	}, &out)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if !res.Killed {
		t.Errorf("Killed = false, want true after the context expired")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Run() took %v, want it to return promptly after cancellation", elapsed)
	}
}

func TestRunRejectsEmptyScript(t *testing.T) {
	t.Parallel()

	_, err := machine.NewExecutor().Run(context.Background(), machine.Command{Host: machine.LocalHost}, &strings.Builder{})
	if err == nil {
		t.Error("Run() error = nil, want an error for an empty script")
	}
}

func TestResultOK(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		res  machine.Result
		want bool
	}{
		{"clean exit", machine.Result{ExitCode: 0}, true},
		{"non-zero exit", machine.Result{ExitCode: 1}, false},
		{"killed even with a zero code", machine.Result{ExitCode: 0, Killed: true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.res.OK(); got != tc.want {
				t.Errorf("OK() = %v, want %v", got, tc.want)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestRemoteScriptEnvSurvivesTheShell covers the ssh path, where environment
// is applied by exporting it inside a remote shell rather than through
// cmd.Env. Lane context is multi-line and contains quotes and shell
// metacharacters, so a naive export would corrupt it or break the script.
func TestRemoteScriptEnvSurvivesTheShell(t *testing.T) {
	t.Parallel()

	value := "predicate failed: it's broken\n\nFAIL: $(whoami) saw `2` prompts\nexpected 1"
	script := machine.RemoteScript(machine.Command{
		Env:    map[string]string{"SPRINTD_LAST_FAILURE": value},
		Script: `printf %s "$SPRINTD_LAST_FAILURE"`,
	})

	// Run the generated remote script locally: this is exactly what ssh would
	// hand to bash on the far side.
	var out strings.Builder
	res, err := machine.NewExecutor().Run(context.Background(), machine.Command{
		Host: machine.LocalHost, Script: script,
	}, &out)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if !res.OK() {
		t.Fatalf("Run() result = %+v, want a clean exit", res)
	}
	if out.String() != value {
		t.Errorf("environment did not survive the remote shell:\n got %q\nwant %q", out.String(), value)
	}
}
