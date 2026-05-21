package ingest

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/StephanSchmidt/loupe/internal/githost"
	"github.com/StephanSchmidt/loupe/internal/store"
)

// fakeGitHost implements githost.GitHost from in-memory data. No HTTP.
type fakeGitHost struct {
	workspaces []githost.Workspace
	repos      map[string][]githost.Repo        // workspace slug → repos
	commits    map[string][]githost.Commit      // "ws/repo" → commits
	prs        map[string][]githost.PullRequest // "ws/repo" → PRs
	prCommits  map[string]map[string][]githost.Commit
	// commitErr, when set for a "ws/repo" key, makes ListCommits yield
	// that error as the very first iteration result — used by tests that
	// simulate a real-world failure mid-baseline (renamed repo 301, ACL
	// drift, transient that exhausted retries, …).
	commitErr map[string]error
}

func (f *fakeGitHost) Name() string { return "fake-vcs" }

func (f *fakeGitHost) ListWorkspaces(_ context.Context) ([]githost.Workspace, error) {
	return f.workspaces, nil
}

func (f *fakeGitHost) ListRepos(_ context.Context, ws string) ([]githost.Repo, error) {
	return f.repos[ws], nil
}

func (f *fakeGitHost) ListCommits(_ context.Context, repo githost.RepoRef, since time.Time) iter.Seq2[githost.Commit, error] {
	commits := f.commits[repo.FullName()]
	err := f.commitErr[repo.FullName()]
	return func(yield func(githost.Commit, error) bool) {
		if err != nil {
			yield(githost.Commit{}, err)
			return
		}
		for _, c := range commits {
			if !since.IsZero() && c.CommittedAt.Before(since) {
				continue
			}
			if !yield(c, nil) {
				return
			}
		}
	}
}

func (f *fakeGitHost) ListPullRequests(_ context.Context, repo githost.RepoRef, since time.Time) iter.Seq2[githost.PullRequest, error] {
	prs := f.prs[repo.FullName()]
	return func(yield func(githost.PullRequest, error) bool) {
		for _, pr := range prs {
			if !since.IsZero() && pr.CreatedAt.Before(since) {
				continue
			}
			if !yield(pr, nil) {
				return
			}
		}
	}
}

func (f *fakeGitHost) ListPRCommits(_ context.Context, repo githost.RepoRef, prID string) ([]githost.Commit, error) {
	return f.prCommits[repo.FullName()][prID], nil
}

func buildFake() *fakeGitHost {
	at := func(n int64) time.Time { return time.Unix(n, 0).UTC() }
	return &fakeGitHost{
		workspaces: []githost.Workspace{
			{Slug: "acme", Name: "Acme"},
			{Slug: "beta", Name: "Beta"},
		},
		repos: map[string][]githost.Repo{
			"acme": {
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "backend"}, Name: "backend"},
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "frontend"}, Name: "frontend"},
			},
			"beta": {
				{RepoRef: githost.RepoRef{Workspace: "beta", Slug: "agent"}, Name: "agent"},
			},
		},
		commits: map[string][]githost.Commit{
			"acme/backend": {
				{SHA: "a1", AuthorEmail: "alice@a", AuthorName: "Alice", CommittedAt: at(1700000300), Message: "feat"},
				{SHA: "a2", AuthorEmail: "alice@a", AuthorName: "Alice", CommittedAt: at(1700000200),
					Message: "fix\n\nCo-Authored-By: Claude <noreply@anthropic.com>"},
			},
			"acme/frontend": {
				{SHA: "f1", AuthorEmail: "bob@a", AuthorName: "Bob", CommittedAt: at(1700000400), Message: "init"},
			},
			"beta/agent": {
				{SHA: "g1", AuthorEmail: "carol@b", AuthorName: "Carol", CommittedAt: at(1700000500), Message: "init"},
			},
		},
		prs: map[string][]githost.PullRequest{
			"acme/backend": {
				{ID: "1", Title: "Add login", State: "MERGED", AuthorEmail: "alice@a",
					SourceBranch: "feat/login", DestinationBranch: "main",
					CreatedAt: at(1700000100), MergeCommitSHA: "merge-a"},
			},
		},
	}
}

