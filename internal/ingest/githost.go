// Package ingest writes data fetched from githost.GitHost / tracker.Tracker
// providers into the local sqlite store. The package depends only on the
// interfaces in those packages — never on a concrete provider — so adding
// a new provider doesn't touch this file.
package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/StephanSchmidt/loupe/internal/githost"
	"github.com/StephanSchmidt/loupe/internal/store"
)

// GitHostStats is the summary returned by IngestGitHost.
type GitHostStats struct {
	Workspaces   int
	Repos        int
	Commits      int
	PullRequests int
}

// GitHostFilter restricts which workspaces/repos get ingested. The zero
// value means "ingest everything" (the default baseline behaviour).
type GitHostFilter struct {
	// Repo is "workspace/slug" — when set, only this repo is ingested,
	// every other workspace and repo is skipped before any commit or PR
	// API call is made.
	Repo string
}

// IngestGitHost walks gh's discovery surface (workspaces → repos → commits
// & PRs) and persists every row into s. Each repo's watermark is advanced
// at the end of its loop body, so a mid-baseline failure preserves
// progress for repos already processed.
//
// progressOut may be nil; otherwise it receives one line per repo.
func IngestGitHost(ctx context.Context, s *store.Store, gh githost.GitHost, progressOut io.Writer, filter GitHostFilter) (GitHostStats, error) {
	var stats GitHostStats
	provider := gh.Name()
	now := time.Now().UTC().Unix()

	wantWorkspace := ""
	if filter.Repo != "" {
		// "ws/slug" — anything past the first slash is the slug, matched
		// exactly by FullName() inside the repo loop.
		i := strings.IndexByte(filter.Repo, '/')
		if i <= 0 {
			return stats, fmt.Errorf("invalid --repo filter %q (want workspace/slug)", filter.Repo)
		}
		wantWorkspace = filter.Repo[:i]
	}

	workspaces, err := gh.ListWorkspaces(ctx)
	if err != nil {
		return stats, fmt.Errorf("list workspaces: %w", err)
	}
	for _, ws := range workspaces {
		if wantWorkspace != "" && ws.Slug != wantWorkspace {
			continue
		}
		if err := ingestWorkspace(ctx, s.DB(), gh, provider, ws, now, progressOut, &stats, filter); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func ingestWorkspace(
	ctx context.Context,
	db *sql.DB,
	gh githost.GitHost,
	provider string,
	ws githost.Workspace,
	now int64,
	progressOut io.Writer,
	stats *GitHostStats,
	filter GitHostFilter,
) error {
	if err := upsertWorkspace(ctx, db, provider, ws, now); err != nil {
		return err
	}
	stats.Workspaces++

	repos, err := gh.ListRepos(ctx, ws.Slug)
	if err != nil {
		return fmt.Errorf("list repos for %s: %w", ws.Slug, err)
	}
	for _, repo := range repos {
		if filter.Repo != "" && repo.FullName() != filter.Repo {
			continue
		}
		if err := ingestRepo(ctx, db, gh, provider, repo, now, progressOut, stats); err != nil {
			return err
		}
	}
	return advanceWorkspaceWatermark(ctx, db, provider, ws.Slug, now)
}

func ingestRepo(
	ctx context.Context,
	db *sql.DB,
	gh githost.GitHost,
	provider string,
	repo githost.Repo,
	now int64,
	progressOut io.Writer,
	stats *GitHostStats,
) error {
	if err := upsertRepo(ctx, db, provider, repo, now); err != nil {
		return err
	}
	stats.Repos++

	nCommits, err := streamRepoCommits(ctx, db, gh, provider, repo)
	if err != nil {
		return err
	}
	stats.Commits += nCommits

	nPRs, err := streamRepoPRs(ctx, db, gh, provider, repo)
	if err != nil {
		return err
	}
	stats.PullRequests += nPRs

	// Use end-of-repo-ingest time as the watermark instead of run-start.
	// If a commit lands during the ingest with committed_at between
	// run-start and the moment we actually fetched the commit list, using
	// run-start would skip it on the next baseline.
	if err := advanceRepoWatermark(ctx, db, provider, repo.FullName(), time.Now().UTC().Unix()); err != nil {
		return err
	}
	if progressOut != nil {
		_, _ = fmt.Fprintf(progressOut, "    %s: %d commits, %d PRs\n",
			repo.FullName(), nCommits, nPRs)
	}
	return nil
}

func streamRepoCommits(ctx context.Context, db *sql.DB, gh githost.GitHost, provider string, repo githost.Repo) (int, error) {
	since, err := readRepoWatermark(ctx, db, provider, repo.FullName(), "last_commit_indexed_at")
	if err != nil {
		return 0, err
	}
	n := 0
	for commit, streamErr := range gh.ListCommits(ctx, repo.RepoRef, since) {
		if streamErr != nil {
			return n, fmt.Errorf("stream commits %s: %w", repo.FullName(), streamErr)
		}
		if err := upsertCommit(ctx, db, provider, repo, commit); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func streamRepoPRs(ctx context.Context, db *sql.DB, gh githost.GitHost, provider string, repo githost.Repo) (int, error) {
	since, err := readRepoWatermark(ctx, db, provider, repo.FullName(), "last_pr_indexed_at")
	if err != nil {
		return 0, err
	}
	n := 0
	for pr, streamErr := range gh.ListPullRequests(ctx, repo.RepoRef, since) {
		if streamErr != nil {
			return n, fmt.Errorf("stream PRs %s: %w", repo.FullName(), streamErr)
		}
		if err := upsertPR(ctx, db, provider, repo, pr); err != nil {
			return n, err
		}
		if err := ingestPRCommits(ctx, db, gh, provider, repo, pr); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ingestPRCommits fetches the PR's pre-squash commits via ListPRCommits
// and persists them into pr_commits so the squash-recovery detector can
// read the original commit messages. A PR-commit's SHA may or may not
// also exist in `commits` (it does for regular merges, doesn't for
// squash merges) — pr_commits carries an independent copy of the
// message either way so detection doesn't depend on which merge mode
// the destination branch ended up using.
//
// Errors from the per-PR commit API are non-fatal — squash recovery is
// a recall booster, not a correctness requirement, and a 404/403 on a
// single PR shouldn't abort the whole ingest run.
func ingestPRCommits(ctx context.Context, db *sql.DB, gh githost.GitHost, provider string, repo githost.Repo, pr githost.PullRequest) error {
	id := scopedPRID(provider, repo.FullName(), pr.ID)
	commits, err := gh.ListPRCommits(ctx, repo.RepoRef, pr.ID)
	if err != nil {
		// Surface the error in logs eventually; for now, swallow so a
		// single bad PR doesn't kill the rest of the ingest.
		return nil //nolint:nilerr // intentional best-effort fetch
	}
	for _, c := range commits {
		if err := upsertPRCommit(ctx, db, id, c); err != nil {
			return err
		}
	}
	return nil
}

const upsertPRCommitSQL = `
INSERT INTO pr_commits (pr_id, commit_sha, author_email, author_name, message)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(pr_id, commit_sha) DO UPDATE SET
    author_email = excluded.author_email,
    author_name  = excluded.author_name,
    message      = excluded.message
`

func upsertPRCommit(ctx context.Context, db *sql.DB, prID string, c githost.Commit) error {
	_, err := db.ExecContext(ctx, upsertPRCommitSQL, prID, c.SHA, c.AuthorEmail, c.AuthorName, c.Message)
	if err != nil {
		return fmt.Errorf("upsert pr_commit %s/%s: %w", prID, c.SHA, err)
	}
	return nil
}

const upsertWorkspaceSQL = `
INSERT INTO workspaces (provider, slug, name, discovered_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(provider, slug) DO UPDATE SET name = excluded.name
`

func upsertWorkspace(ctx context.Context, db *sql.DB, provider string, ws githost.Workspace, discoveredAt int64) error {
	_, err := db.ExecContext(ctx, upsertWorkspaceSQL, provider, ws.Slug, ws.Name, discoveredAt)
	if err != nil {
		return fmt.Errorf("upsert workspace %s: %w", ws.Slug, err)
	}
	return nil
}

const advanceWorkspaceWatermarkSQL = `UPDATE workspaces SET last_indexed_at = ? WHERE provider = ? AND slug = ?`

func advanceWorkspaceWatermark(ctx context.Context, db *sql.DB, provider, slug string, at int64) error {
	_, err := db.ExecContext(ctx, advanceWorkspaceWatermarkSQL, at, provider, slug)
	return err
}

const upsertRepoSQL = `
INSERT INTO repos (provider, full_name, workspace, slug, name, discovered_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(provider, full_name) DO UPDATE SET
    workspace = excluded.workspace,
    slug      = excluded.slug,
    name      = excluded.name
`

func upsertRepo(ctx context.Context, db *sql.DB, provider string, repo githost.Repo, discoveredAt int64) error {
	_, err := db.ExecContext(ctx, upsertRepoSQL,
		provider, repo.FullName(), repo.Workspace, repo.Slug, repo.Name, discoveredAt)
	if err != nil {
		return fmt.Errorf("upsert repo %s: %w", repo.FullName(), err)
	}
	return nil
}

const advanceRepoWatermarkSQL = `
UPDATE repos SET last_commit_indexed_at = ?, last_pr_indexed_at = ?
WHERE provider = ? AND full_name = ?
`

func advanceRepoWatermark(ctx context.Context, db *sql.DB, provider, fullName string, at int64) error {
	_, err := db.ExecContext(ctx, advanceRepoWatermarkSQL, at, at, provider, fullName)
	return err
}

// readRepoWatermark reads the named column (last_commit_indexed_at or
// last_pr_indexed_at) for a repo. Returns zero time if NULL.
func readRepoWatermark(ctx context.Context, db *sql.DB, provider, fullName, column string) (time.Time, error) {
	// column is from a closed allowlist in the orchestrator above; safe to
	// interpolate.
	q := fmt.Sprintf(`SELECT %s FROM repos WHERE provider = ? AND full_name = ?`, column) // #nosec G201 -- column from trusted const
	var ts sql.NullInt64
	err := db.QueryRowContext(ctx, q, provider, fullName).Scan(&ts)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read %s for %s: %w", column, fullName, err)
	}
	if !ts.Valid {
		return time.Time{}, nil
	}
	return time.Unix(ts.Int64, 0).UTC(), nil
}

// upsertCommitSQL deliberately leaves provider/workspace/repo_name OUT of
// the ON CONFLICT update set. Two repos can legitimately share a SHA
// (forks, mirrors, monorepo extractions). We want first-write-wins
// attribution rather than letting a later re-ingest of a fork silently
// reassign every shared commit. The other columns (message, parent count,
// committed_at) are content-of-the-commit so they're safe to re-sync.
const upsertCommitSQL = `
INSERT INTO commits (
    sha, provider, workspace, repo_name, author_email, author_name,
    committed_at, message, parent_count, files_changed, insertions, deletions
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0)
ON CONFLICT(sha) DO UPDATE SET
    author_email = excluded.author_email,
    author_name  = excluded.author_name,
    committed_at = excluded.committed_at,
    message      = excluded.message,
    parent_count = excluded.parent_count
`

func upsertCommit(ctx context.Context, db *sql.DB, provider string, repo githost.Repo, c githost.Commit) error {
	_, err := db.ExecContext(ctx, upsertCommitSQL,
		c.SHA, provider, repo.Workspace, repo.FullName(),
		c.AuthorEmail, c.AuthorName, c.CommittedAt.Unix(),
		c.Message, c.ParentCount,
	)
	if err != nil {
		return fmt.Errorf("upsert commit %s: %w", c.SHA, err)
	}
	return nil
}

const upsertPRSQL = `
INSERT INTO prs (
    id, provider, workspace, repo_name, title, state, author_email,
    author_login, author_is_bot,
    source_branch, destination_branch, created_at, merged_at, closed_at,
    merge_commit_sha, labels
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    provider           = excluded.provider,
    workspace          = excluded.workspace,
    repo_name          = excluded.repo_name,
    title              = excluded.title,
    state              = excluded.state,
    author_email       = excluded.author_email,
    author_login       = excluded.author_login,
    author_is_bot      = excluded.author_is_bot,
    source_branch      = excluded.source_branch,
    destination_branch = excluded.destination_branch,
    merged_at          = excluded.merged_at,
    closed_at          = excluded.closed_at,
    merge_commit_sha   = excluded.merge_commit_sha,
    labels             = excluded.labels
`

// scopedPRID namespaces a provider's raw PR id (e.g. "1") with the
// provider and repo so PR #1 in two different repos doesn't collide on
// `prs.id`'s primary key. Provider-side ids like Bitbucket's "1" and
// GitHub's "1" would otherwise overwrite each other across repos.
func scopedPRID(provider, fullName, rawID string) string {
	return provider + ":" + fullName + "#" + rawID
}

func upsertPR(ctx context.Context, db *sql.DB, provider string, repo githost.Repo, pr githost.PullRequest) error {
	labels := ""
	if len(pr.Labels) > 0 {
		b, err := json.Marshal(pr.Labels)
		if err != nil {
			return fmt.Errorf("encode labels for PR %s: %w", pr.ID, err)
		}
		labels = string(b)
	}
	var mergedAt, closedAt sql.NullInt64
	if pr.MergedAt != nil {
		mergedAt = sql.NullInt64{Int64: pr.MergedAt.Unix(), Valid: true}
	}
	if pr.ClosedAt != nil {
		closedAt = sql.NullInt64{Int64: pr.ClosedAt.Unix(), Valid: true}
	}
	id := scopedPRID(provider, repo.FullName(), pr.ID)
	isBot := 0
	if pr.AuthorIsBot {
		isBot = 1
	}
	_, err := db.ExecContext(ctx, upsertPRSQL,
		id, provider, repo.Workspace, repo.FullName(),
		pr.Title, pr.State, pr.AuthorEmail,
		pr.AuthorLogin, isBot,
		pr.SourceBranch, pr.DestinationBranch,
		pr.CreatedAt.Unix(), mergedAt, closedAt,
		pr.MergeCommitSHA, labels,
	)
	if err != nil {
		return fmt.Errorf("upsert PR %s: %w", id, err)
	}
	return nil
}
