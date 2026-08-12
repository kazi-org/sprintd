# sprintd

A dumb, token-free scheduler that dispatches deadline-bounded `claude -p` lanes
across machines, watchdogs them, and verifies each one with an acceptance
predicate.

It exists because a coordinating agent session doing dispatch in a tick loop
burns frontier-model quota to discover that nothing changed, and goes dark when
it runs out. sprintd is a single Go binary. It costs nothing to wait.

## The predicate discipline

**A lane is complete only when a separate process observes its acceptance
predicate pass.** Never when the session doing the work reports success.

This is the whole point of the tool, so it is enforced structurally rather than
by convention:

- `predicate` is required on every lane. A sprint file with a lane that lacks
  one is rejected at parse time, by lane id, before anything is dispatched.
- A lane whose process exits `0` but whose predicate fails is **not complete**.
  It is requeued, and if attempts run out it escalates.
- The predicate runs as its own command, in the repo, with no account
  credentials attached — it is a check, not agent work, and must not consume
  the quota it exists to protect.
- When a predicate fails, its combined output is attached to the next attempt's
  prompt. The retry sees exactly which observable condition it has to make
  true, in the words of the check that will judge it again.

The failure this prevents is concrete: a lane can merge what looks like a fix,
report success, and leave the symptom untouched for many builds afterwards
because nothing but the lane itself ever checked. A lane without an observable
predicate is not a lane.

## Install

```sh
brew install kazi-org/tap/sprintd
```

Or build from source:

```sh
go build -o sprintd .
```

> The tap formula downloads release assets from this repository. While the
> repository is private, `brew` needs a GitHub token with access to it:
> `export HOMEBREW_GITHUB_API_TOKEN=$(gh auth token)` before installing.

## Commands

```
sprintd preflight --sprint <file>   check every machine can run lanes
sprintd run       --sprint <file>   dispatch the sprint
sprintd status    --run <dir>       show the state of a run
sprintd version                     print the version
```

That is the whole CLI.

### Exit statuses

| Code | Meaning |
|------|---------|
| 0 | every lane verified |
| 1 | sprintd itself failed (bad sprint file, unwritable run directory) |
| 2 | the sprint ran and at least one lane escalated |
| 3 | preflight failed, so nothing was dispatched |

A supervisor should treat 2 as "look at the run directory", not as a crash.

### `sprintd preflight`

Checks every machine any lane targets, in parallel, inside a per-machine budget
(`--budget`, default 60s):

1. the machine is reachable (`ssh` with `BatchMode`, so a password prompt is a
   failure rather than a hang),
2. each repo path exists and is a git checkout,
3. `git fetch` works there, and the checkout is actually usable — see below,
4. `claude --version` runs,
5. a trivial `claude -p` returns the expected string — once per account that
   has its own `config_dir`, since that is what separates credentials.

Step 5 is graded on the output, not just the exit status. An agent that exits
`0` and says nothing is the locked-keychain shape, and it is exactly what
would otherwise burn twenty lanes' worth of deadline doing nothing.

#### The checkout gate

**A repo path in a sprint file is not necessarily a usable checkout.** After
fetching, preflight reports where the tree sits relative to the repo's `base`
and **fails** — not warns — when either:

- it is behind the base by more than `stale_threshold` (default 50), or
- it carries commits the base does not, meaning the branch has diverged.

A checkout left on an old branch makes a lane read stale source, grep stale CI
config and reason about a codebase that no longer exists — and the lane reports
success while doing it, because nothing in its own view looks wrong. That
failure is silent and it is expensive, which is why a warning is not enough.

The message carries the numbers rather than the words "stale checkout":

```
here  local  checkout app  FAIL  39ms  on develop, 60 behind and 1 ahead of origin/main, not an ancestor of it
```

Use `--force` to dispatch anyway, or raise `stale_threshold` for a repo where
being behind is expected.

`sprintd run` runs preflight first and refuses to dispatch if it fails. Use
`--force` to dispatch anyway, or `--skip-preflight` to skip it.

### `sprintd run`