func TestIngestGitHost_EndToEnd(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	stats, err := IngestGitHost(context.Background(), s, buildFake(), nil, GitHostFilter{})
	if err != nil {
		t.Fatalf("IngestGitHost: %v", err)
	}

	t.Run("stats counters", func(t *testing.T) {
		want := GitHostStats{Workspaces: 2, Repos: 3, Commits: 4, PullRequests: 1}
		if stats != want {
			t.Errorf("stats = %+v, want %+v", stats, want)
		}
	})

	t.Run("workspaces table populated", func(t *testing.T) {
		assertCount(t, s, `SELECT COUNT(*) FROM workspaces WHERE provider = 'fake-vcs'`, 2)
	})

	t.Run("repos table populated", func(t *testing.T) {
		assertCount(t, s, `SELECT COUNT(*) FROM repos WHERE provider = 'fake-vcs'`, 3)
	})

	t.Run("commit columns carry provider+workspace", func(t *testing.T) {
		var provider, workspace string
		if err := s.DB().QueryRow(`SELECT provider, workspace FROM commits WHERE sha = 'a1'`).Scan(&provider, &workspace); err != nil {
			t.Fatalf("read commit a1: %v", err)
		}
		if provider != "fake-vcs" || workspace != "acme" {
			t.Errorf("commit a1 columns: provider=%q workspace=%q", provider, workspace)
		}
	})

	t.Run("trailer body preserved", func(t *testing.T) {
		var msg string
		if err := s.DB().QueryRow(`SELECT message FROM commits WHERE sha = 'a2'`).Scan(&msg); err != nil {
			t.Fatalf("read commit a2: %v", err)
		}
		if !contains(msg, "Co-Authored-By: Claude") {
			t.Errorf("a2 message missing trailer: %q", msg)
		}
	})

	t.Run("PR columns carry provider+workspace", func(t *testing.T) {
		var provider, workspace string
		if err := s.DB().QueryRow(`SELECT provider, workspace FROM prs WHERE id = 'fake-vcs:acme/backend#1'`).Scan(&provider, &workspace); err != nil {
			t.Fatalf("read pr 1: %v", err)
		}
		if provider != "fake-vcs" || workspace != "acme" {
			t.Errorf("pr 1 columns: provider=%q workspace=%q", provider, workspace)
		}
	})

	t.Run("watermarks advanced", func(t *testing.T) {
		var wm int64
		if err := s.DB().QueryRow(`SELECT last_commit_indexed_at FROM repos WHERE full_name = 'acme/backend'`).Scan(&wm); err != nil {
			t.Fatalf("read watermark: %v", err)
		}
		if wm == 0 {
			t.Errorf("watermark not advanced for acme/backend")
		}
	})
}

func TestIngestGitHost_RepoFilterSkipsOthers(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	stats, err := IngestGitHost(context.Background(), s, buildFake(), nil, GitHostFilter{Repo: "acme/backend"})
	if err != nil {
		t.Fatalf("IngestGitHost: %v", err)
	}
	// Filter excludes acme/frontend and the whole beta workspace.
	want := GitHostStats{Workspaces: 1, Repos: 1, Commits: 2, PullRequests: 1}
	if stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	assertCount(t, s, `SELECT COUNT(*) FROM repos WHERE full_name = 'acme/backend'`, 1)
	assertCount(t, s, `SELECT COUNT(*) FROM repos WHERE full_name = 'acme/frontend'`, 0)
	assertCount(t, s, `SELECT COUNT(*) FROM commits WHERE repo_name = 'beta/agent'`, 0)
}

// TestIngestGitHost_SkipsArchivedRepos verifies that archived repos are
// skipped before any commit/PR fetch, logged to progressOut, and counted
// in stats.ReposSkippedArch — so a workspace of 2000 mostly-archived repos
// doesn't burn API budget on read-only history.
func TestIngestGitHost_SkipsArchivedRepos(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	createdAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeGitHost{
		workspaces: []githost.Workspace{{Slug: "acme", Name: "Acme"}},
		repos: map[string][]githost.Repo{
			"acme": {
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "live"}, Name: "live"},
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "old"}, Name: "old", Archived: true},
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "ancient"}, Name: "ancient", Archived: true},
			},
		},
		commits: map[string][]githost.Commit{
			"acme/live":    {{SHA: "l1", AuthorEmail: "x@y", AuthorName: "X", CommittedAt: createdAt, Message: "ok"}},
			"acme/old":     {{SHA: "o1", AuthorEmail: "x@y", AuthorName: "X", CommittedAt: createdAt, Message: "must not be fetched"}},
			"acme/ancient": {{SHA: "a1", AuthorEmail: "x@y", AuthorName: "X", CommittedAt: createdAt, Message: "must not be fetched"}},
		},
	}

	var progress strings.Builder
	stats, err := IngestGitHost(context.Background(), s, fake, &progress, GitHostFilter{})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if stats.Repos != 1 {
		t.Errorf("stats.Repos = %d, want 1", stats.Repos)
	}
	if stats.ReposSkippedArch != 2 {
		t.Errorf("stats.ReposSkippedArch = %d, want 2", stats.ReposSkippedArch)
	}
	if stats.Commits != 1 {
		t.Errorf("stats.Commits = %d, want 1 (archived repos must not be fetched)", stats.Commits)
	}

	assertCount(t, s, `SELECT COUNT(*) FROM repos WHERE full_name = 'acme/old'`, 0)
	assertCount(t, s, `SELECT COUNT(*) FROM repos WHERE full_name = 'acme/ancient'`, 0)
	assertCount(t, s, `SELECT COUNT(*) FROM commits WHERE sha IN ('o1', 'a1')`, 0)

	out := progress.String()
	if !contains(out, "acme/old: skipped (archived)") {
		t.Errorf("progress out missing skip log for acme/old: %q", out)
	}
	if !contains(out, "acme/ancient: skipped (archived)") {
		t.Errorf("progress out missing skip log for acme/ancient: %q", out)
	}
}

