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

No token or GitHub account is needed.

Or build from source:

```sh
go build -o sprintd .
```

## Commands

```
sprintd preflight --sprint <file>   check every machine can run lanes
sprintd run       --sprint <file>   dispatch the sprint
sprintd status  [--run <dir>] [--json]   show the state of a run
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
5. `claude --version` runs and a trivial `claude -p` returns the expected
   string — once per account that has its own `config_dir`, since that is what
   separates credentials.

Steps 4 and 5 are skipped, visibly, for a sprint whose lanes all declare
`command`: sprintd never invokes claude for such a sprint, and failing it on an
account probe for a tool it does not use would be wrong. One `prompt` lane
anywhere makes them blocking again.

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
    needs: [L0]                       # ordering only — see "How lanes run"

  - id: L2
    repo: app
    goal: "the flake in the payments suite is gone"
    command: "kazi apply goals/payments-flake.toml"   # instead of prompt
    predicate: "./scripts/payments-stable.sh"
    machine: mini
```

### `prompt` or `command`

A lane says **either** what to prompt an agent with, or what command to run.

| Field | sprintd runs |
|---|---|
| `prompt` | `claude -p '<prompt>' --model <model>` |
| `command` | your command, verbatim |

A lane must have exactly one of them. Both, or neither, is rejected at parse
time by lane id — the same discipline as a missing predicate.

