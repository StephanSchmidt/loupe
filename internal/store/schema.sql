-- Loupe state schema. All statements are idempotent — this file is applied
-- on every Open() and must remain safe to re-run.

CREATE TABLE IF NOT EXISTS commits (
    sha            TEXT PRIMARY KEY,
    repo_name      TEXT NOT NULL,
    author_email   TEXT NOT NULL,
    author_name    TEXT NOT NULL,
    committed_at   INTEGER NOT NULL,   -- unix epoch seconds
    message        TEXT NOT NULL,
    parent_count   INTEGER NOT NULL DEFAULT 0,
    files_changed  INTEGER NOT NULL DEFAULT 0,
    insertions     INTEGER NOT NULL DEFAULT 0,
    deletions      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_commits_committed_at ON commits(committed_at);
CREATE INDEX IF NOT EXISTS idx_commits_author_email ON commits(author_email);
CREATE INDEX IF NOT EXISTS idx_commits_repo_name    ON commits(repo_name);

CREATE TABLE IF NOT EXISTS ai_signals (
    commit_sha    TEXT NOT NULL,
    signal_kind   TEXT NOT NULL,        -- "co_author_trailer" | "pr_label" | "heuristic"
    signal_source TEXT NOT NULL,        -- "claude" | "aider" | "copilot" | "cursor" | ...
    confidence    TEXT NOT NULL,        -- "high" | "medium" | "low"
    detail        TEXT,
    PRIMARY KEY (commit_sha, signal_kind, signal_source),
    FOREIGN KEY (commit_sha) REFERENCES commits(sha) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_ai_signals_source ON ai_signals(signal_source);

CREATE TABLE IF NOT EXISTS prs (
    id                  TEXT PRIMARY KEY,    -- bitbucket numeric id as string
    repo_name           TEXT NOT NULL,
    title               TEXT NOT NULL,
    state               TEXT NOT NULL,        -- "OPEN" | "MERGED" | "DECLINED" | "SUPERSEDED"
    author_email        TEXT NOT NULL,
    author_login        TEXT NOT NULL DEFAULT '', -- GitHub login / Bitbucket nickname; "[bot]" suffix on App identities
    author_is_bot       INTEGER NOT NULL DEFAULT 0, -- 1 if host reports the author as a bot (GitHub user.type=="Bot")
    source_branch       TEXT NOT NULL,
    destination_branch  TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    merged_at           INTEGER,
    closed_at           INTEGER,
    merge_commit_sha    TEXT,
    labels              TEXT                  -- JSON array
);

CREATE INDEX IF NOT EXISTS idx_prs_created_at ON prs(created_at);
CREATE INDEX IF NOT EXISTS idx_prs_merged_at  ON prs(merged_at);

-- pr_commits captures the pre-squash commits attached to a PR via the
-- provider's "PR commits" API. For regular merges the SHAs in this table
-- usually appear in `commits` too (they landed on the destination branch).
-- For squash merges they don't — the row exists so the squash-recovery
-- detector can read the original commit message and emit signals against
-- the PR's merge_commit_sha.
CREATE TABLE IF NOT EXISTS pr_commits (
    pr_id        TEXT NOT NULL,
    commit_sha   TEXT NOT NULL,
    author_email TEXT NOT NULL DEFAULT '',
    author_name  TEXT NOT NULL DEFAULT '',
    message      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (pr_id, commit_sha)
);

CREATE TABLE IF NOT EXISTS tickets (
    id              TEXT PRIMARY KEY,   -- e.g. "ENG-123"
    project_key     TEXT NOT NULL,
    title           TEXT NOT NULL,
    type            TEXT,
    status          TEXT,
    created_at      INTEGER NOT NULL,
    resolved_at     INTEGER,
    closed_at       INTEGER,
    assignee_email  TEXT,
    estimate        REAL
);

CREATE INDEX IF NOT EXISTS idx_tickets_project    ON tickets(project_key);
CREATE INDEX IF NOT EXISTS idx_tickets_created_at ON tickets(created_at);

CREATE TABLE IF NOT EXISTS ticket_transitions (
    ticket_id    TEXT NOT NULL,
    at           INTEGER NOT NULL,
    from_status  TEXT,
    to_status    TEXT NOT NULL,
    PRIMARY KEY (ticket_id, at, to_status)
);

CREATE TABLE IF NOT EXISTS ticket_commits (
    ticket_id    TEXT NOT NULL,
    commit_sha   TEXT NOT NULL,
    join_method  TEXT NOT NULL,    -- "commit_msg" | "pr_title" | "branch_name"
    PRIMARY KEY (ticket_id, commit_sha)
);

CREATE INDEX IF NOT EXISTS idx_ticket_commits_sha ON ticket_commits(commit_sha);

CREATE TABLE IF NOT EXISTS runs (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    kind                    TEXT NOT NULL,      -- "baseline" | "run"
    started_at              INTEGER NOT NULL,
    finished_at             INTEGER,
    status                  TEXT NOT NULL,      -- "running" | "ok" | "error"
    last_ingested_commit_at INTEGER,
    notes                   TEXT
);

-- Provider discovery + watermarks. `provider` is the GitHost/Tracker Name()
-- (e.g. "bitbucket-cloud", "jira-cloud"); carrying it on every domain row
-- lets multiple providers coexist in one store without migration.

CREATE TABLE IF NOT EXISTS workspaces (
    provider        TEXT NOT NULL,
    slug            TEXT NOT NULL,
    name            TEXT NOT NULL,
    discovered_at   INTEGER NOT NULL,
    last_indexed_at INTEGER,
    PRIMARY KEY (provider, slug)
);

CREATE TABLE IF NOT EXISTS repos (
    provider               TEXT NOT NULL,
    full_name              TEXT NOT NULL,        -- "workspace/slug"
    workspace              TEXT NOT NULL,
    slug                   TEXT NOT NULL,
    name                   TEXT NOT NULL,
    discovered_at          INTEGER NOT NULL,
    last_commit_indexed_at INTEGER,
    last_pr_indexed_at     INTEGER,
    PRIMARY KEY (provider, full_name)
);

CREATE INDEX IF NOT EXISTS idx_repos_workspace ON repos(provider, workspace);

CREATE TABLE IF NOT EXISTS tracker_projects (
    provider              TEXT NOT NULL,
    key                   TEXT NOT NULL,
    name                  TEXT NOT NULL,
    discovered_at         INTEGER NOT NULL,
    last_issue_indexed_at INTEGER,
    PRIMARY KEY (provider, key)
);
