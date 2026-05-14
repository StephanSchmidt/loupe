# Loupe

Diagnostic CLI that measures the impact of AI coding assistants on engineering teams. Indexes Bitbucket Cloud and Jira Cloud via REST APIs, detects AI-assisted commits, and renders a reveal.js slide deck a CTO can present in their next exec meeting.

Not an analytics platform. Closer in shape to `lighthouse` or `npm audit` — run once for a baseline, run weekly to track impact. No SaaS, no login, no data leaves your environment.

The full design brief lives in [`loupe-brief.md`](loupe-brief.md).

## Status

**v0 — usable for an honest baseline, not yet feature-complete.**

Working:
- Bitbucket Cloud and GitHub for git, Jira Cloud and GitHub Issues for tracking — all behind `githost.GitHost` / `tracker.Tracker` interfaces (GitHub Enterprise Server not yet supported)
- Co-Authored-By trailer detection (Claude, Aider, Copilot, Cursor)
- ISO-week aggregates plus automatic AI-adoption cutover detection (config override supported)
- reveal.js deck with weekly throughput (human vs AI) and adoption % charts, plus PNG/SVG exports under `reports/<run>/charts/`

Not yet:
- `loupe run` (weekly incremental) — stub
- `loupe export` (static HTML / PDF) — stub
- End-to-end cycle time (ticket → merged) — the headline chart from the brief
- Per-team breakdown, quality counterweight (defects, churn)
- Squash-merge trailer recovery via PR-commit lookup
- GitLab / Linear providers, GitHub Enterprise Server — one package + one switch case each

## Install

```bash
git clone https://github.com/StephanSchmidt/loupe.git
cd loupe
make build        # produces ./bin/loupe
# or
go install ./cmd/loupe
```

Homebrew tap will be wired up once releases are cut (`brew install stephanschmidt/tap/loupe`).

## Usage

```bash
loupe init        # interactive wizard — writes loupe.yaml (no secrets stored)
loupe baseline    # full ingest from Bitbucket + Jira, renders a fresh deck
loupe present     # opens the most recent deck in your browser
loupe status      # local sqlite summary — no API calls
```

`loupe baseline` prompts (echo off) for the Bitbucket app password and Jira API token every invocation. No env vars, no keychain in v0. State is kept locally at `.loupe/state.db`; decks land under `./reports/<timestamp>/index.html`.

Alongside the interactive deck, each run also writes standalone chart exports under `./reports/<timestamp>/charts/`:

- `throughput.png`, `adoption.png` — paste straight into Slack or email
- `throughput.svg`, `adoption.svg` — high-resolution for board docs / wikis

These are static snapshots — the in-browser deck uses Apache ECharts for the interactive version.

## Config

`loupe.yaml` holds non-secret coordinates only. See [`loupe.example.yaml`](loupe.example.yaml) for the full shape:

```yaml
org: acme-eng

git_host:
  provider: bitbucket-cloud
  base_url: https://api.bitbucket.org/2.0
  username: you@example.com

tracker:
  provider: jira-cloud
  site: acme.atlassian.net
  email: you@example.com

ai_adoption:
  # cutover_date: 2026-03-15   # uncomment to override auto-detection
  detection:
    co_author_trailers: true
  min_weekly_commits_for_cutover: 0.05

windows:
  baseline_weeks: 12
  comparison_weeks: 12

output:
  path: ./reports
```

Every workspace and Jira project the credentials can see is indexed automatically — no include/exclude lists in v0.

To use GitHub instead, set both providers to `github` and supply a personal access token at prompt time:

```yaml
git_host:
  provider: github
  # base_url and username are optional; defaults to api.github.com / authed user

tracker:
  provider: github
  # base_url is optional; defaults to api.github.com
```

Loupe will enumerate the authed user's own repos plus every org they belong to, and treat each repo with Issues enabled as a tracker project.

## Methodology

Loupe surfaces what it can measure honestly and refuses to overclaim.

- **Primary signal**: `Co-Authored-By:` trailers. Claude Code and Aider write these by default; Cursor and Copilot generally do not — the asymmetry is flagged on the deck rather than papered over.
- **Cutover detection**: first ISO week with an AI-commit ratio at or above the threshold (default 5%), or a manual date override.
- **No ROI calculation**: dev-hours-saved depends on assumptions that don't survive technical scrutiny. Loupe refuses to guess.
- **No "productivity +X%" headline**: throughput is always paired with whatever quality signal we can compute.

If no AI signal is detected at all, the tool still runs — lead time and throughput ship anyway.

## Adding a provider

Two interfaces live in [`internal/githost/host.go`](internal/githost/host.go) and [`internal/tracker/tracker.go`](internal/tracker/tracker.go). Drop a package that implements one of them, add a case to `buildGitHost` / `buildTracker` in [`cmd/cmdbaseline/cmdbaseline.go`](cmd/cmdbaseline/cmdbaseline.go), and the rest of the pipeline (ingest, analyze, deck) needs no changes.

## Development

```bash
make build           # build ./bin/loupe with ldflags-injected version
make test            # gotestsum across all packages
make coverage-check  # fails below 80% line coverage
make lint            # vet + staticcheck + golangci-lint + nilaway + gocyclo
make sec             # gosec + govulncheck
make check           # lint + sec + secrets
./scripts/smoke.sh   # end-to-end against an in-process httptest server
```

Go 1.26.3, `modernc.org/sqlite` (pure-Go, no CGO), GoReleaser cross-compiles linux/darwin/windows for amd64+arm64.

## License

MIT. See [LICENSE](LICENSE).