| Flag | Default | Meaning |
|------|---------|---------|
| `--sprint` | required | path to the sprint file |
| `--run-dir` | `.sprintd/<sprint>-<timestamp>` | where logs, heartbeats and results go |
| `--stall` | `10m` | kill a lane that produces no output for this long |
| `--predicate-timeout` | `10m` | bound on a single predicate run |
| `--poll` | `5s` | how often the watchdog samples lane activity |
| `--force` | off | dispatch even if preflight fails |
| `--skip-preflight` | off | do not run preflight at all |
| `--ccusage` | auto | command used to read account usage |

## Sprint file reference

```yaml
sprint: example-launch-2026-08-12     # required
opened: 2026-08-12T08:00:00Z          # optional
closes: 2026-08-12T20:00:00Z          # optional; bounds the whole run

defaults:                             # applied to any lane that omits them
  model: sonnet
  deadline: 90m
  retries: 2

machines:
  mini:    { host: local }            # "local" means no ssh
  builder: { host: user@10.0.0.2 }    # anything else is an ssh destination

accounts:
  - name: primary
    reserve_floor_pct: 30             # never dispatch below this much remaining
    weekly_token_limit: 500000000     # your plan's weekly entitlement
  - name: secondary
    reserve_floor_pct: 0
    config_dir: ~/.claude-secondary   # CLAUDE_CONFIG_DIR for this account
    weekly_token_limit: 500000000

repos:
  app:
    path: ~/Code/example-org/app
    max_concurrent: 4                 # lanes at once in this repo
    base: origin/main                 # what lanes branch from (default)
    stale_threshold: 50               # commits behind base before preflight fails

lanes:
  - id: L1                            # unique
    repo: app                         # must name a declared repo
    goal: "the double prompt is gone" # what "done" means, in one line
    prompt: "…"                       # the claude -p prompt
    predicate: "./scripts/check.sh"   # REQUIRED. exit 0 means complete
    machine: mini                     # must name a declared machine
    model: opus                       # optional; overrides defaults
    deadline: 90m                     # optional; overrides defaults
    retries: 1                        # optional; overrides defaults
    needs: [L0]                       # lanes that must complete first
```

A full annotated example is in [`examples/sprint.yaml`](examples/sprint.yaml).

Paths beginning with `~` are expanded against the running user's home
directory, so one sprint file works across machines. Unknown fields are a parse
error, not a silent no-op: a mistyped `predicat:` would otherwise remove the
only thing that makes the lane real.

### Validation

The sprint is rejected, naming the lane, if any lane has no predicate, no
prompt, no goal, an unknown repo or machine, an unknown or self-referential
`needs`, a duplicate id, or no resolvable model or deadline. `needs` cycles are
rejected too, as is a repo with a `max_concurrent` below 1 or a negative
`stale_threshold`, an account with a `reserve_floor_pct` outside 0–100, and two
accounts sharing a `config_dir` — their credentials could not be told apart at
dispatch.

Lane ids and the sprint name must be usable in a branch name (letters, digits,
dot, dash, underscore), because both become part of one.

## How lanes run

**Every lane gets its own git worktree**, branched from the repo's `base` after
a fresh fetch, at `<repo>/../.sprintd-worktrees/<sprint>/<lane>` on the branch
`sprintd/<sprint>/<lane>`.

This is not a convenience. It is what makes a `max_concurrent` above 1 coherent
at all: without it, concurrent lanes would share one working tree, one index
and one checked-out branch, and would silently overwrite each other's edits. It
also decouples a lane from whatever the primary checkout happens to be sitting
on, so a neglected tree cannot feed it stale source.

Three consequences worth knowing:

- **Write predicates relative to the repo root.** A predicate is run inside the
  lane's worktree, so `./scripts/check.sh` checks the lane's work. An absolute
  path such as `cd ~/Code/org/app && ./scripts/check.sh` escapes the worktree
  and verifies the wrong tree — it would pass or fail on code the lane never
  touched.
- **`needs` is ordering, not code inheritance.** A dependent lane branches from
  the base like any other, so it sees its dependency's work only once that work
  has landed on the base. The base is re-fetched when each lane's worktree is
  created, so a dependency merged mid-sprint is picked up.
