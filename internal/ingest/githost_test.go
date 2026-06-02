package ingest

import (
	"context"
	"iter"
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
	// prCommitCalls counts ListPRCommits invocations so tests can assert
	// the per-PR squash-recovery fetch is gated by configuration.
	prCommitCalls int
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
	return func(yield func(githost.Commit, error) bool) {
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
	f.prCommitCalls++
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
		prCommits: map[string]map[string][]githost.Commit{
			"acme/backend": {
				"1": {
					{SHA: "pc1", AuthorEmail: "alice@a", AuthorName: "Alice", CommittedAt: at(1700000050),
						Message: "wip\n\nCo-Authored-By: Claude <noreply@anthropic.com>"},
				},
			},
		},
	}
}

func TestIngestGitHost_SquashRecoveryGate(t *testing.T) {
	// The per-PR commit fetch (squash-merge recovery) is the most expensive
	// part of ingest — one extra API call per PR. It must only happen when
	// GitHostFilter.SquashMergeRecovery is set, so operators can turn it off.
	t.Run("enabled fetches PR commits", func(t *testing.T) {
		s, err := store.Open(store.MemoryPath)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = s.Close() }()
		f := buildFake()
		if _, err := IngestGitHost(context.Background(), s, f, nil, GitHostFilter{SquashMergeRecovery: true}); err != nil {
			t.Fatalf("IngestGitHost: %v", err)
		}
		if f.prCommitCalls == 0 {
			t.Errorf("ListPRCommits was not called with recovery enabled")
		}
		assertCount(t, s, `SELECT COUNT(*) FROM pr_commits`, 1)
	})

	t.Run("disabled skips PR commits", func(t *testing.T) {
		s, err := store.Open(store.MemoryPath)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = s.Close() }()
		f := buildFake()
		if _, err := IngestGitHost(context.Background(), s, f, nil, GitHostFilter{SquashMergeRecovery: false}); err != nil {
			t.Fatalf("IngestGitHost: %v", err)
		}
		if f.prCommitCalls != 0 {
			t.Errorf("ListPRCommits called %d times with recovery disabled, want 0", f.prCommitCalls)
		}
		assertCount(t, s, `SELECT COUNT(*) FROM pr_commits`, 0)
	})
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
