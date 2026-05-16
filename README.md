# Loupe

<p align="center"><img src="logo.svg" alt="Loupe" width="420"></p>

[![CI](https://github.com/StephanSchmidt/loupe/actions/workflows/ci.yml/badge.svg)](https://github.com/StephanSchmidt/loupe/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/StephanSchmidt/loupe)](https://goreportcard.com/report/github.com/StephanSchmidt/loupe)
[![Go Reference](https://pkg.go.dev/badge/github.com/StephanSchmidt/loupe.svg)](https://pkg.go.dev/github.com/StephanSchmidt/loupe)
[![Latest Release](https://img.shields.io/github/v/release/StephanSchmidt/loupe)](https://github.com/StephanSchmidt/loupe/releases/latest)
[![Dependabot](https://img.shields.io/badge/dependabot-enabled-blue?logo=dependabot)](https://github.com/StephanSchmidt/loupe/network/updates)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/StephanSchmidt/loupe/blob/main/LICENSE)

A small CLI that looks at your commits and tells you how much of your codebase is being written with AI assistants. It talks to Bitbucket+Jira or GitHub, reads commit trailers, and produces a reveal.js slide deck you can show in an exec meeting.

Think `lighthouse`, but for AI adoption. Run it once for a baseline, run it again in a quarter, compare. It runs locally — no SaaS account, nothing leaves your machine.

## Status

v0.1. The headline charts work, but a lot is still on the to-do list.

What works:

- Bitbucket Cloud + Jira Cloud, or GitHub on its own (one PAT plays both roles)
- AI detection across two confidence tiers: trailers, body footers, AI-bot author identity, PR labels, branch prefixes, and squash-merge recovery (high-confidence); seat-holder propagation (medium, opt-in)
- Tool list: Claude Code, Aider, Copilot, Cursor, Devin, Gemini Code Assist, Jules, OpenCode
- Auto-detected adoption cutover week, with a config override if you'd rather pin it
- reveal.js deck with weekly throughput (human vs AI evidence vs AI inferred) and adoption %, plus PNG/SVG exports

Not done yet:

- `loupe run` for weekly incremental updates (stub)
- `loupe export` to static HTML / PDF (stub)
- End-to-end cycle time (ticket → merged) — this is the chart I actually wanted
- Per-team breakdown and a quality counterweight (defects, churn)
- GitLab, Linear, GitHub Enterprise Server

## Install

**macOS and Linux (recommended):**

```bash
brew install stephanschmidt/tap/loupe
```

The Homebrew tap publishes a fresh bottle on every release, Intel and Apple Silicon.

<details>
<summary>Other install methods</summary>

- **Go**: `go install github.com/StephanSchmidt/loupe/cmd/loupe@latest`
- **Prebuilt binaries** for linux / macOS / windows on amd64 + arm64: see the [releases page](https://github.com/StephanSchmidt/loupe/releases)
- **From source**:
  ```bash
  git clone https://github.com/StephanSchmidt/loupe.git
  cd loupe
  make build        # produces ./bin/loupe
  ```

</details>

## Usage

```bash
loupe init        # interactive wizard, writes loupe.yaml
loupe baseline    # ingest, then render the deck
loupe present     # opens the latest deck in your browser
loupe status      # local sqlite summary, no API calls
```

`loupe baseline` prompts for credentials every time it runs (echo off). There are no env vars or keychain hooks in v0 — I'd rather not invent a key-management story before it's needed. State lives at `.loupe/state.db`; decks go under `./reports/<timestamp>/`.

If you only care about one repo on a GitHub account that has fifty others, narrow it down:

```bash
loupe baseline --repo StephanSchmidt/loupe
```

`--repo` filters the git host before any API call. When both providers are GitHub, the tracker filter follows automatically. Use `--project KEY` to scope the tracker side independently (a Jira project key, for instance).

Each run also drops standalone chart exports under `./reports/<timestamp>/charts/`:

- `throughput.png`, `adoption.png` for Slack and email
- `throughput.svg`, `adoption.svg` for board docs and wikis

The in-browser deck uses Apache ECharts; the PNG/SVG files are static snapshots produced server-side.

## Config

`loupe.yaml` holds non-secret coordinates only. Full shape is in [`loupe.example.yaml`](loupe.example.yaml):

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
  # cutover_date: 2026-03-15   # uncomment to pin the cutover yourself
  detection:
    co_author_trailers: true
  min_weekly_commits_for_cutover: 0.05

windows:
  baseline_weeks: 12
  comparison_weeks: 12

output:
  path: ./reports
```

Every workspace and Jira project the credentials can see is indexed. There's no include/exclude list — let me know if you actually need one.

For GitHub-only setups, set both providers to `github` and supply a PAT at prompt time:

```yaml
git_host:
  provider: github
  # base_url and username are optional; defaults to api.github.com / authed user

tracker:
  provider: github
  # base_url is optional; defaults to api.github.com
```

Loupe enumerates your own repos plus every org you belong to, and treats each repo with Issues enabled as a tracker project.

## How AI detection works

Detection runs in two confidence tiers. High-confidence signals are direct evidence — something specific in the commit, the PR, or the author identity. Medium-confidence signals are inferred from those. The deck splits the throughput chart into "AI (evidence)" and "AI (inferred)" whenever any week has medium-confidence commits.

**High-confidence**

- `Co-Authored-By:` trailer for Claude, Aider, Copilot, Cursor, Devin, Gemini Code Assist, Jules, OpenCode.
- Body footer (`Generated with [Claude Code]`, `Generated with [opencode]`).
- Author identity is a known AI bot (Copilot Coding Agent, Devin, Gemini Code Assist, Jules).
- PR carries an AI label (`ai-generated`, `copilot`, `claude`, …) — list is configurable.
- PR branch starts with an AI prefix (`copilot/`, `cursor/`, `claude/`, …) — list is configurable.
- Squash-merge recovery: trailers on the pre-squash PR commits get attributed to the merge SHA, so the trailer doesn't get lost when the PR is squashed.

**Medium-confidence (inferred)**

- Seat-holder propagation: if a developer has any high-confidence AI commit in a given week and repo, their other commits that week and repo are marked as inferred AI. Off by default — turn it on in `loupe.yaml` if you want it.
- Unrecognised `*[bot]` author identity: looks like a GitHub App bot, no matching tool — likely automated, tool unknown.

The cutover week is the first ISO week where AI-tagged commits hit 5% of weekly commits (configurable). You can also pin a date in `loupe.yaml`.

Copilot and Cursor users still tend to read low because their tools don't write trailers by default. The PR-label, branch-prefix, and squash-recovery detectors close part of that gap; the asymmetry is called out on the methodology slide either way.

There's no ROI calculation. The math you'd need — hours saved per AI-tagged commit — falls apart the moment a skeptical CFO asks where the number comes from, so I didn't add it. Throughput and adoption are what the deck shows.

If no AI signal is detected, the deck still renders. Throughput and lead time don't need it.

## Development

```bash
make build           # ./bin/loupe with version ldflags
make test            # gotestsum across all packages
make coverage-check  # fails below 80% line coverage
make lint            # vet + staticcheck + golangci-lint + nilaway + gocyclo
make sec             # gosec + govulncheck
make check           # lint + sec + secrets
go tool smoke        # end-to-end against an in-process httptest server
```

Go 1.26.3, `modernc.org/sqlite` (pure Go, no CGO), GoReleaser cross-compiles linux/darwin/windows for amd64+arm64.

## License

MIT. See [LICENSE](LICENSE).
