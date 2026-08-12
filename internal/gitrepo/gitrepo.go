// Package gitrepo inspects and prepares the git checkouts lanes run against.
//
// It exists because a repo path in a sprint file is not necessarily a usable
// checkout. A tree sitting far behind its base, or on a branch that diverged
// from it, makes a lane read stale source, grep stale CI config and branch
// from the wrong commit -- and the lane reports success while doing it,
// because nothing in its own view looks wrong.
package gitrepo

import (
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/kazi-org/sprintd/internal/machine"
)

// WorktreeDirName is the directory, alongside the repo, that holds lane
// worktrees.
const WorktreeDirName = ".sprintd-worktrees"

// Executor runs a command on the machine holding the repo.
type Executor interface {
	Run(ctx context.Context, cmd machine.Command, out io.Writer) (machine.Result, error)
}

// Status is a checkout's position relative to its base ref.
type Status struct {
	// Branch is the checked-out branch, or "HEAD" when detached.
	Branch string
	// BaseResolved is false when the base ref does not exist in the repo,
	// which usually means it was never fetched or is misspelled.
	BaseResolved bool
	// Behind is the number of commits in base that HEAD does not have.
	Behind int
	// Ahead is the number of commits in HEAD that base does not have.
	Ahead int
	// HeadAncestorOfBase is true when HEAD is contained in base, meaning the
	// checkout carries nothing the base does not already have.
	HeadAncestorOfBase bool
}

// Stale reports whether the checkout is too far behind its base.
func (s Status) Stale(threshold int) bool { return s.Behind > threshold }

// Diverged reports whether the checkout carries commits the base does not.
func (s Status) Diverged() bool { return !s.HeadAncestorOfBase }

// Describe renders the checkout's position with the actual numbers in it, so a
// failure says what is wrong rather than "stale checkout".
func (s Status) Describe(base string) string {
	if !s.BaseResolved {
		return fmt.Sprintf("base %s does not exist in this checkout", base)
	}
	desc := fmt.Sprintf("on %s, %d behind and %d ahead of %s", s.Branch, s.Behind, s.Ahead, base)
	if s.HeadAncestorOfBase {
		return desc
	}
	return desc + ", not an ancestor of it"
}

// statusScript emits key=value lines rather than relying on positional output,
// so a git version that adds a line does not silently shift the parse.
const statusScript = `
printf 'branch=%%s\n' "$(git rev-parse --abbrev-ref HEAD)"
if git rev-parse --verify --quiet %[1]s >/dev/null 2>&1; then
  printf 'base=ok\n'
  counts=$(git rev-list --left-right --count %[1]s...HEAD)
  printf 'behind=%%s\n' "$(printf '%%s' "$counts" | awk '{print $1}')"
  printf 'ahead=%%s\n' "$(printf '%%s' "$counts" | awk '{print $2}')"
  if git merge-base --is-ancestor HEAD %[1]s; then
    printf 'ancestor=yes\n'
  else
    printf 'ancestor=no\n'
  fi
else
  printf 'base=missing\n'
fi
`

// Fetch updates the repo's remote refs. Lanes branch from the base, so the
// base has to be current before anything is dispatched.
func Fetch(ctx context.Context, exec Executor, host, repoPath string) error {
	var out strings.Builder
	res, err := exec.Run(ctx, machine.Command{
		Host: host, Dir: repoPath, Script: "git fetch --quiet --prune",
	}, &out)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", repoPath, err)
	}
	if !res.OK() {
		return fmt.Errorf("fetching %s: git fetch exited %d: %s",
			repoPath, res.ExitCode, firstLine(out.String()))
	}
	return nil
}

