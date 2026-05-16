package analyze

import (
	"context"
	"testing"

	"github.com/StephanSchmidt/loupe/internal/store"
)

func seedTicket(t *testing.T, s *store.Store, id, projectKey string) {
	t.Helper()
	_, err := s.DB().Exec(`
        INSERT INTO tickets (id, project_key, title, created_at)
        VALUES (?, ?, 't', 1700000000)`, id, projectKey)
	if err != nil {
		t.Fatalf("seed ticket %s: %v", id, err)
	}
}

func seedCommitWithMessage(t *testing.T, s *store.Store, sha, repo, message string) {
	t.Helper()
	_, err := s.DB().Exec(`
        INSERT INTO commits (sha, repo_name, author_email, author_name, committed_at, message)
        VALUES (?, ?, 'a@a', 'A', 1700000000, ?)`, sha, repo, message)
	if err != nil {
		t.Fatalf("seed commit %s: %v", sha, err)
	}
}

func TestLinkCommitsToTickets_JiraKeyMatchesAgainstKnownTicket(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedTicket(t, s, "ENG-123", "ENG")
	seedCommitWithMessage(t, s, "c1", "acme/backend", "ENG-123 fix login")

	ctx := context.Background()
	n, err := LinkCommitsToTickets(ctx, s)
	if err != nil {
		t.Fatalf("LinkCommitsToTickets: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d links, want 1", n)
	}

	var got string
	if err := s.DB().QueryRow(`SELECT ticket_id FROM ticket_commits WHERE commit_sha='c1'`).Scan(&got); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if got != "ENG-123" {
		t.Errorf("ticket_id = %q, want ENG-123", got)
	}
}

func TestLinkCommitsToTickets_DoesNotMatchUnknownProjectKey(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedTicket(t, s, "ENG-1", "ENG")
	// FAKE-1 looks valid but isn't in the tickets table — must not link.
	seedCommitWithMessage(t, s, "c1", "acme/backend", "FAKE-1 nothing here")

	ctx := context.Background()
	if _, err := LinkCommitsToTickets(ctx, s); err != nil {
		t.Fatalf("LinkCommitsToTickets: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ticket_commits`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 links for unknown key, got %d", n)
	}
}

func TestLinkCommitsToTickets_LowercaseRejected(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedTicket(t, s, "ENG-1", "ENG")
	seedCommitWithMessage(t, s, "c1", "acme/backend", "feat-1 unrelated") // wrong case

	ctx := context.Background()
	if _, err := LinkCommitsToTickets(ctx, s); err != nil {
		t.Fatalf("LinkCommitsToTickets: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ticket_commits`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("lowercase should not match, got %d links", n)
	}
}

func TestLinkCommitsToTickets_GitHubIssueScopedToRepo(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Issue #42 exists in acme/backend; commit in acme/backend references #42.
	seedTicket(t, s, "acme/backend#42", "acme/backend")
	seedCommitWithMessage(t, s, "c1", "acme/backend", "fix login (closes #42)")

	// Issue #42 also exists in beta/frontend; commit in acme/backend
	// referencing #42 should NOT link to it (bare #N resolves against
	// the commit's own repo).
	seedTicket(t, s, "beta/frontend#42", "beta/frontend")

	ctx := context.Background()
	if _, err := LinkCommitsToTickets(ctx, s); err != nil {
		t.Fatalf("LinkCommitsToTickets: %v", err)
	}

	var got string
	if err := s.DB().QueryRow(`SELECT ticket_id FROM ticket_commits WHERE commit_sha='c1'`).Scan(&got); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if got != "acme/backend#42" {
		t.Errorf("ticket_id = %q, want acme/backend#42", got)
	}
}

func TestLinkCommitsToTickets_ExplicitOwnerRepoCrossesScope(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedTicket(t, s, "beta/frontend#42", "beta/frontend")
	seedCommitWithMessage(t, s, "c1", "acme/backend", "fixes beta/frontend#42")

	ctx := context.Background()
	if _, err := LinkCommitsToTickets(ctx, s); err != nil {
		t.Fatalf("LinkCommitsToTickets: %v", err)
	}
	var got string
	if err := s.DB().QueryRow(`SELECT ticket_id FROM ticket_commits WHERE commit_sha='c1'`).Scan(&got); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if got != "beta/frontend#42" {
		t.Errorf("ticket_id = %q, want beta/frontend#42", got)
	}
}

func TestLinkCommitsToTickets_Idempotent(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedTicket(t, s, "ENG-1", "ENG")
	seedCommitWithMessage(t, s, "c1", "acme/backend", "ENG-1 fix")

	ctx := context.Background()
	if _, err := LinkCommitsToTickets(ctx, s); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := LinkCommitsToTickets(ctx, s); err != nil {
		t.Fatalf("second: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ticket_commits`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("count after re-run = %d, want 1", n)
	}
}

func TestLinkCommitsToTickets_HashNumberWordBoundary(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedTicket(t, s, "acme/backend#1", "acme/backend")
	// "#1000" should not match "acme/backend#1" — word boundary rejects.
	seedCommitWithMessage(t, s, "c1", "acme/backend", "bumped batch size to #1000")

	ctx := context.Background()
	if _, err := LinkCommitsToTickets(ctx, s); err != nil {
		t.Fatalf("LinkCommitsToTickets: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ticket_commits`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 (no word-boundary match), got %d", n)
	}
}
