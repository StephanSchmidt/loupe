package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
	"github.com/StephanSchmidt/loupe/internal/tracker"
)

// TrackerStats is the summary returned by IngestTracker.
type TrackerStats struct {
	Projects int
	Issues   int
}

// TrackerFilter restricts which projects get ingested. The zero value
// means "ingest everything" (the default baseline behaviour).
type TrackerFilter struct {
	// Project is the tracker project key — when set, only that project's
	// issues are fetched. For GitHub the key is "owner/repo"; for Jira
	// it is the project key (e.g. "ENG").
	Project string
}

// IngestTracker walks t's projects → issues and persists rows into s.
// Per-project watermark advances at the end of each project's loop body.
func IngestTracker(ctx context.Context, s *store.Store, t tracker.Tracker, progressOut io.Writer, filter TrackerFilter) (TrackerStats, error) {
	var stats TrackerStats
	provider := t.Name()
	now := time.Now().UTC().Unix()

	projects, err := t.ListProjects(ctx)
	if err != nil {
		return stats, fmt.Errorf("list projects: %w", err)
	}

	for _, p := range projects {
		if filter.Project != "" && p.Key != filter.Project {
			continue
		}
		if err := upsertProject(ctx, s.DB(), provider, p, now); err != nil {
			return stats, err
		}
		stats.Projects++

		since, err := readProjectWatermark(ctx, s.DB(), provider, p.Key)
		if err != nil {
			return stats, err
		}
		nIssues := 0
		for iss, streamErr := range t.ListIssues(ctx, p.Key, since) {
			if streamErr != nil {
				return stats, fmt.Errorf("stream issues %s: %w", p.Key, streamErr)
			}
			if err := upsertIssue(ctx, s.DB(), provider, iss); err != nil {
				return stats, err
			}
			if err := upsertTicketTransitions(ctx, s.DB(), iss); err != nil {
				return stats, err
			}
			nIssues++
		}
		stats.Issues += nIssues

		// Watermark = end-of-this-project's-ingest, not run-start. An issue
		// updated between run-start and the moment ListIssues actually
		// returned would otherwise be skipped on the next baseline.
		if err := advanceProjectWatermark(ctx, s.DB(), provider, p.Key, time.Now().UTC().Unix()); err != nil {
			return stats, err
		}
		if progressOut != nil {
			_, _ = fmt.Fprintf(progressOut, "    %s: %d issues\n", p.Key, nIssues)
		}
	}

	return stats, nil
}

const upsertProjectSQL = `
INSERT INTO tracker_projects (provider, key, name, discovered_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(provider, key) DO UPDATE SET name = excluded.name
`

func upsertProject(ctx context.Context, db *sql.DB, provider string, p tracker.Project, discoveredAt int64) error {
	_, err := db.ExecContext(ctx, upsertProjectSQL, provider, p.Key, p.Name, discoveredAt)
	if err != nil {
		return fmt.Errorf("upsert project %s: %w", p.Key, err)
	}
	return nil
}

const advanceProjectWatermarkSQL = `UPDATE tracker_projects SET last_issue_indexed_at = ? WHERE provider = ? AND key = ?`

func advanceProjectWatermark(ctx context.Context, db *sql.DB, provider, key string, at int64) error {
	_, err := db.ExecContext(ctx, advanceProjectWatermarkSQL, at, provider, key)
	return err
}

const readProjectWatermarkSQL = `SELECT last_issue_indexed_at FROM tracker_projects WHERE provider = ? AND key = ?`

func readProjectWatermark(ctx context.Context, db *sql.DB, provider, key string) (time.Time, error) {
	var ts sql.NullInt64
	err := db.QueryRowContext(ctx, readProjectWatermarkSQL, provider, key).Scan(&ts)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read watermark for %s: %w", key, err)
	}
	if !ts.Valid {
		return time.Time{}, nil
	}
	return time.Unix(ts.Int64, 0).UTC(), nil
}

const upsertIssueSQL = `
INSERT INTO tickets (
    id, provider, project_key, title, type, status,
    created_at, resolved_at, closed_at, assignee_email, estimate
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    provider       = excluded.provider,
    project_key    = excluded.project_key,
    title          = excluded.title,
    type           = excluded.type,
    status         = excluded.status,
    resolved_at    = excluded.resolved_at,
    closed_at      = excluded.closed_at,
    assignee_email = excluded.assignee_email,
    estimate       = excluded.estimate
`

func upsertIssue(ctx context.Context, db *sql.DB, provider string, iss tracker.Issue) error {
	var resolvedAt, closedAt sql.NullInt64
	if iss.ResolvedAt != nil {
		resolvedAt = sql.NullInt64{Int64: iss.ResolvedAt.Unix(), Valid: true}
	}
	if iss.ClosedAt != nil {
		closedAt = sql.NullInt64{Int64: iss.ClosedAt.Unix(), Valid: true}
	}
	// Persist by human-readable key (e.g. "ENG-123"), not numeric ID —
	// matches how tickets are referenced in commit messages and PR titles.
	id := iss.Key
	if id == "" {
		id = iss.ID
	}
	_, err := db.ExecContext(ctx, upsertIssueSQL,
		id, provider, iss.ProjectKey, iss.Title, iss.Type, iss.Status,
		iss.CreatedAt.Unix(), resolvedAt, closedAt, iss.AssigneeEmail, iss.Estimate,
	)
	if err != nil {
		return fmt.Errorf("upsert issue %s: %w", id, err)
	}
	return nil
}

const upsertTicketTransitionSQL = `
INSERT INTO ticket_transitions (ticket_id, at, from_status, to_status)
VALUES (?, ?, ?, ?)
ON CONFLICT(ticket_id, at, to_status) DO UPDATE SET
    from_status = excluded.from_status
`

// upsertTicketTransitions persists each status change emitted by the
// tracker. Idempotent — the (ticket_id, at, to_status) primary key
// already deduplicates re-ingests.
func upsertTicketTransitions(ctx context.Context, db *sql.DB, iss tracker.Issue) error {
	if len(iss.Transitions) == 0 {
		return nil
	}
	id := iss.Key
	if id == "" {
		id = iss.ID
	}
	for _, tr := range iss.Transitions {
		if tr.At.IsZero() {
			continue
		}
		if _, err := db.ExecContext(ctx, upsertTicketTransitionSQL,
			id, tr.At.Unix(), tr.FromStatus, tr.ToStatus); err != nil {
			return fmt.Errorf("upsert transition %s @ %s: %w", id, tr.At, err)
		}
	}
	return nil
}
