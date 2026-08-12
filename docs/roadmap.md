# Roadmap

## Shipped

- **sprintd v0.1.0 — deadline-bounded lane scheduler with predicate verification** — David Ndungu, 2026-08-12, [#1](https://github.com/kazi-org/sprintd/pull/1). Sprint-file parse/validation (predicate required), local + ssh dispatch capped per repo, stall and deadline watchdog with process-group kill, retry carrying the predicate's own output, account allocator over ccusage with reserve floor and 85% hard stop, per-machine preflight, append-only results.jsonl and status table, goreleaser + homebrew tap + launchd example.
- **v0.1.1–v0.1.3 — packaging fixes** — David Ndungu, 2026-08-12. Archives now ship the examples (goreleaser does not expand `**` and a glob matching nothing is only a warning); the tap formula authenticates release-asset downloads from the private repo and requires its strategy from the tap root. `brew install kazi-org/tap/sprintd` verified from a clean untap, installing 0.1.3.

- **sprintd v0.2.0 — stale-checkout gate + per-lane worktrees** — David Ndungu, 2026-08-12, [#2](https://github.com/kazi-org/sprintd/pull/2). Preflight fetches each repo and fails (not warns) when the checkout is behind its `base` by more than `stale_threshold` or has diverged, naming the actual counts. Every lane now runs in its own git worktree branched from a freshly fetched base, which also fixes the shared-working-tree collision that made `max_concurrent > 1` incoherent. Verified against real git repos: a checkout 60 behind fed lanes 61-commit worktrees.

- **sprintd v0.2.1 — public repo, token-free install, predicate docs** — David Ndungu, 2026-08-12, [#3](https://github.com/kazi-org/sprintd/pull/3). Repo made public (history audited clean beforehand), so the tap's private-repository download strategy was removed and `brew install kazi-org/tap/sprintd` now needs no token or GitHub account — verified from a clean untap with every GitHub credential unset. README documents the exit-0 predicate trap, with a verified example: `govulncheck ./...` reports 13 vulnerabilities in this repo and exits 0.

- **sprintd v0.3.0 — a lane can supply its own command** — David Ndungu, 2026-08-12, [#4](https://github.com/kazi-org/sprintd/pull/4). `command:` runs verbatim instead of sprintd composing `claude -p` from a `prompt:`; exactly one of the two is required, checked at parse time. Lets a lane run `kazi apply` so kazi owns convergence while sprintd supplies machine, account, deadline and watchdog. Account pinning still applies to a command lane; preflight skips its claude probes, visibly, for an all-command sprint. Additive — a prompt lane composes byte-for-byte the same command, pinned by a regression test.

- **`needs` semantics recorded and guarded** — David Ndungu, 2026-08-12, [#5](https://github.com/kazi-org/sprintd/pull/5). Docs, example and one test; no release. Ordering-only semantics documented in the README with the deep-chain anti-pattern called out, moved out of Planned into Decided, the example rewritten to show genuine ordering instead of inheritance, and a test that goes red against the rejected branch-from-dependency design.

- **sprintd v0.4.0 — a retried command is told why the last attempt failed** — David Ndungu, 2026-08-12, [#6](https://github.com/kazi-org/sprintd/pull/6). `SPRINTD_ATTEMPT` on every dispatch and `SPRINTD_LAST_FAILURE` on attempts after the first, carrying the failure reason and the predicate's output, bounded, reason line kept whole. Closes the parity gap where a `command:` lane re-ran identically and burned a deadline (and dispatched-agent quota) per retry to reproduce a known failure. Covers the ssh path, where the multi-line value travels through a remote shell.

- **sprintd v0.5.0 — `status --json`, the machine-readable interface** — David Ndungu, 2026-08-12, [#7](https://github.com/kazi-org/sprintd/pull/7) and [#8](https://github.com/kazi-org/sprintd/pull/8). Versioned JSON report so a dashboard never parses `results.jsonl`. Runs write `run.json` at start (before the slow ccusage sample) and finalise it at end, which is what makes an explicit running / finished / interrupted answer possible. Machines report busy or idle with no fabricated utilisation figure; account usage carries `sampled_at` and `sampled_once`. No run at all exits 0 with a `no_run` shape. A log truncated by a kill mid-write drops only its final line and says so.

## In progress

_Nothing._

## In flight (PRs open)

_Nothing._

## Planned

- Worktrees are never removed automatically, so a long-lived machine accumulates them. Deliberate for now (an agent may leave uncommitted work), but a `sprintd prune` that removes only worktrees whose lanes completed and whose branches are merged would be the safe version.
- Per-account usage is read once at the start of a run. If sprints get long enough that an account crosses its floor mid-run, re-sample between lanes.
- `ccusage` only sees transcripts under a config dir on the local machine, so an account's usage on another host is invisible. Aggregating across the three machines would need a shared usage view.

## Decided

Behaviours that were questioned and ruled on. They are settled, not pending — do not rebuild them the other way without reopening the decision.

- **`needs` is ordering only; dependent lanes branch from the base.** 2026-08-12. A lane with `needs` waits for its dependencies to complete, but still branches from the repo's `base` like any other lane, so it sees a dependency's work only once that work has **landed on the base** — not merely when the dependency's predicate passed. Branching a dependent from its dependency's branch was considered and rejected. Rationale: it matches how CI works, and "merged to base" is a stronger completion signal than "the predicate passed" — a lane whose predicate passed but whose work was never reviewed or merged has not really finished. Accepted cost: a chain of dependent lanes stalls unless each one merges, so sprint files should prefer independent lanes and avoid deep `needs` chains. Documented in the README under "How lanes run".

## Blocked

_Nothing._