- **Worktrees are never removed automatically.** An agent may have left
  uncommitted work in one, and an escalated lane's tree is exactly the one
  someone will want to read. Their paths and branches are recorded in
  `results.jsonl`. Clean them up with `git worktree remove` (or delete the
  directory and run `git worktree prune`) once the work is merged or discarded.

**Dispatch.** `claude -p '<prompt>' --model <model>` in the lane's worktree,
locally or over ssh. The deadline is enforced in-process, not by wrapping the
command in `timeout(1)` — macOS does not ship `timeout`, and one of the target
machines is a Mac. Cancelling kills the child's whole process group, so an
agent that forked a build or a test runner does not outlive its own lane.

**Concurrency** is capped per repo by `max_concurrent`, and by nothing else.
The scarce resource is mergeable changes in one repo, not CPU or memory, so
there is deliberately no load-based throttling.

**Ordering.** A lane waits for every id in its `needs`. If one of them
escalates, the dependent escalates too rather than running against a
foundation that was never laid.

**Watchdog.** A lane that writes nothing for `--stall` is killed. A lane that
runs past its deadline is killed. Either way the reason is attached to the next
attempt's prompt, and the lane is requeued. When `retries` runs out the lane
becomes `escalated` with its last failure output preserved.

**Verification.** See the predicate discipline above.

## Account allocation

sprintd reads per-account usage by shelling out to
[`ccusage`](https://github.com/ryoppippi/ccusage), pointing `CLAUDE_CONFIG_DIR`
at each account's directory.

- An account is skipped once it is at or past **85% of its weekly limit**,
  whatever its floor says.
- An account is skipped while its remaining weekly percentage is at or below
  its `reserve_floor_pct`. That is what keeps a coordinator's own account
  usable.
- Among eligible accounts the least-consumed goes first, with ties broken by
  how many lanes each already took, so a sprint spreads rather than piling on
  one account.
- If `ccusage` is missing or its output cannot be parsed, sprintd logs a clear
  warning and falls back to round-robin rather than failing. The floor and the
  hard stop cannot be enforced in that state, and the log says so.
- If every account is exhausted, the lane escalates with that reason. It is
  never silently dispatched anyway.

Two limits are worth knowing. Usage is sampled once at the start of a run, not
before every lane — a sprint is minutes to hours long and the floor protects a
reserve rather than metering to the token. And `ccusage` aggregates the
transcripts under a config directory on **this machine**, so usage incurred by
the same account on another host is invisible to it.

## Output

The run directory holds everything:

```
<run-dir>/
  results.jsonl              append-only, one record per state transition
  logs/<lane>.attempt-N.log  each attempt's combined output
  heartbeats/<lane>.json     last activity, idle seconds, bytes produced
```

`results.jsonl` records `dispatched`, `stalled`, `killed`, `requeued`,
`predicate_failed`, `complete` and `escalated`, each with a timestamp, machine,
account, model, attempt number, and the lane's worktree and branch. Terminal records carry the predicate output,
so a failure can be read without re-running anything. Each record is flushed as
it is written, so a killed sprintd still leaves a complete history.

```sh
sprintd status --run .sprintd/example-launch-2026-08-12-20260812T090000Z
```

```
sprint: example-launch-2026-08-12  lanes: 3

LANE  STATE      ATTEMPT  MACHINE  ACCOUNT    MODEL   TOOK   REASON
L1    complete   2        mini     primary    opus    14m2s  predicate passed
L2    complete   1        builder  secondary  sonnet  6m11s  predicate passed
L3    escalated  3        mini     secondary  sonnet  31m8s  3 attempts exhausted; last …

complete: 2  escalated: 1  in flight: 0
```

## Running supervised

[`examples/com.kazi.sprintd.plist`](examples/com.kazi.sprintd.plist) is a
launchd agent. It runs in a GUI session rather than as a system daemon so it
can reach the login keychain holding the Claude credentials, and it does not
restart on exit: a sprint is a bounded job with a close time, and restarting it
would re-dispatch lanes that already escalated.

## What this is not

No database — files only. No web UI, no TUI, no plugin system. One binary, one
input file, one run directory.

## Development

```sh
go test -race ./...
go vet ./...
goreleaser release --snapshot --clean
```

## License

MIT. See [LICENSE](LICENSE).