`command` exists so a lane can run something that owns its own agent loop
rather than having sprintd compose one. The motivating case is
[kazi](https://github.com/kazi-org/kazi), a reconciliation controller that
already grinds until its own predicates hold:

```yaml
  - id: L3
    repo: app
    goal: "the cold-launch check passes"
    command: "kazi apply goals/cold-launch.toml"
    predicate: "./scripts/cold-launch-check.sh"
    machine: mini
```

For converge-shaped work, sprintd's retry loop and kazi's reconciliation loop
are the same loop. Running the lane as `kazi apply` lets kazi own convergence,
and leaves sprintd supplying only what kazi has no concept of: **which machine,
which account, the deadline, and the liveness watchdog**. The two stay separate
tools rather than merging, because kazi's model — guards, held-out predicates,
drift detection, convergence vectors — is far more than a one-off lane needs,
and forcing that schema onto every lane would be the bad-abstraction case.

Three things to know about `command` lanes:

- **Account pinning still applies.** `CLAUDE_CONFIG_DIR` is set for the lane's
  assigned account whatever the command is. This matters and is not obvious: a
  `kazi apply` lane that dispatches agents internally still spends that
  account's quota, so it has to be pinned and counted like any other lane, or
  the reserve floor protects nothing.
- **A retry passes the previous failure in the environment.** There is no
  prompt to append it to, so it arrives as `SPRINTD_LAST_FAILURE` instead —
  see below.
- **Model is not required, and not used.** A command lane picks its own model,
  or has no concept of one.

The predicate contract is identical either way: its own process, in the lane's
worktree, with no account environment, and exit 0 is the only thing that counts
as complete.

#### What a lane's command is told

Every dispatched command gets these, on top of the account pin:

| Variable | Set when | Contains |
|---|---|---|
| `SPRINTD_ATTEMPT` | always | the 1-based attempt number |
| `SPRINTD_LAST_FAILURE` | attempts after the first | why the previous attempt failed, and the predicate's output |

`SPRINTD_LAST_FAILURE` is absent on the first attempt, so its presence is how a
command tells a retry from a fresh run. Its contents are the same context a
`prompt` lane gets appended to its prompt — the failure reason, then the
predicate's combined output (or the log tail, for a stall or a deadline) —
truncated to a bounded size, keeping the end, which is where the error is.

A command that reads it gets full parity with a prompt lane:

```sh
#!/bin/bash
# scripts/lane.sh
if [ -n "${SPRINTD_LAST_FAILURE:-}" ]; then
  echo "attempt ${SPRINTD_ATTEMPT}, previous failure:"
  echo "$SPRINTD_LAST_FAILURE"
fi
exec kazi apply goals/cold-launch.toml
```

A command that ignores it re-runs identically, which is fine when the command
already reads the world — `kazi apply` re-derives state from the repo every
time, so it does not need to be told what failed. But without reading one or
the other, a lane with `retries: 2` runs the same command three times for the
same result, spending three deadlines, and the quota of any agents it dispatches,
to reproduce a failure that was already known.

A full annotated example is in [`examples/sprint.yaml`](examples/sprint.yaml).

Paths beginning with `~` are expanded against the running user's home
directory, so one sprint file works across machines. Unknown fields are a parse
error, not a silent no-op: a mistyped `predicat:` would otherwise remove the
only thing that makes the lane real.

### Validation

The sprint is rejected, naming the lane, if any lane has no predicate, no goal,
neither a prompt nor a command, both a prompt and a command, an unknown repo or
machine, an unknown or self-referential
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
- **`needs` is ordering, not code inheritance** — see below. A dependent lane
  branches from the base like any other.
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
escalates, the dependent escalates too rather than running against a foundation
that was never laid.

### `needs` is ordering, not code inheritance

This is the one thing about `needs` that surprises people, so it is worth being
blunt about.

A lane with `needs` waits for its dependencies to complete, and then **branches
from the repo's `base` like every other lane**. It does not branch from its
dependency. So it sees a dependency's work only once that work has **landed on
the base** — not merely once the dependency's predicate passed.

The base is re-fetched when each lane's worktree is created, so a dependency
merged part-way through a sprint *is* picked up by a lane that starts after the
merge.

If you are wondering why your dependent lane cannot see the fix the lane before
it made: that is why. The fix is on a `sprintd/<sprint>/<lane>` branch, and
nothing has merged it.

This is deliberate. It matches how CI works, and "merged to the base" is a
stronger signal of completion than "the predicate passed" — a lane whose
predicate passed but whose work was never reviewed or merged has not really
finished, and a dependent built on top of it would be building on something
nobody accepted.

**The consequence: deep `needs` chains are an anti-pattern in a sprint file.**
A chain of dependent lanes stalls unless each link merges to the base before
the next one starts, and sprintd does not merge anything. Prefer independent
lanes. Use `needs` for genuine ordering — work that must not overlap, or a lane
that should not start until an earlier one has been dealt with — and keep the
chains shallow. If two pieces of work truly build on each other, they are
usually one lane.

**Watchdog.** A lane that writes nothing for `--stall` is killed. A lane that
runs past its deadline is killed. Either way the reason is attached to the next
attempt's prompt, and the lane is requeued. When `retries` runs out the lane
becomes `escalated` with its last failure output preserved.

**Verification.** See the predicate discipline above.

## Writing predicates that actually fail

**sprintd passes a predicate on exit 0 and nothing else.** It does not read the
predicate's output, and it has no verdict modes. That keeps the contract to one
command, but it puts one obligation on you: **the predicate script decides the
verdict itself and exits non-zero. Never trust the exit code of the tool it
wraps.**

Plenty of tools report problems and still exit 0. That is not hypothetical —
here is this repository, scanned by a tool many people would wrap directly:

```console
$ govulncheck ./... ; echo "exit=$?"
Your code is affected by 0 vulnerabilities.
This scan also found 1 vulnerability in packages you import and 12
vulnerabilities in modules you require, but your code doesn't appear to call
these vulnerabilities.
exit=0
```

Thirteen reported vulnerabilities, exit 0. A lane dispatched to deal with them
whose predicate is the bare command would be marked **complete** with every one
of them still there — the exact false completion this tool exists to prevent,
reintroduced through a predicate its author believed was safe.

Wrong:

```yaml
predicate: "govulncheck ./..."
```

Right — the wrapper inspects the findings and decides:

```yaml
predicate: "./scripts/no-vulns.sh"
```

```sh
#!/bin/bash
# Exit non-zero if the scan reports anything at all, whatever the tool's own
# exit code says.
set -euo pipefail
found=$(govulncheck -format json ./... | grep -c '"osv"' || true)
if [ "$found" -gt 0 ]; then
  echo "FAIL: govulncheck reported $found vulnerability records"
  exit 1
fi
echo "OK: no vulnerability records"
```

Know the shape so you recognise it. `trivy` exits 0 on findings unless you pass
`--exit-code`; `grype` exits 0 unless you pass `--fail-on`; `govulncheck` exits
0 in `-format json` mode regardless, and, as above, also exits 0 in its default
mode for vulnerabilities your code does not call. Linters, coverage tools and
report generators behave the same way.

The reliable habit: **run your predicate by hand against a known-broken tree
before you put it in a sprint file, and confirm it exits non-zero.** A
predicate that has never failed has never been tested.

### If you need more than exit 0

Some checks want more than a single exit code — structured evidence, flake
tolerance, guards against a lane satisfying the letter of a check, or a loop
that converges rather than a one-shot verdict. sprintd deliberately does not
grow those; it stays a scheduler with a one-command contract.

Point at something that does. [kazi](https://github.com/kazi-org/kazi) is a
reconciliation controller built around exactly that problem, and a lane can use
it from either side with no change to either tool:

```yaml
# kazi does the work and owns convergence; sprintd supplies the machine, the
# account, the deadline and the watchdog.
command: "kazi apply goals/cold-launch.toml"
predicate: "./scripts/cold-launch-check.sh"
```

```yaml
# or: an ordinary prompt lane, with kazi asked for the verdict.
prompt: "…"
predicate: "kazi status --goal cold-launch --exit-code"
```

Either way sprintd decides completion the same way — by the predicate's exit
status, observed from outside the lane.

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

**Before a multi-account sprint can run, each account's `config_dir` has to
exist and be logged in once.** A `CLAUDE_CONFIG_DIR` is just a directory;
pointing at an empty one does not create an account. Run `claude` once per
directory and complete the login:

```sh
CLAUDE_CONFIG_DIR=~/.claude-secondary claude   # then /login
```

Skip this and lanes pinned to that account fail on the first dispatch.
Preflight catches it — that is what its per-account `claude -p` probe is for —
but it is worth doing before you ever get there.

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

With no `--run`, it reads the most recent run under `.sprintd`.

```
sprint: example-launch-2026-08-12  lanes: 3

LANE  STATE      ATTEMPT  MACHINE  ACCOUNT    MODEL   TOOK   REASON
L1    complete   2        mini     primary    opus    14m2s  predicate passed
L2    complete   1        builder  secondary  sonnet  6m11s  predicate passed
L3    escalated  3        mini     secondary  sonnet  31m8s  3 attempts exhausted; last …

complete: 2  escalated: 1  in flight: 0
```

### `sprintd status --json` — the machine-readable interface

```sh
sprintd status --json                 # the most recent run under .sprintd
sprintd status --json --run <dir>     # a specific run
```

This exists so nothing has to parse `results.jsonl`. That file is an
append-only event log whose record shapes belong to sprintd and will change;
anything reading it would break silently. **This command is the supported
interface** — a stable, versioned shape that a dashboard or monitor can depend
on.

`schema_version` is the contract. It is `1` today. It will be raised only when
a field is removed or repurposed; adding a field is not a break. A consumer
should refuse a version it does not recognise rather than guess.

**Exit status:** `0` normally, `2` if any lane escalated, `1` on a real error.
The JSON is written to stdout in every case except a real error, so a consumer
can read the payload and treat `2` as data rather than failure.

#### `status` is the answer to "is anything running"

```
no_run       no run was found at all — a healthy answer, not an error
running      a sprintd process is alive and still working
finished     the run reached its own end and recorded it
interrupted  the run never recorded an end and its process is gone
unknown      liveness could not be determined
```

This is a recorded fact, never inferred from timestamps. A run writes
`run.json` when it starts and rewrites it with a completion time when it ends;
`running` means that file has no completion **and** the recorded process id
still exists. So a sprint killed mid-flight reads `interrupted`, not `running`,
and never renders as live when it is not.

Two honest limits, both carried in the payload's `notes`:

- Liveness is a check on a recorded process id. A reused pid could in principle
  read as running. There is no cheaper check that is more truthful.
- A run directory read on a different machine from the one that produced it
  reports `unknown`, because the process cannot be checked from there.

A run killed exactly mid-write to `results.jsonl` leaves one truncated line at
the end of the file — everything recorded before it was already flushed. That
line is dropped rather than failing the whole report; a note says so, and the
lane it belonged to falls back to whatever the manifest declared for it
(`pending`, if it had never resolved before). A malformed line anywhere else in
the file is real corruption, not a partial write, and still fails.

#### What the payload contains

- `sprint` — name, `opened`/`closes` from the sprint file, run directory, start
  and completion times, elapsed seconds.
- `lanes` — every lane the sprint declared, including ones that never started.
  Per lane: repo, machine, account, model or command, state, whether that state
  is terminal, attempt number, dispatched and resolved timestamps, duration,
  reason, the captured predicate output for terminal failures, and the worktree
  and branch holding the work.
- `totals` — lanes, dispatched, converged, escalated, unresolved, and counts by
  state. A consumer should not have to derive these.
- `machines` — every declared machine, and for each whether it is `busy`, which
  lanes are running on it, and which lanes are unresolved. **A machine with
  nothing on it appears, idle, rather than being absent.** There is
  deliberately no utilisation percentage: sprintd knows what it dispatched
  where, it does not measure load, and a number it cannot observe would be
  worse on a dashboard than no number.
- `accounts` — each account's weekly usage **as sampled once when the run
  started**, with `sampled_at` and `sampled_once: true`. sprintd does not
  refresh this during a run, so a consumer must not present it as live.
- `notes` — caveats that apply to this payload, meant to be rendered.

`lanes`, `machines` and `accounts` are always arrays, never `null`, so a
consumer can iterate without a nil check.

#### Reporting never mutates

`status` only reads. It does not touch a worktree, write to the run directory,
or alter a run in any way, so it is safe to poll and safe to run against a
sprint that is live.

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
