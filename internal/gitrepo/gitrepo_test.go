package gitrepo_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/kazi-org/sprintd/internal/gitrepo"
	"github.com/kazi-org/sprintd/internal/machine"
)

// scriptedExec answers by the first rule whose substring matches.
type scriptedExec struct {
	mu    sync.Mutex
	calls []machine.Command
	rules []rule
}

type rule struct {
	match  string
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
		if r.err != nil {
			return machine.Result{ExitCode: -1}, r.err
		}
		fmt.Fprint(out, r.output)
		return machine.Result{ExitCode: r.exit}, nil
	}
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

// statusOutput renders what the probe script prints on the machine.
func statusOutput(branch string, baseOK bool, behind, ahead int, ancestor bool) string {
	if !baseOK {
		return fmt.Sprintf("branch=%s\nbase=missing\n", branch)
	}
	yn := "no"
	if ancestor {
		yn = "yes"
	}
	return fmt.Sprintf("branch=%s\nbase=ok\nbehind=%d\nahead=%d\nancestor=%s\n", branch, behind, ahead, yn)
}

func TestInspect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		output       string
		wantBranch   string
		wantBehind   int
		wantAhead    int
		wantBase     bool
		wantAncestor bool
	}{
		{
			name:         "current checkout",
			output:       statusOutput("main", true, 0, 0, true),
			wantBranch:   "main",
			wantBase:     true,
			wantAncestor: true,
		},
		{
			// The real shape of a neglected checkout: an old branch carrying
			// its own commits and missing most of the base.
			name:       "diverged and far behind",
			output:     statusOutput("develop", true, 1550, 433, false),
			wantBranch: "develop",
			wantBehind: 1550,
			wantAhead:  433,
			wantBase:   true,
		},
		{
			name:       "base never fetched",
			output:     statusOutput("main", false, 0, 0, false),
			wantBranch: "main",
		},
		{
			name:         "detached head",
			output:       statusOutput("HEAD", true, 3, 0, true),
			wantBranch:   "HEAD",
			wantBehind:   3,
			wantBase:     true,
			wantAncestor: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			exec := &scriptedExec{rules: []rule{{match: "rev-parse", output: tc.output}}}
			got, err := gitrepo.Inspect(context.Background(), exec, "local", "/srv/app", "origin/main")
			if err != nil {
				t.Fatalf("Inspect() error = %v, want nil", err)
			}
			if got.Branch != tc.wantBranch {
				t.Errorf("Branch = %q, want %q", got.Branch, tc.wantBranch)
			}
			if got.Behind != tc.wantBehind {
				t.Errorf("Behind = %d, want %d", got.Behind, tc.wantBehind)
			}
			if got.Ahead != tc.wantAhead {
				t.Errorf("Ahead = %d, want %d", got.Ahead, tc.wantAhead)
			}
			if got.BaseResolved != tc.wantBase {
				t.Errorf("BaseResolved = %v, want %v", got.BaseResolved, tc.wantBase)
			}
			if got.HeadAncestorOfBase != tc.wantAncestor {
				t.Errorf("HeadAncestorOfBase = %v, want %v", got.HeadAncestorOfBase, tc.wantAncestor)
			}
		})
	}
}

func TestInspectRejectsUnusableOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		rules []rule
	}{
		{name: "empty output", rules: []rule{{match: "rev-parse", output: ""}}},
		{name: "no base line", rules: []rule{{match: "rev-parse", output: "branch=main\n"}}},
		{name: "unparseable count", rules: []rule{{match: "rev-parse", output: "branch=main\nbase=ok\nbehind=lots\n"}}},
		{name: "probe failed", rules: []rule{{match: "rev-parse", exit: 128, output: "not a git repository"}}},
		{name: "executor error", rules: []rule{{match: "rev-parse", err: errors.New("ssh died")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := gitrepo.Inspect(context.Background(), &scriptedExec{rules: tc.rules}, "local", "/srv/app", "origin/main")
			if err == nil {
				t.Error("Inspect() error = nil, want an error rather than a zero Status read as healthy")
			}
		})
	}
}

func TestStatusStaleAndDiverged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		status       gitrepo.Status
		threshold    int
		wantStale    bool
		wantDiverged bool
	}{
		{
			name:      "exactly at the threshold is not stale",
			status:    gitrepo.Status{Behind: 50, HeadAncestorOfBase: true},
			threshold: 50,
		},
		{
			name:      "one past the threshold is stale",
			status:    gitrepo.Status{Behind: 51, HeadAncestorOfBase: true},
			threshold: 50,
			wantStale: true,
		},
		{
			name:      "a zero threshold demands an exactly current checkout",
			status:    gitrepo.Status{Behind: 1, HeadAncestorOfBase: true},
			threshold: 0,
			wantStale: true,
		},
		{
			name:         "carrying commits the base lacks is divergence",
			status:       gitrepo.Status{Behind: 0, Ahead: 3, HeadAncestorOfBase: false},
			threshold:    50,
			wantDiverged: true,
		},
		{
			name:         "the neglected-checkout case is both",
			status:       gitrepo.Status{Behind: 1550, Ahead: 433, HeadAncestorOfBase: false},
			threshold:    50,
			wantStale:    true,
			wantDiverged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.status.Stale(tc.threshold); got != tc.wantStale {
				t.Errorf("Stale(%d) = %v, want %v", tc.threshold, got, tc.wantStale)
			}
			if got := tc.status.Diverged(); got != tc.wantDiverged {
				t.Errorf("Diverged() = %v, want %v", got, tc.wantDiverged)
			}
		})
	}
}

