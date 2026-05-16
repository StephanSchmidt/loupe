package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
)

func cycleSeedTicket(t *testing.T, s *store.Store, id string, createdAt time.Time) {
	t.Helper()
	if _, err := s.DB().Exec(`
        INSERT INTO tickets (id, project_key, title, created_at)
        VALUES (?, 'P', 't', ?)`, id, createdAt.Unix()); err != nil {
		t.Fatalf("seed ticket %s: %v", id, err)
	}
}

func cycleSeedTransition(t *testing.T, s *store.Store, ticketID string, at time.Time, to string) {
	t.Helper()
	if _, err := s.DB().Exec(`
        INSERT INTO ticket_transitions (ticket_id, at, from_status, to_status)
        VALUES (?, ?, '', ?)`, ticketID, at.Unix(), to); err != nil {
		t.Fatalf("seed transition: %v", err)
	}
}

func cycleSeedCommit(t *testing.T, s *store.Store, sha string, at time.Time) {
	t.Helper()
	if _, err := s.DB().Exec(`
        INSERT INTO commits (sha, repo_name, author_email, author_name, committed_at, message)
        VALUES (?, 'r', 'a@a', 'A', ?, '')`, sha, at.Unix()); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

func cycleSeedLink(t *testing.T, s *store.Store, ticketID, sha string) {
	t.Helper()
	if _, err := s.DB().Exec(`
        INSERT INTO ticket_commits (ticket_id, commit_sha, join_method)
        VALUES (?, ?, 'commit_msg')`, ticketID, sha); err != nil {
		t.Fatalf("seed link: %v", err)
	}
}

func cycleDefaultConfig() CycleConfig {
	return CycleConfig{DevStartedStatuses: []string{"In Progress", "In Review"}}
}

func TestComputeCycles_TransitionPath(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	created := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	started := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	last := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)

	cycleSeedTicket(t, s, "ENG-1", created)
	cycleSeedTransition(t, s, "ENG-1", started, "In Progress")
	cycleSeedCommit(t, s, "c1", last)
	cycleSeedLink(t, s, "ENG-1", "c1")

	cycles, err := ComputeCycles(context.Background(), s, cycleDefaultConfig())
	if err != nil {
		t.Fatalf("ComputeCycles: %v", err)
	}
	if len(cycles) != 1 {
		t.Fatalf("got %d cycles, want 1", len(cycles))
	}
	c := cycles[0]
	if c.DevStartedReason != DevStartedReasonTransition {
		t.Errorf("reason = %q, want transition", c.DevStartedReason)
	}
	if c.IdeaToDev != 2*24*time.Hour {
		t.Errorf("IdeaToDev = %v, want 48h", c.IdeaToDev)
	}
	if c.DevToRelease != 4*24*time.Hour {
		t.Errorf("DevToRelease = %v, want 96h", c.DevToRelease)
	}
}

func TestComputeCycles_FirstCommitFallback(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	created := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	c1At := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	c2At := time.Date(2026, 5, 8, 9, 0, 0, 0, time.UTC)

	cycleSeedTicket(t, s, "ENG-1", created)
	// No transition at all — must fall back to first commit.
	cycleSeedCommit(t, s, "c1", c1At)
	cycleSeedCommit(t, s, "c2", c2At)
	cycleSeedLink(t, s, "ENG-1", "c1")
	cycleSeedLink(t, s, "ENG-1", "c2")

	cycles, err := ComputeCycles(context.Background(), s, cycleDefaultConfig())
	if err != nil {
		t.Fatalf("ComputeCycles: %v", err)
	}
	if len(cycles) != 1 {
		t.Fatalf("got %d cycles, want 1", len(cycles))
	}
	c := cycles[0]
	if c.DevStartedReason != DevStartedReasonFirstCommit {
		t.Errorf("reason = %q, want first_commit", c.DevStartedReason)
	}
	if c.IdeaToDev != 3*24*time.Hour {
		t.Errorf("IdeaToDev = %v, want 72h (created → first commit)", c.IdeaToDev)
	}
	if c.DevToRelease != 4*24*time.Hour {
		t.Errorf("DevToRelease = %v, want 96h (first commit → last commit)", c.DevToRelease)
	}
}

