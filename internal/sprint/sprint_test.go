package sprint_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kazi-org/sprintd/internal/sprint"
)

// validSprint is the baseline every negative case mutates one field of.
const validSprint = `
sprint: demo
opened: 2026-08-12T08:00:00Z
closes: 2026-08-12T20:00:00Z
defaults:
  model: sonnet
  deadline: 90m
  retries: 2
machines:
  mini: { host: local }
  dgx:  { host: user@10.0.0.2 }
accounts:
  - { name: primary, reserve_floor_pct: 30, weekly_token_limit: 1000 }
  - { name: secondary, reserve_floor_pct: 0, weekly_token_limit: 1000, config_dir: /tmp/secondary }
repos:
  app:  { path: /srv/app, max_concurrent: 4 }
lanes:
  - id: L1
    repo: app
    goal: "the thing is gone"
    prompt: "make the thing go away"
    predicate: "./check.sh"
    machine: mini
  - id: L2
    repo: app
    goal: "the other thing"
    prompt: "do the other thing"
    predicate: "./check2.sh"
    machine: dgx
    model: opus
    deadline: 30m
    needs: [L1]
`

func TestParseValidSprint(t *testing.T) {
	t.Parallel()

	s, err := sprint.Parse([]byte(validSprint))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if s.Name != "demo" {
		t.Errorf("Name = %q, want demo", s.Name)
	}
	if got, want := len(s.Lanes), 2; got != want {
		t.Fatalf("len(Lanes) = %d, want %d", got, want)
	}

	lanes := s.ResolvedLanes()
	if got, want := lanes[0].Model, "sonnet"; got != want {
		t.Errorf("L1 model = %q, want %q (from defaults)", got, want)
	}
	if got, want := lanes[0].Timeout, 90*time.Minute; got != want {
		t.Errorf("L1 timeout = %v, want %v (from defaults)", got, want)
	}
	if got, want := lanes[0].Attempts, 3; got != want {
		t.Errorf("L1 attempts = %d, want %d (retries 2 + 1)", got, want)
	}
	if got, want := lanes[0].Host, "local"; got != want {
		t.Errorf("L1 host = %q, want %q", got, want)
	}
	if got, want := lanes[0].MaxConcurrent, 4; got != want {
		t.Errorf("L1 max_concurrent = %d, want %d", got, want)
	}
	if got, want := lanes[1].Model, "opus"; got != want {
		t.Errorf("L2 model = %q, want %q (lane overrides default)", got, want)
	}
	if got, want := lanes[1].Timeout, 30*time.Minute; got != want {
		t.Errorf("L2 timeout = %v, want %v (lane overrides default)", got, want)
	}
	if got, want := lanes[1].Host, "user@10.0.0.2"; got != want {
		t.Errorf("L2 host = %q, want %q", got, want)
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{
			name:    "lane without a predicate",
			mutate:  func(s string) string { return strings.Replace(s, `    predicate: "./check.sh"`+"\n", "", 1) },
			wantSub: "lane L1 has no predicate",
		},
		{
			name: "lane with a blank predicate",
			mutate: func(s string) string {
				return strings.Replace(s, `predicate: "./check.sh"`, `predicate: "   "`, 1)
			},
			wantSub: "lane L1 has no predicate",
		},
		{
			name:    "lane with an empty prompt",
			mutate:  func(s string) string { return strings.Replace(s, `prompt: "make the thing go away"`, `prompt: ""`, 1) },
			wantSub: "lane L1 has an empty prompt",
		},
		{
			name:    "lane with an empty goal",
			mutate:  func(s string) string { return strings.Replace(s, `goal: "the thing is gone"`, `goal: ""`, 1) },
			wantSub: "lane L1 has an empty goal",
		},
		{
			name:    "lane referencing an unknown repo",
			mutate:  func(s string) string { return strings.Replace(s, "    repo: app\n", "    repo: nope\n", 1) },
			wantSub: `lane L1 references unknown repo "nope"`,
		},
		{
			name:    "lane referencing an unknown machine",
			mutate:  func(s string) string { return strings.Replace(s, "machine: mini", "machine: nope", 1) },
			wantSub: `lane L1 references unknown machine "nope"`,
		},
		{
			name:    "lane needing an unknown lane",
			mutate:  func(s string) string { return strings.Replace(s, "needs: [L1]", "needs: [L9]", 1) },
			wantSub: `lane L2 needs unknown lane "L9"`,
		},
		{
			name:    "lane depending on itself",
			mutate:  func(s string) string { return strings.Replace(s, "needs: [L1]", "needs: [L2]", 1) },
			wantSub: "lane L2 depends on itself",
		},
		{
			name: "needs cycle",
			mutate: func(s string) string {
				return strings.Replace(s, `    predicate: "./check.sh"`,
					"    predicate: \"./check.sh\"\n    needs: [L2]", 1)
			},
			wantSub: "needs cycle",
		},
		{
			name:    "duplicate lane id",
			mutate:  func(s string) string { return strings.Replace(s, "id: L2", "id: L1", 1) },
			wantSub: "duplicate lane id L1",
		},
		{
			name:    "repo with a zero concurrency cap",
			mutate:  func(s string) string { return strings.Replace(s, "max_concurrent: 4", "max_concurrent: 0", 1) },
			wantSub: "repo app has max_concurrent 0",
		},
		{
			name:    "no accounts",
			mutate:  func(s string) string { return cutBlock(s, "accounts:", "repos:") },
			wantSub: "no accounts declared",
		},
		{
			name: "accounts sharing a config dir",
			mutate: func(s string) string {
				return strings.Replace(s, ", config_dir: /tmp/secondary", "", 1)
			},
			wantSub: "cannot be told apart",
		},
		{
			name: "reserve floor out of range",
			mutate: func(s string) string {
				return strings.Replace(s, "reserve_floor_pct: 30", "reserve_floor_pct: 140", 1)
			},
			wantSub: "reserve_floor_pct 140.0",
		},
		{
			name:    "closes before opens",
			mutate:  func(s string) string { return strings.Replace(s, "closes: 2026-08-12T20", "closes: 2026-08-12T07", 1) },
			wantSub: "is not after opened",
		},
		{
			name:    "no model anywhere",
			mutate:  func(s string) string { return strings.Replace(s, "  model: sonnet\n", "", 1) },
			wantSub: "lane L1 has no model",
		},
		{
			name:    "no deadline anywhere",
			mutate:  func(s string) string { return strings.Replace(s, "  deadline: 90m\n", "", 1) },
			wantSub: "lane L1 has deadline 0s",
		},
		{
			name:    "empty sprint name",
			mutate:  func(s string) string { return strings.Replace(s, "sprint: demo", `sprint: ""`, 1) },
			wantSub: "sprint name is empty",
		},
		{
			name: "negative stale threshold",
			mutate: func(s string) string {
				return strings.Replace(s, "max_concurrent: 4", "max_concurrent: 4, stale_threshold: -1", 1)
			},
			wantSub: "repo app has stale_threshold -1",
		},
		{
			name:    "lane id that cannot be a branch name",
			mutate:  func(s string) string { return strings.Replace(s, "id: L1", `id: "L 1"`, 1) },
			wantSub: "lane id \"L 1\" is not usable in a branch name",
		},
		{
			name:    "sprint name that cannot be a branch name",
			mutate:  func(s string) string { return strings.Replace(s, "sprint: demo", "sprint: my demo", 1) },
			wantSub: "sprint name \"my demo\" is not usable in a branch name",
		},
		{
			name:    "unknown field",
			mutate:  func(s string) string { return strings.Replace(s, "sprint: demo", "sprint: demo\nmystery: 1", 1) },
			wantSub: "field mystery not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := sprint.Parse([]byte(tc.mutate(validSprint)))
			if err == nil {
				t.Fatalf("Parse() error = nil, want an error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Parse() error = %q, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestMissingPredicateIsInvalidSprint pins the sentinel so callers can branch
// on a bad sprint file rather than on message text.
func TestMissingPredicateIsInvalidSprint(t *testing.T) {
	t.Parallel()

	broken := strings.Replace(validSprint, `    predicate: "./check.sh"`+"\n", "", 1)
	_, err := sprint.Parse([]byte(broken))
	if !errors.Is(err, sprint.ErrInvalid) {
		t.Fatalf("Parse() error = %v, want it to wrap ErrInvalid", err)
	}
}

func TestExpandsHomeInPaths(t *testing.T) {
	// No t.Parallel: t.Setenv rewrites process-wide state.
	home := t.TempDir()
	t.Setenv("HOME", home)

	src := strings.Replace(validSprint, "path: /srv/app", "path: ~/Code/app", 1)
	s, err := sprint.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if got, want := s.Repos["app"].Path, home+"/Code/app"; got != want {
		t.Errorf("repo path = %q, want %q", got, want)
	}
}

func TestMachineRepos(t *testing.T) {
	t.Parallel()

	s, err := sprint.Parse([]byte(validSprint))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	got := s.MachineRepos()
	for _, name := range []string{"mini", "dgx"} {
		targets := got[name]
		if len(targets) != 1 {
			t.Fatalf("MachineRepos()[%s] = %v, want one repo", name, targets)
		}
		if targets[0].Path != "/srv/app" || targets[0].Name != "app" {
			t.Errorf("MachineRepos()[%s][0] = %+v, want repo app at /srv/app", name, targets[0])
		}
		if targets[0].Base != sprint.DefaultBase {
			t.Errorf("base = %q, want the default %q", targets[0].Base, sprint.DefaultBase)
		}
		if targets[0].StaleThreshold != sprint.DefaultStaleThreshold {
			t.Errorf("stale threshold = %d, want the default %d",
				targets[0].StaleThreshold, sprint.DefaultStaleThreshold)
		}
	}
}

// cutBlock removes the lines from the line beginning with start up to, but not
// including, the line beginning with end.
func cutBlock(s, start, end string) string {
	lines := strings.Split(s, "\n")
	var out []string
	dropping := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, start):
			dropping = true
		case strings.HasPrefix(line, end):
			dropping = false
		}
		if !dropping {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func TestRepoBaseAndStaleDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		repoLine      string
		wantBase      string
		wantThreshold int
	}{
		{
			name:          "unset takes the defaults",
			repoLine:      "app:  { path: /srv/app, max_concurrent: 4 }",
			wantBase:      sprint.DefaultBase,
			wantThreshold: sprint.DefaultStaleThreshold,
		},
		{
			name:          "declared values win",
			repoLine:      "app:  { path: /srv/app, max_concurrent: 4, base: origin/trunk, stale_threshold: 5 }",
			wantBase:      "origin/trunk",
			wantThreshold: 5,
		},
		{
			// Zero has to survive: it means the checkout must be exactly
			// current, which is a legitimate thing to demand.
			name:          "an explicit zero threshold is not treated as unset",
			repoLine:      "app:  { path: /srv/app, max_concurrent: 4, stale_threshold: 0 }",
			wantBase:      sprint.DefaultBase,
			wantThreshold: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src := strings.Replace(validSprint, "app:  { path: /srv/app, max_concurrent: 4 }", tc.repoLine, 1)
			s, err := sprint.Parse([]byte(src))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			repo := s.Repos["app"]
			if got := repo.BaseRef(); got != tc.wantBase {
				t.Errorf("BaseRef() = %q, want %q", got, tc.wantBase)
			}
			if got := repo.StaleLimit(); got != tc.wantThreshold {
				t.Errorf("StaleLimit() = %d, want %d", got, tc.wantThreshold)
			}
			if got := s.ResolvedLanes()[0].Base; got != tc.wantBase {
				t.Errorf("resolved lane base = %q, want %q", got, tc.wantBase)
			}
		})
	}
}
