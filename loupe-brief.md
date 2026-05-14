# Loupe — Project Brief

A CLI tool that measures the impact of AI coding assistants on engineering teams by analyzing git commits and ticket data, and outputs a presenter-ready reveal.js slide deck for CTOs/CEOs.

---

## 1. Problem & Positioning

**Audience:** CTOs and CEOs who have rolled out (or are evaluating) AI coding tools and need to answer "is this working?" — for themselves, their boards, and finance.

**Shape:** Not an analytics platform. A diagnostic CLI, like `lighthouse` or `npm audit`. Run once for a baseline, run weekly to track impact. Output is a reveal.js slide deck the CTO presents in the exec meeting.

**Why this shape wins:**
- No data leaves the customer's environment → no security review, no procurement
- `brew install` and run in minutes → trial-friendly, no SaaS commitment
- Slides (not a dashboard) match how execs actually communicate up
- Weekly cadence creates the habit without requiring a login

**The honest pitch:** "We measure AI-assisted work where we can detect it. Here's coverage by tool, here's what it tells us, here's what we can't see." Overclaiming kills trust with technical CTOs.

---

## 2. The Core Methodology

### Data sources
- **Git:** commits, PRs, reviews, file diffs, branch lifecycles, co-author trailers
- **Tickets** (Jira / Linear / GitHub Issues): state transitions, estimates, types, assignees, links
- **The join:** ticket ID → commits → PR → merge → close. This is where the value lives.

### AI signal detection (in order of reliability)
1. **Direct telemetry** from Copilot/Cursor/Claude Code APIs (best, hardest to get)
2. **Co-author trailers** — `Co-Authored-By: Claude <noreply@anthropic.com>` and equivalents. Claude Code and Aider write these by default. Cursor and Copilot generally don't — flag this asymmetry prominently.
3. **Self-tagged PRs** (label like `ai-assisted`) — opt-in, voluntary
4. **Cohort comparison** — AI-tool seat holders vs non-seats, or pre/post adoption windows
5. **Heuristics** (commit size, comment style) — noisy, don't lead with these

### The cutover line
The "AI adoption began" marker for before/after analysis. Two options, both supported:
- **Auto-detected:** first AI-tagged commit, or first week with >5% AI commits (to avoid one-off noise)
- **Manual override** in config: "rolled out Copilot org-wide on March 15." Some teams want a narrative date even if individuals were experimenting earlier.

### Confidence tiers
Every metric surfaces which signal tier it relies on:
- **High:** trailer present, telemetry available
- **Medium:** seat-holder + timing patterns
- **Low:** heuristic only

---

## 3. Metrics

Keep headline metrics to **4-6**. More than that and the story dilutes.

### Throughput
- PRs merged per dev per week
- Tickets closed per sprint
- Story points completed

### Cycle time (the metric execs care about most)
- Ticket-opened → first commit
- First commit → PR open
- PR open → merge
- Merge → ticket close
- End-to-end: ticket-opened → merged-to-main

### Quality (must always be paired with throughput)
- Code churn (lines rewritten within 2-3 weeks)
- Revert rate
- Bug-tagged tickets per feature ticket
- Reopened tickets
- Hotfix frequency

### Review velocity
- Time-to-first-review
- Review iterations
- Comments per PR

### Estimation accuracy
- Estimated vs actual time
- Drift over sprints

### Work shape
- Refactor / new / bugfix ratios
- File-type distribution
- PR size distribution

### Adoption
- % of devs with AI-tagged commits
- Which tools detected
- Coverage by repo and team

---

## 4. Traps & Methodology Defenses

Naive before/after will get torn apart by any technical CTO. Build in defenses:

- **Long enough windows:** minimum 8-12 weeks each side, ideally a quarter
- **Seasonality controls:** holidays, end-of-quarter pushes, hiring waves
- **Confounders surfaced explicitly:** team size changes, major refactors, incident weeks
- **Cohort comparison where possible:** teams that adopted vs teams that didn't, same time window — much stronger than pure before/after
- **Per-dev curves:** each developer's individual before/after, then aggregated — controls for team composition shifts
- **Selection bias on AI-tagged commits:** devs reach for AI on certain work types (boilerplate, tests, glue) and not others (debugging, architecture). Comparing AI vs non-AI commits is comparing different *tasks*, not different *methods*. Flag this.
- **Trailer ≠ contribution amount:** one autocompleted line and 500 generated lines both get the same trailer
- **Squash merges can drop trailers** depending on platform config — verify per repo
- **LOC and commit count reward the wrong things** — never standalone, always paired with quality

**The trap to avoid:** selling a "productivity up 40%" headline number. Every consulting firm has been burned doing this. Honest measurement is the differentiator.

---

## 5. Questions Execs Will Ask (Design the Report Around These)

- "Is this real or is the team gaming the metric?" → pair every throughput metric with a quality metric
- "What about the devs not using it?" → adoption coverage chart
- "Is this worth the seat cost?" → ROI calculation, even if rough
- "Which teams should adopt next?" → comparative view across teams
- "Are we shipping more bugs?" → defect rate trend, prominently

---

## 6. Tech Stack