func TestComputeCycles_SkipsTicketWithoutCommits(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	cycleSeedTicket(t, s, "ENG-1", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	// transition exists but no commits → skip
	cycleSeedTransition(t, s, "ENG-1", time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC), "In Progress")

	cycles, err := ComputeCycles(context.Background(), s, cycleDefaultConfig())
	if err != nil {
		t.Fatalf("ComputeCycles: %v", err)
	}
	if len(cycles) != 0 {
		t.Errorf("got %d cycles, want 0 (no completion data)", len(cycles))
	}
}

func TestComputeCycles_StatusMatchIsCaseInsensitive(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	created := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	started := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)

	cycleSeedTicket(t, s, "ENG-1", created)
	cycleSeedTransition(t, s, "ENG-1", started, "in progress") // lowercase
	cycleSeedCommit(t, s, "c1", last)
	cycleSeedLink(t, s, "ENG-1", "c1")

	cycles, _ := ComputeCycles(context.Background(), s, cycleDefaultConfig())
	if len(cycles) != 1 || cycles[0].DevStartedReason != DevStartedReasonTransition {
		t.Fatalf("expected transition-based cycle, got %+v", cycles)
	}
}

func TestComputeCycles_NegativeClampedToZero(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Ticket created AFTER its first transition (data oddity from a
	// backfilled ticket). Cycle must clamp to zero rather than going
	// negative.
	created := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	started := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)

	cycleSeedTicket(t, s, "ENG-1", created)
	cycleSeedTransition(t, s, "ENG-1", started, "In Progress")
	cycleSeedCommit(t, s, "c1", last)
	cycleSeedLink(t, s, "ENG-1", "c1")

	cycles, _ := ComputeCycles(context.Background(), s, cycleDefaultConfig())
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(cycles))
	}
	if cycles[0].IdeaToDev != 0 {
		t.Errorf("IdeaToDev = %v, want 0 (negative clamp)", cycles[0].IdeaToDev)
	}
}

func TestWeeklyCycles_BucketsByCompletionWeek(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	created := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC) // Mon (wk 18 starts)
	wk19 := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	wk20 := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)

	// Ticket A: completes in wk 19
	cycleSeedTicket(t, s, "A-1", created)
	cycleSeedTransition(t, s, "A-1", wk19.Add(-24*time.Hour), "In Progress")
	cycleSeedCommit(t, s, "c-a", wk19.Add(48*time.Hour))
	cycleSeedLink(t, s, "A-1", "c-a")

	// Ticket B: completes in wk 20
	cycleSeedTicket(t, s, "B-1", created)
	cycleSeedTransition(t, s, "B-1", wk20.Add(-24*time.Hour), "In Progress")
	cycleSeedCommit(t, s, "c-b", wk20.Add(48*time.Hour))
	cycleSeedLink(t, s, "B-1", "c-b")

	weeks, err := WeeklyCycles(context.Background(), s, cycleDefaultConfig())
	if err != nil {
		t.Fatalf("WeeklyCycles: %v", err)
	}
	if len(weeks) != 2 {
		t.Fatalf("got %d weeks, want 2: %+v", len(weeks), weeks)
	}
	if !weeks[0].WeekStart.Before(weeks[1].WeekStart) {
		t.Errorf("weeks not sorted ascending: %+v", weeks)
	}
}

func TestWeeklyCycles_CountsFallback(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	created := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	wk := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)

	// One transition-based + one fallback-based ticket in the same week.
	cycleSeedTicket(t, s, "T-1", created)
	cycleSeedTransition(t, s, "T-1", wk.Add(-3*24*time.Hour), "In Progress")
	cycleSeedCommit(t, s, "c-1", wk)
	cycleSeedLink(t, s, "T-1", "c-1")

	cycleSeedTicket(t, s, "T-2", created)
	cycleSeedCommit(t, s, "c-2", wk)
	cycleSeedLink(t, s, "T-2", "c-2")

	weeks, _ := WeeklyCycles(context.Background(), s, cycleDefaultConfig())
	if len(weeks) != 1 {
		t.Fatalf("expected 1 week, got %d", len(weeks))
	}
	if weeks[0].TicketCount != 2 {
		t.Errorf("TicketCount = %d, want 2", weeks[0].TicketCount)
	}
	if weeks[0].FallbackTicketCount != 1 {
		t.Errorf("FallbackTicketCount = %d, want 1", weeks[0].FallbackTicketCount)
	}
}
