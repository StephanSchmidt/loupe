# Bitbucket single-token workspace access

**Date:** 2026-06-02
**Status:** Approved

## Problem

`loupe baseline` against a `bitbucket-cloud` git host fails with HTTP 401 when
the user supplies an Atlassian API token:

```
ingest git host: list workspaces: bitbucket-cloud GET /2.0/workspaces
returned 401: {"error": {"message": "Token is invalid, expired, or not
supported for this endpoint."}}
```

The ingest loop (`internal/ingest/githost.go:59`) unconditionally calls
`gh.ListWorkspaces`, and the Bitbucket implementation hits the account-level
`/2.0/workspaces` endpoint. Atlassian API tokens (the scoped tokens created at
id.atlassian.com) are **not supported on `/2.0/workspaces`** — only Bitbucket
app passwords or OAuth are. Resource endpoints like `/repositories/{workspace}`
*do* accept API tokens.

The predecessor tool (`../measurements`) never hit this: it read the workspace
from `bitbucket_workspace` in config and went straight to
`/repositories/{workspace}`, so a single Atlassian API token covered both
Bitbucket and Jira.

Loupe currently treats `org` as a display label only and ignores it during
ingest, so there is no way to scope Bitbucket to a known workspace and skip the
enumeration call.

## Goal

A single Atlassian API token works for both `bitbucket-cloud` and `jira-cloud`
in one `loupe baseline` run, matching the old tool's behavior. No app password
required.

## Design

Bitbucket-only change, isolated to the client. `org` becomes a real workspace
scope for Bitbucket (it stays a label for GitHub/GitLab/Azure).

1. **`bitbucket.New` gains a `workspace` parameter.** Stored on `Client` as a
   new field. `buildGitHost` (`cmd/cmdbaseline/cmdbaseline.go`) passes
   `cfg.Org`.

2. **`bitbucket.ListWorkspaces` short-circuits.** When `workspace != ""`, it
   returns a single `githost.Workspace{Slug: workspace, Name: workspace}` and
   makes **no HTTP call** — `/2.0/workspaces` is never touched. When
   `workspace == ""`, it falls back to the existing `/2.0/workspaces`
   enumeration (preserves the old path for tests and any app-password caller).

   The Name is set to the slug rather than fetched via `/2.0/workspaces/{slug}`,
   to avoid a second call that an API token might also reject. Name is only used
   for display and the workspace upsert.

3. The ingest loop is unchanged: it receives the single-element workspace list
   and proceeds to `ListRepos(org)` → `/repositories/{org}`, which API tokens
   support.

### Auth note

Bitbucket basic-auth wire format is unchanged: `username:secret`. With an
Atlassian API token the username is the account **email** (already what the
transitioned `loupe.yaml` sets); with an app password it is the Bitbucket
username. Both still work — the only change is which workspace-listing path
runs.

## Out of scope

- GitHub / GitLab / Azure workspace enumeration (unchanged; `org` stays a label
  there).
- Other Bitbucket endpoints (codesearch, repos, commits, branches).
- Fetching the workspace display name.

## Testing

- New unit test: `ListWorkspaces` with a workspace set returns exactly that
  workspace and issues **zero** HTTP requests (assert via a test server that
  fails the test if called).
- Existing enumeration test keeps passing by constructing the client with an
  empty workspace.
- Update all `bitbucket.New` call sites to pass the new argument.

## Files touched

- `internal/githost/bitbucket/client.go` — field, `New` param, short-circuit.
- `cmd/cmdbaseline/cmdbaseline.go` — pass `cfg.Org`.
- `internal/githost/bitbucket/client_test.go` — new arg, new test.
- `loupe.example.yaml` / `README.md` — note Bitbucket uses `org` as the
  workspace and works with a single Atlassian API token.
