# Roadmap

## Shipped

- **sprintd v0.1.0 — deadline-bounded lane scheduler with predicate verification** — David Ndungu, 2026-08-12, [#1](https://github.com/kazi-org/sprintd/pull/1). Sprint-file parse/validation (predicate required), local + ssh dispatch capped per repo, stall and deadline watchdog with process-group kill, retry carrying the predicate's own output, account allocator over ccusage with reserve floor and 85% hard stop, per-machine preflight, append-only results.jsonl and status table, goreleaser + homebrew tap + launchd example.
- **v0.1.1–v0.1.3 — packaging fixes** — David Ndungu, 2026-08-12. Archives now ship the examples (goreleaser does not expand `**` and a glob matching nothing is only a warning); the tap formula authenticates release-asset downloads from the private repo and requires its strategy from the tap root. `brew install kazi-org/tap/sprintd` verified from a clean untap, installing 0.1.3.

- **sprintd v0.2.0 — stale-checkout gate + per-lane worktrees** — David Ndungu, 2026-08-12, [#2](https://github.com/kazi-org/sprintd/pull/2). Preflight fetches each repo and fails (not warns) when the checkout is behind its `base` by more than `stale_threshold` or has diverged, naming the actual counts. Every lane now runs in its own git worktree branched from a freshly fetched base, which also fixes the shared-working-tree collision that made `max_concurrent > 1` incoherent. Verified against real git repos: a checkout 60 behind fed lanes 61-commit worktrees.

- **sprintd v0.2.1 — public repo, token-free install, predicate docs** — David Ndungu, 2026-08-12, [#3](https://github.com/kazi-org/sprintd/pull/3). Repo made public (history audited clean beforehand), so the tap's private-repository download strategy was removed and `brew install kazi-org/tap/sprintd` now needs no token or GitHub account — verified from a clean untap with every GitHub credential unset. README documents the exit-0 predicate trap, with a verified example: `govulncheck ./...` reports 13 vulnerabilities in this repo and exits 0.

## In progress

_Nothing._

## In flight (PRs open)

_Nothing._

## Planned

- Worktrees are never removed automatically, so a long-lived machine accumulates them. Deliberate for now (an agent may leave uncommitted work), but a `sprintd prune` that removes only worktrees whose lanes completed and whose branches are merged would be the safe version.
- `needs` is ordering only: a dependent lane branches from base, so it sees a dependency's work only once that work lands. If dependent lanes need to build on each other directly, that needs a decision about branching from the dependency instead.
- Per-account usage is read once at the start of a run. If sprints get long enough that an account crosses its floor mid-run, re-sample between lanes.
- `ccusage` only sees transcripts under a config dir on the local machine, so an account's usage on another host is invisible. Aggregating across the three machines would need a shared usage view.

## Blocked

_Nothing._