- **Language:** Go (single static binary, no runtime deps, runs anywhere including air-gapped)
- **Distribution:** Homebrew (`brew install yourorg/tap/loupe`), GoReleaser for cross-compile + tap update + GitHub release
- **Storage:** sqlite via `modernc.org/sqlite` (pure Go, no CGO — keeps brew install clean). State at `.loupe/state.db`
- **Git host:** Bitbucket Cloud REST 2.0 via a hand-rolled client over a shared `internal/apiclient` (basic auth). No local git checkout required.
- **Tracker:** Jira Cloud REST v3 via a hand-rolled client over the same `internal/apiclient` (basic auth).
- **Provider abstraction:** `internal/githost.GitHost` and `internal/tracker.Tracker` interfaces with neutral domain types. Adding GitLab / GitHub / Linear is a one-package + one-switch-case addition.
- **Output:** reveal.js bundled as embedded assets via `//go:embed` — single binary stays single
- **Templating:** Go `html/template`, chart data injected as JSON
- **Secrets:** prompted interactively (echo off via `golang.org/x/term`) on every `loupe baseline` invocation. No env vars, no keychain in v0. Hidden `--bitbucket-token` / `--jira-token` flags exist for CI / smoke tests.

---

## 7. CLI Surface

```
loupe init        # interactive config wizard
loupe baseline    # first run, indexes every workspace + project via API, renders deck
loupe run         # weekly incremental run (v0.2 — currently a stub)
loupe status      # local-only summary of what's indexed and when
loupe present     # opens reveal.js deck in browser
loupe export      # export deck as static HTML folder or PDF (v0.2 — stub)
```

### Flags worth having
- `--cutover-date` to override AI adoption detection
- `--dry-run` to validate config without writing state

---

## 8. Config File (`loupe.yaml`)

Checked into the repo. Only non-secrets are persisted — tokens are prompted at every `loupe baseline` invocation.

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

teams:
  - name: platform
    members: [alice@acme.com, bob@acme.com]

ai_adoption:
  # auto-detect by default; uncomment to override
  # cutover_date: 2026-03-15
  detection:
    co_author_trailers: true
    pr_labels: [ai-assisted]
  min_weekly_commits_for_cutover: 0.05

windows:
  baseline_weeks: 12
  comparison_weeks: 12

output:
  path: ./reports
```

Workspaces and Jira projects are **auto-discovered** from the credential — every workspace / project the user can see is indexed. No include / exclude lists in v0.

---

## 9. The Slide Deck (the actual product)

Each slide = one idea, presenter-paced. Large numbers, one chart, minimal text. reveal.js speaker notes for the CTO to glance at.

1. **Title** — "AI Impact Report — Acme Eng — Week of [date]"
2. **TL;DR** — 3 numbers, big type, directional arrows
3. **The adoption line** — when AI showed up, who's using it, % coverage, which tools detected
4. **Cycle time before/after** — *the money chart*. Weekly cycle time plotted, AI adoption line marked, single delta number
5. **Throughput before/after** — PRs and tickets shipped
6. **Quality counterweight** — defects, reverts, churn. "Here's what didn't get worse" (or did)
7. **Per-team breakdown** — where impact is concentrated
8. **Confidence & caveats** — the methodology slide that earns trust with skeptics
9. **What changed this week** — weekly-run only, deltas vs last report and vs baseline
10. **Appendix** — raw numbers, definitions, confounders considered

### Slide-level affordances
- **Deep links** per slide — CTO pastes just the cycle time slide into Slack
- **Speaker notes** on every slide
- **Skeptic mode / methodology appendix** — show the raw data, windowing logic, confounders. CTOs want to poke at it before trusting the headline. Make that easy.

---

## 10. First-Run Experience

If `loupe init` takes more than 2 minutes the tool feels heavy. The wizard should:
- Auto-detect git remotes in the current directory
- Ask for ticket system, validate creds
- Auto-detect or prompt for team members
- Finish with "ready — run `loupe baseline`"

Baseline run on a typical repo should complete in <5 min. Weekly run in <1 min.

---

## 11. Operational Properties

- **Idempotent:** weekly run on the same week produces identical output
- **Incremental:** state file lets weekly runs only fetch new data
- **Offline mode:** git-only fallback for shops that won't let tools call out
- **No telemetry from Loupe itself** — trust matters with this audience
- **Static output:** no server, no auth. Email it, Slack it, commit it to the repo
- **CI-friendly:** exit code + summary line for teams that want to automate the weekly cadence (GitHub Action: weekly cron → run → commit deck to a `reports/` branch)

---

## 12. Distribution & Pricing

**Distribution:** Homebrew tap day one. GoReleaser config in repo. Removes friction from shipping weekly improvements.

**Pricing models to consider:**
- **Open-core:** CLI open source, premium report templates and "executive narrative" generation paid
- **Fully open + hosted weekly automation:** OSS CLI, paid GitHub Action / hosted runner for teams that want hands-off
- **Per-seat or per-repo annual license**

Decide early — it shapes what goes in the binary vs the templates.

---

## 13. The One Chart That Matters

Page-one chart, the one a CEO glances at and forwards to the board:

**Weekly end-to-end cycle time (ticket-opened → merged-to-main), plotted over the last 6 months, with a vertical line at AI adoption and a single delta number ("−23% post-adoption").** Quality counterweight (defect rate) as a secondary line on the same chart so the metric can't be gamed.

Everything else in the deck is supporting evidence for this one chart.

---

## 14. Open Questions To Resolve Next

- Exact schema for the ticket↔commit↔PR join, including fallback when ticket IDs aren't in commit messages
- Which chart library inside reveal.js — Chart.js, ECharts, or hand-rolled D3
- How to handle monorepos vs multi-repo orgs in the config
- Whether to support GitLab/Bitbucket in v1 or GitHub-only
- ROI calculation methodology — seat cost is easy, "dev-hours saved" needs an assumption you'll have to defend
- Whether per-developer views exist at all (surveillance risk) or only aggregates ≥ N people