// TestIngestGitHost_PerRepoFailureIsResumable simulates the user-observed
// 24h-into-baseline failure: one repo in the middle of the workspace
// errors out (here a synthetic 301-equivalent). The run must:
//   - keep going past the bad repo,
//   - leave the bad repo's commit watermark unset (so the next baseline
//     retries it),
//   - persist the other repos' watermarks (so the next baseline skips
//     them),
//   - report a non-nil error so the caller knows the run was partial,
//   - log the failure to progressOut for the operator.
func TestIngestGitHost_PerRepoFailureIsResumable(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeGitHost{
		workspaces: []githost.Workspace{{Slug: "acme", Name: "Acme"}},
		repos: map[string][]githost.Repo{
			"acme": {
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "first"}, Name: "first"},
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "broken"}, Name: "broken"},
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "third"}, Name: "third"},
			},
		},
		commits: map[string][]githost.Commit{
			"acme/first": {{SHA: "x1", AuthorEmail: "x@y", AuthorName: "X", CommittedAt: at, Message: "ok"}},
			"acme/third": {{SHA: "x3", AuthorEmail: "x@y", AuthorName: "X", CommittedAt: at, Message: "ok"}},
		},
		commitErr: map[string]error{
			"acme/broken": errMovedPermanently,
		},
	}

	var progress strings.Builder
	stats, err := IngestGitHost(context.Background(), s, fake, &progress, GitHostFilter{})
	if err == nil {
		t.Fatal("expected non-nil error for partial-failure run")
	}
	if !contains(err.Error(), "acme/broken") {
		t.Errorf("error %q does not mention failed repo", err)
	}

	if stats.ReposFailed != 1 {
		t.Errorf("stats.ReposFailed = %d, want 1", stats.ReposFailed)
	}
	if stats.Commits != 2 {
		t.Errorf("stats.Commits = %d, want 2 (first+third still ingested)", stats.Commits)
	}

	// Failed repo: row inserted (we upsert before streaming) but commit
	// watermark stays NULL/0 so the next baseline retries from the
	// beginning.
	var wm int64
	row := s.DB().QueryRow(`SELECT COALESCE(last_commit_indexed_at, 0) FROM repos WHERE full_name = 'acme/broken'`)
	if err := row.Scan(&wm); err != nil {
		t.Fatalf("read broken watermark: %v", err)
	}
	if wm != 0 {
		t.Errorf("acme/broken watermark = %d, want 0 (so next run retries)", wm)
	}

	// Completed repos: watermark set so the next baseline skips them.
	row = s.DB().QueryRow(`SELECT last_commit_indexed_at FROM repos WHERE full_name = 'acme/first'`)
	if err := row.Scan(&wm); err != nil || wm == 0 {
		t.Errorf("acme/first watermark = %d (err=%v), want non-zero", wm, err)
	}
	row = s.DB().QueryRow(`SELECT last_commit_indexed_at FROM repos WHERE full_name = 'acme/third'`)
	if err := row.Scan(&wm); err != nil || wm == 0 {
		t.Errorf("acme/third watermark = %d (err=%v), want non-zero", wm, err)
	}

	out := progress.String()
	if !contains(out, "acme/broken: FAILED") {
		t.Errorf("progress output missing FAILED line: %q", out)
	}
}

// errMovedPermanently mirrors the error message shape callers see after
// the github client exhausts its redirect-follow budget (or hits a
// non-redirect 301 path). Used as a synthetic stand-in so the ingest
// test doesn't need a live HTTP server.
var errMovedPermanently = errMoved("github GET /repos/acme/broken/commits returned 301: Moved Permanently")

type errMoved string

func (e errMoved) Error() string { return string(e) }

