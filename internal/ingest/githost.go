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
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/StephanSchmidt/loupe/internal/apiclient"
	"github.com/StephanSchmidt/loupe/internal/githost"
	"github.com/StephanSchmidt/loupe/internal/progress"
	"github.com/StephanSchmidt/loupe/internal/store"
)

// repoIngestConcurrency bounds how many repos in a workspace are scanned in
// parallel. The dominant cost is API latency, so overlapping repos is a
// large win; the store serialises writes through a single connection, so
// the database stays consistent regardless. Each repo additionally fetches
// its commits and PRs concurrently, so peak in-flight requests is about
// twice this. The apiclient's bounded 429/5xx backoff keeps that from
// tripping rate limits.
const repoIngestConcurrency = 8

// progressEvery / progressEveryPR throttle mid-repo progress reports so a
// large repo's running counts tick visibly without flooding the reporter.
const (
	progressEvery   = 100
	progressEveryPR = 25
)

// ErrInterrupted is returned when ingest stops early because the caller's
// context was cancelled (Ctrl-C). It wraps context.Canceled so callers can
// detect it with errors.Is(err, context.Canceled). In-flight repos are
// allowed to finish first, so the progress they made is persisted and the
// next run resumes from there.
var ErrInterrupted = fmt.Errorf("ingest interrupted: %w", context.Canceled)

// GitHostStats is the summary returned by IngestGitHost.
type GitHostStats struct {
	Workspaces   int
	Repos        int
	Commits      int
	PullRequests int
}

// GitHostFilter restricts which workspaces/repos get ingested and carries
// per-run ingest options. The zero value means "ingest everything, no
// squash-merge recovery".
type GitHostFilter struct {
	// Repo is "workspace/slug" — when set, only this repo is ingested,
	// every other workspace and repo is skipped before any commit or PR
	// API call is made.
	Repo string

	// SquashMergeRecovery enables the per-PR commit fetch that recovers
	// pre-squash commit messages. It is the most expensive part of ingest
	// (one extra API call per PR), so it is opt-in: callers pass the value
	// of ai_adoption.detection.squash_merge_recovery here.
	SquashMergeRecovery bool

	// Since overrides the per-repo watermark for this run. When non-zero,
	// commits and PRs are fetched from this point regardless of the stored
	// last_commit_indexed_at / last_pr_indexed_at — used by `loupe run
	// --since` to re-pull a window. The zero value keeps the normal
	// watermark-incremental behaviour.
	Since time.Time
}