// TestDescribeCarriesTheNumbers pins that a failure message says what is
// actually wrong. "stale checkout" is the message that gets ignored.
func TestDescribeCarriesTheNumbers(t *testing.T) {
	t.Parallel()

	got := gitrepo.Status{Branch: "develop", BaseResolved: true, Behind: 1550, Ahead: 433}.
		Describe("origin/main")
	for _, want := range []string{"develop", "1550 behind", "433 ahead", "origin/main", "not an ancestor"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to contain %q", got, want)
		}
	}

	missing := gitrepo.Status{Branch: "main"}.Describe("origin/trunk")
	if !strings.Contains(missing, "origin/trunk") || !strings.Contains(missing, "does not exist") {
		t.Errorf("Describe() with an unresolved base = %q, want it to name the missing ref", missing)
	}
}

func TestFetch(t *testing.T) {
	t.Parallel()

	exec := &scriptedExec{}
	if err := gitrepo.Fetch(context.Background(), exec, "local", "/srv/app"); err != nil {
		t.Fatalf("Fetch() error = %v, want nil", err)
	}
	if got := exec.scripts(); len(got) != 1 || !strings.Contains(got[0], "git fetch") {
		t.Errorf("Fetch() ran %v, want a git fetch", got)
	}

	failing := &scriptedExec{rules: []rule{{match: "fetch", exit: 128, output: "could not read from remote"}}}
	err := gitrepo.Fetch(context.Background(), failing, "local", "/srv/app")
	if err == nil {
		t.Fatal("Fetch() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "could not read from remote") {
		t.Errorf("Fetch() error = %v, want it to carry git's own message", err)
	}
}

func TestWorktreeNaming(t *testing.T) {
	t.Parallel()

	// Worktrees sit alongside the repo, never inside it: inside, they would
	// show up as untracked files in the tree lanes are working on.
	root := gitrepo.WorktreeRoot("/home/u/Code/org/app", "launch-1")
	if want := "/home/u/Code/org/.sprintd-worktrees/launch-1"; root != want {
		t.Errorf("WorktreeRoot() = %q, want %q", root, want)
	}
	if got, want := gitrepo.WorktreePath("/home/u/Code/org/app", "launch-1", "L1"),
		"/home/u/Code/org/.sprintd-worktrees/launch-1/L1"; got != want {
		t.Errorf("WorktreePath() = %q, want %q", got, want)
	}
	if got, want := gitrepo.BranchName("launch-1", "L1"), "sprintd/launch-1/L1"; got != want {
		t.Errorf("BranchName() = %q, want %q", got, want)
	}
}

func TestEnsureWorktree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		rules       []rule
		wantCreated bool
		wantErr     bool
		wantScript  []string
	}{
		{
			name:        "creates from the base after fetching",
			rules:       []rule{{match: "worktree add", output: "worktree=created\n"}},
			wantCreated: true,
			// The prune matters: git keeps a worktree registered after its
			// directory is gone and then refuses to reuse the branch, so a
			// second run after a manual cleanup would fail without it.
			wantScript: []string{"git fetch", "git worktree prune", "worktree add", "'origin/main'", "'sprintd/s/L1'"},
		},
		{
			name:  "reuses an existing tree so a retry keeps its progress",
			rules: []rule{{match: "worktree add", output: "worktree=reused\n"}},
		},
		{
			name:    "a failed add is an error, not a silent in-place fallback",
			rules:   []rule{{match: "worktree add", exit: 128, output: "fatal: invalid reference: origin/main"}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			exec := &scriptedExec{rules: tc.rules}
			created, err := gitrepo.EnsureWorktree(context.Background(), exec, "local",
				"/srv/app", "/srv/.sprintd-worktrees/s/L1", "sprintd/s/L1", "origin/main")
			if tc.wantErr {
				if err == nil {
					t.Fatal("EnsureWorktree() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureWorktree() error = %v, want nil", err)
			}
			if created != tc.wantCreated {
				t.Errorf("created = %v, want %v", created, tc.wantCreated)
			}
			joined := strings.Join(exec.scripts(), "\n")
			for _, want := range tc.wantScript {
				if !strings.Contains(joined, want) {
					t.Errorf("script missing %q; ran:\n%s", want, joined)
				}
			}
		})
	}
}

// TestEnsureWorktreeRunsInTheRepo pins that the git commands are issued from
// the repository, not from the worktree that does not exist yet.
func TestEnsureWorktreeRunsInTheRepo(t *testing.T) {
	t.Parallel()

	exec := &scriptedExec{rules: []rule{{match: "worktree add", output: "worktree=created\n"}}}
	if _, err := gitrepo.EnsureWorktree(context.Background(), exec, "local",
		"/srv/app", "/srv/wt/L1", "sprintd/s/L1", "origin/main"); err != nil {
		t.Fatalf("EnsureWorktree() error = %v, want nil", err)
	}
	exec.mu.Lock()
	defer exec.mu.Unlock()
	for _, c := range exec.calls {
		if c.Dir != "/srv/app" {
			t.Errorf("command ran in %q, want the repo /srv/app", c.Dir)
		}
	}
}