func TestIngestGitHost_RepoFilterRejectsBadFormat(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	_, err = IngestGitHost(context.Background(), s, buildFake(), nil, GitHostFilter{Repo: "no-slash"})
	if err == nil || !contains(err.Error(), "workspace/slug") {
		t.Errorf("expected workspace/slug error, got %v", err)
	}
}

func assertCount(t *testing.T, s *store.Store, query string, want int) {
	t.Helper()
	var got int
	if err := s.DB().QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if got != want {
		t.Errorf("count %q = %d, want %d", query, got, want)
	}
}

func TestIngestGitHost_IdempotentReruns(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	fake := buildFake()
	if _, err := IngestGitHost(context.Background(), s, fake, nil, GitHostFilter{}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if _, err := IngestGitHost(context.Background(), s, fake, nil, GitHostFilter{}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	var nCommits, nPRs, nRepos int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM commits`).Scan(&nCommits)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM prs`).Scan(&nPRs)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM repos`).Scan(&nRepos)
	if nCommits != 4 || nPRs != 1 || nRepos != 3 {
		t.Errorf("after re-ingest: commits=%d prs=%d repos=%d (want 4/1/3 — no duplicates)", nCommits, nPRs, nRepos)
	}
}

// TestIngestGitHost_SharedSHAKeepsFirstAttribution guards against a bug
// where ingesting a fork (same SHA, different repo) silently overwrote
// the original repo's attribution columns.
func TestIngestGitHost_SharedSHAKeepsFirstAttribution(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	createdAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	shared := githost.Commit{SHA: "deadbeef", AuthorEmail: "x@y", AuthorName: "X", CommittedAt: createdAt, Message: "shared"}
	fake := &fakeGitHost{
		workspaces: []githost.Workspace{{Slug: "acme", Name: "Acme"}},
		repos: map[string][]githost.Repo{
			"acme": {
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "origin"}, Name: "origin"},
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "fork"}, Name: "fork"},
			},
		},
		commits: map[string][]githost.Commit{
			"acme/origin": {shared},
			"acme/fork":   {shared},
		},
	}
	if _, err := IngestGitHost(context.Background(), s, fake, nil, GitHostFilter{}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var repo string
	if err := s.DB().QueryRow(`SELECT repo_name FROM commits WHERE sha = 'deadbeef'`).Scan(&repo); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if repo != "acme/origin" {
		t.Errorf("repo_name = %q, want acme/origin (first-write wins for shared SHAs)", repo)
	}
}

// TestIngestGitHost_PR_IDsAreScopedAcrossRepos guards against a bug where
// PR #1 in repo A overwrote PR #1 in repo B because both produced the
// same `prs.id`.
func TestIngestGitHost_PR_IDsAreScopedAcrossRepos(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	createdAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeGitHost{
		workspaces: []githost.Workspace{{Slug: "acme", Name: "Acme"}},
		repos: map[string][]githost.Repo{
			"acme": {
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "alpha"}, Name: "alpha"},
				{RepoRef: githost.RepoRef{Workspace: "acme", Slug: "beta"}, Name: "beta"},
			},
		},
		commits: map[string][]githost.Commit{
			"acme/alpha": {{SHA: "a1", AuthorEmail: "x@y", CommittedAt: createdAt, Message: "a"}},
			"acme/beta":  {{SHA: "b1", AuthorEmail: "x@y", CommittedAt: createdAt, Message: "b"}},
		},
		prs: map[string][]githost.PullRequest{
			"acme/alpha": {{ID: "1", Title: "alpha-1", State: "MERGED", AuthorEmail: "x@y", CreatedAt: createdAt}},
			"acme/beta":  {{ID: "1", Title: "beta-1", State: "MERGED", AuthorEmail: "x@y", CreatedAt: createdAt}},
		},
	}
	if _, err := IngestGitHost(context.Background(), s, fake, nil, GitHostFilter{}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM prs`).Scan(&n); err != nil {
		t.Fatalf("count prs: %v", err)
	}
	if n != 2 {
		t.Fatalf("got %d PRs, want 2 (one per repo — same raw id, different scopes)", n)
	}

	var alphaTitle, betaTitle string
	_ = s.DB().QueryRow(`SELECT title FROM prs WHERE repo_name = 'acme/alpha'`).Scan(&alphaTitle)
	_ = s.DB().QueryRow(`SELECT title FROM prs WHERE repo_name = 'acme/beta'`).Scan(&betaTitle)
	if alphaTitle != "alpha-1" || betaTitle != "beta-1" {
		t.Errorf("titles got (%q, %q), want (alpha-1, beta-1) — second ingest must not overwrite first", alphaTitle, betaTitle)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