// IngestGitHost walks gh's discovery surface (workspaces → repos → commits
// & PRs) and persists every row into s. Each repo's watermark is advanced
// at the end of its loop body, so a mid-baseline failure preserves
// progress for repos already processed.
//
// reporter receives progress events; pass nil to discard them.
func IngestGitHost(ctx context.Context, s *store.Store, gh githost.GitHost, reporter progress.Reporter, filter GitHostFilter) (GitHostStats, error) {
	if reporter == nil {
		reporter = progress.Nop()
	}
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
		if ctx.Err() != nil {
			return stats, ErrInterrupted
		}
		if err := ingestWorkspace(ctx, s.DB(), gh, provider, ws, now, reporter, &stats, filter); err != nil {
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
	reporter progress.Reporter,
	stats *GitHostStats,
	filter GitHostFilter,
) error {
	if err := upsertWorkspace(ctx, db, provider, ws, now); err != nil {
		return err
	}
	stats.Workspaces++

	reporter.Listing(ws.Slug)
	repos, err := gh.ListRepos(ctx, ws.Slug)
	if err != nil {
		return fmt.Errorf("list repos for %s: %w", ws.Slug, err)
	}
	// If the host reports the workspace's true repo count, surface repos the
	// credential can't read (insufficient token scope) as "hidden".
	visible, total := len(repos), len(repos)
	if rc, ok := gh.(githost.RepoCounter); ok {
		if t, ok := rc.WorkspaceRepoTotal(ws.Slug); ok && t > total {
			total = t
		}
	}
	reporter.WorkspaceFound(ws.Slug, visible, total)

	// Repos are independent: scan up to repoIngestConcurrency of them in
	// parallel. A mutex guards the shared stats and progress writer; the
	// store itself serialises writes through its single connection.
	//
	// On Ctrl-C we let in-flight repos finish rather than abandoning them
	// mid-write, so their watermarks are saved and the next run resumes
	// cleanly. That means the repo work must NOT observe the signal
	// cancellation: it runs under a detached context. The signal context
	// (ctx) is only consulted to stop scheduling *new* repos. A genuine
	// repo error still cancels its siblings via gctx (fail-fast).
	workCtx := context.WithoutCancel(ctx)
	g, gctx := errgroup.WithContext(workCtx)
	g.SetLimit(repoIngestConcurrency)
	var mu sync.Mutex
	interrupted := false
	for _, repo := range repos {
		if filter.Repo != "" && repo.FullName() != filter.Repo {
			continue
		}
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		repo := repo
		g.Go(func() error {
			nCommits, nPRs, err := ingestRepo(gctx, db, gh, provider, repo, now, reporter, filter.SquashMergeRecovery, filter.Since)
			if err != nil {
				return err
			}
			mu.Lock()
			stats.Repos++
			stats.Commits += nCommits
			stats.PullRequests += nPRs
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if interrupted {
		// Don't advance the workspace watermark: the workspace isn't fully
		// indexed. (It isn't read for skipping anyway, but keep it honest.)
		return ErrInterrupted
	}
	return advanceWorkspaceWatermark(workCtx, db, provider, ws.Slug, now)
}

// ingestRepo ingests one repo and returns its commit and PR counts. It is
// safe to run concurrently for distinct repos: every DB write goes through
// the store's single serialised connection, and reporter is concurrency-safe.
func ingestRepo(
	ctx context.Context,
	db *sql.DB,
	gh githost.GitHost,
	provider string,
	repo githost.Repo,
	now int64,
	reporter progress.Reporter,
	squashRecovery bool,
	sinceOverride time.Time,
) (nCommits, nPRs int, err error) {
	if err := upsertRepo(ctx, db, provider, repo, now); err != nil {
		return 0, 0, err
	}
	reporter.RepoStart(repo.FullName())

	// Carry a retry notifier so the apiclient's 429/5xx backoff surfaces as
	// "on backoff" status for this repo without parsing request paths.
	ctx = apiclient.WithRetryNotify(ctx, func(attempt int, delay time.Duration) {
		reporter.RepoBackoff(repo.FullName(), attempt, delay)
	})

	// Commits and PRs hit independent endpoints and independent tables —
	// fetch them concurrently so a repo's wall-clock is the slower of the
	// two streams, not their sum. A real error in one cancels the other
	// (fail-fast) via the group's context.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		n, err := streamRepoCommits(gctx, db, gh, provider, repo, sinceOverride, reporter)
		nCommits = n
		return err
	})
	g.Go(func() error {
		n, err := streamRepoPRs(gctx, db, gh, provider, repo, squashRecovery, sinceOverride, reporter)
		nPRs = n
		return err
	})
	if err := g.Wait(); err != nil {
		return 0, 0, err
	}

	// Use end-of-repo-ingest time as the watermark instead of run-start.
	// If a commit lands during the ingest with committed_at between
	// run-start and the moment we actually fetched the commit list, using
	// run-start would skip it on the next baseline.
	if err := advanceRepoWatermark(ctx, db, provider, repo.FullName(), time.Now().UTC().Unix()); err != nil {
		return 0, 0, err
	}
	reporter.RepoDone(repo.FullName(), nCommits, nPRs)
	return nCommits, nPRs, nil
}

func streamRepoCommits(ctx context.Context, db *sql.DB, gh githost.GitHost, provider string, repo githost.Repo, sinceOverride time.Time, reporter progress.Reporter) (int, error) {
	since := sinceOverride
	if since.IsZero() {
		var err error
		since, err = readRepoWatermark(ctx, db, provider, repo.FullName(), "last_commit_indexed_at")
		if err != nil {
			return 0, err
		}
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
		if n%progressEvery == 0 {
			reporter.RepoProgress(repo.FullName(), n, -1)
		}
	}
	return n, nil
}

func streamRepoPRs(ctx context.Context, db *sql.DB, gh githost.GitHost, provider string, repo githost.Repo, squashRecovery bool, sinceOverride time.Time, reporter progress.Reporter) (int, error) {
	since := sinceOverride
	if since.IsZero() {
		var err error
		since, err = readRepoWatermark(ctx, db, provider, repo.FullName(), "last_pr_indexed_at")
		if err != nil {
			return 0, err
		}
	}
	n := 0
	for pr, streamErr := range gh.ListPullRequests(ctx, repo.RepoRef, since) {
		if streamErr != nil {
			return n, fmt.Errorf("stream PRs %s: %w", repo.FullName(), streamErr)
		}
		if err := upsertPR(ctx, db, provider, repo, pr); err != nil {
			return n, err
		}
		// Squash-merge recovery is the only per-PR API call; skip it
		// entirely when disabled so a large org isn't billed one request
		// per PR.
		if squashRecovery {
			if err := ingestPRCommits(ctx, db, gh, provider, repo, pr); err != nil {
				return n, err
			}
		}
		n++
		if n%progressEveryPR == 0 {
			reporter.RepoProgress(repo.FullName(), -1, n)
		}
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

// upsertCommitSQL resolves the provider/workspace/repo_name attribution of
// a shared SHA deterministically: two repos can legitimately share a SHA
// (forks, mirrors, monorepo extractions), and the lexicographically-
// smallest repo_name wins. This is order-independent on purpose — repos are
// ingested concurrently and the host's repo-list order isn't stable across
// runs, so a "first-write-wins" rule would reassign forks unpredictably.
// The other columns (message, parent count, committed_at) are content-of-
// the-commit so they're safe to re-sync unconditionally.
const upsertCommitSQL = `
INSERT INTO commits (
    sha, provider, workspace, repo_name, author_email, author_name,
    committed_at, message, parent_count, files_changed, insertions, deletions
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0)
ON CONFLICT(sha) DO UPDATE SET
    provider     = CASE WHEN excluded.repo_name < commits.repo_name THEN excluded.provider  ELSE commits.provider  END,
    workspace    = CASE WHEN excluded.repo_name < commits.repo_name THEN excluded.workspace ELSE commits.workspace END,
    repo_name    = CASE WHEN excluded.repo_name < commits.repo_name THEN excluded.repo_name ELSE commits.repo_name END,
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
