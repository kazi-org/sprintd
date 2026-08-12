# Roadmap

## Shipped

- **sprintd v0.1.0 — deadline-bounded lane scheduler with predicate verification** — David Ndungu, 2026-08-12, [#1](https://github.com/kazi-org/sprintd/pull/1). Sprint-file parse/validation (predicate required), local + ssh dispatch capped per repo, stall and deadline watchdog with process-group kill, retry carrying the predicate's own output, account allocator over ccusage with reserve floor and 85% hard stop, per-machine preflight, append-only results.jsonl and status table, goreleaser + homebrew tap + launchd example.
- **v0.1.1–v0.1.3 — packaging fixes** — David Ndungu, 2026-08-12. Archives now ship the examples (goreleaser does not expand `**` and a glob matching nothing is only a warning); the tap formula authenticates release-asset downloads from the private repo and requires its strategy from the tap root. `brew install kazi-org/tap/sprintd` verified from a clean untap, installing 0.1.3.

## In progress

_Nothing._

## In flight (PRs open)

_Nothing._

## Planned

- `brew install` needs `HOMEBREW_GITHUB_API_TOKEN` while the repo is private. Making the repo public would remove that requirement for every user; the source carries no home paths, personal usernames or internal hostnames, so it is publishable as-is. Founder call.
- Per-account usage is read once at the start of a run. If sprints get long enough that an account crosses its floor mid-run, re-sample between lanes.
- `ccusage` only sees transcripts under a config dir on the local machine, so an account's usage on another host is invisible. Aggregating across the three machines would need a shared usage view.

## Blocked

_Nothing._