// Inspect reports where the checkout sits relative to base.
func Inspect(ctx context.Context, exec Executor, host, repoPath, base string) (Status, error) {
	var out strings.Builder
	res, err := exec.Run(ctx, machine.Command{
		Host: host, Dir: repoPath,
		Script: fmt.Sprintf(statusScript, machine.Quote(base)),
	}, &out)
	if err != nil {
		return Status{}, fmt.Errorf("inspecting %s: %w", repoPath, err)
	}
	if !res.OK() {
		return Status{}, fmt.Errorf("inspecting %s: exited %d: %s",
			repoPath, res.ExitCode, firstLine(out.String()))
	}
	return parseStatus(out.String())
}

func parseStatus(raw string) (Status, error) {
	var s Status
	var sawBase bool
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "branch":
			s.Branch = value
		case "base":
			sawBase = true
			s.BaseResolved = value == "ok"
		case "behind", "ahead":
			n, err := strconv.Atoi(value)
			if err != nil {
				return Status{}, fmt.Errorf("parsing %s count %q: %w", key, value, err)
			}
			if key == "behind" {
				s.Behind = n
			} else {
				s.Ahead = n
			}
		case "ancestor":
			s.HeadAncestorOfBase = value == "yes"
		}
	}
	if s.Branch == "" || !sawBase {
		return Status{}, fmt.Errorf("git status probe returned nothing usable: %q", strings.TrimSpace(raw))
	}
	return s, nil
}

// WorktreeRoot is where a repo's lane worktrees live: alongside the checkout
// rather than inside it, so they never appear as untracked files in the tree
// the lanes are working on.
func WorktreeRoot(repoPath, sprintName string) string {
	return path.Join(path.Dir(repoPath), WorktreeDirName, sprintName)
}

// WorktreePath is one lane's worktree.
func WorktreePath(repoPath, sprintName, laneID string) string {
	return path.Join(WorktreeRoot(repoPath, sprintName), laneID)
}

// BranchName is the branch a lane's worktree is checked out on.
func BranchName(sprintName, laneID string) string {
	return fmt.Sprintf("sprintd/%s/%s", sprintName, laneID)
}

// worktreeScript fetches and then creates the worktree, and is a no-op when
// the worktree already exists so a retry continues in the tree the previous
// attempt left behind rather than discarding its progress.
const worktreeScript = `
set -e
if [ -e %[1]s ]; then
  printf 'worktree=reused\n'
  exit 0
fi
git fetch --quiet --prune
git worktree prune
mkdir -p %[2]s
git worktree add --quiet -B %[3]s %[1]s %[4]s
printf 'worktree=created\n'
`

// EnsureWorktree gives a lane its own working tree, branched from a freshly
// fetched base.
//
// Every lane gets one for two reasons. It is what makes a repo's
// max_concurrent cap above 1 coherent at all: without it, concurrent lanes
// share one working tree, one index and one checked-out branch, and overwrite
// each other's edits. And it decouples the lane from whatever the primary
// checkout happens to be sitting on, so a neglected tree cannot feed a lane
// stale source.
//
// The worktree is never removed automatically. An agent may have left
// uncommitted work in it, and a lane that escalated is exactly the one whose
// tree someone will want to read.
//
// Registrations are pruned first. Git keeps a worktree registered after its
// directory is deleted, marking it prunable, and refuses to reuse the branch
// while that registration stands -- so a second run after anyone cleaned up
// the trees by hand would otherwise fail every lane before dispatch.
func EnsureWorktree(ctx context.Context, exec Executor, host, repoPath, worktreePath, branch, base string) (created bool, err error) {
	var out strings.Builder
	res, err := exec.Run(ctx, machine.Command{
		Host: host, Dir: repoPath,
		Script: fmt.Sprintf(worktreeScript,
			machine.Quote(worktreePath),
			machine.Quote(path.Dir(worktreePath)),
			machine.Quote(branch),
			machine.Quote(base)),
	}, &out)
	if err != nil {
		return false, fmt.Errorf("preparing worktree for %s: %w", repoPath, err)
	}
	if !res.OK() {
		return false, fmt.Errorf("preparing worktree %s from %s: exited %d: %s",
			worktreePath, base, res.ExitCode, firstLine(out.String()))
	}
	return strings.Contains(out.String(), "worktree=created"), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
