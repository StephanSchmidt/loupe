package analyze

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/montanaflynn/stats"

	"github.com/StephanSchmidt/loupe/internal/store"
)

const (
	// DevStartedReasonTransition means dev-started came from a tracker
	// status change matching the configured "in progress" list.
	DevStartedReasonTransition = "transition"
	// DevStartedReasonFirstCommit means dev-started came from the
	// timestamp of the first commit linked to the ticket — the fallback
	// when no matching status transition exists.
	DevStartedReasonFirstCommit = "first_commit"
)

// TicketCycle is the per-ticket cycle-time record. IdeaToDev is created
// → dev started; DevToRelease is dev started → last linked commit. Both
// are clamped at zero on the (rare) case where a transition lands after
// the last commit.
type TicketCycle struct {
	TicketID         string
	CreatedAt        time.Time
	DevStartedAt     time.Time
	LastDevAt        time.Time
	IdeaToDev        time.Duration
	DevToRelease     time.Duration
	DevStartedReason string
}

// WeekCycle is the per-ISO-week aggregate. Bucketed by the week of
// LastDevAt — i.e. when the ticket finished, mirroring how DORA-style
// cycle-time charts are usually drawn.
type WeekCycle struct {
	WeekStart           time.Time
	TicketCount         int
	FallbackTicketCount int

	MedianIdeaToDev    time.Duration
	P10IdeaToDev       time.Duration
	P90IdeaToDev       time.Duration
	MedianDevToRelease time.Duration
	P10DevToRelease    time.Duration
	P90DevToRelease    time.Duration
}

// CycleConfig is the slice of tracker statuses (case-insensitive) that
// mark "development started". Passed in by callers so analyze stays free
// of an import on the config package.
type CycleConfig struct {
	DevStartedStatuses []string
}

// ComputeCycles produces one TicketCycle per ticket that has enough
// linked data to measure (at least one linked commit; dev_started from
// either a matching transition or fallback to first commit). Tickets
// without any linked commits are dropped — there's no "release" moment
// to bound the cycle.
func ComputeCycles(ctx context.Context, s *store.Store, cfg CycleConfig) ([]TicketCycle, error) {
	wanted := normaliseStatuses(cfg.DevStartedStatuses)

	tickets, err := loadTicketsForCycle(ctx, s.DB())
	if err != nil {
		return nil, err
	}
	devStartByTicket, err := loadDevStartTransitions(ctx, s.DB(), wanted)
	if err != nil {
		return nil, err
	}
	firstCommit, lastCommit, err := loadTicketCommitBounds(ctx, s.DB())
	if err != nil {
		return nil, err
	}

	out := make([]TicketCycle, 0, len(tickets))
	for _, t := range tickets {
		last, hasCommit := lastCommit[t.id]
		if !hasCommit {
			continue
		}
		var devStarted time.Time
		var reason string
		if ts, ok := devStartByTicket[t.id]; ok {
			devStarted = ts
			reason = DevStartedReasonTransition
		} else if first, ok := firstCommit[t.id]; ok {
			devStarted = first
			reason = DevStartedReasonFirstCommit
		} else {
			continue
		}
		ideaToDev := devStarted.Sub(t.createdAt)
		if ideaToDev < 0 {
			ideaToDev = 0
		}
		devToRelease := last.Sub(devStarted)
		if devToRelease < 0 {
			devToRelease = 0
		}
		out = append(out, TicketCycle{
			TicketID:         t.id,
			CreatedAt:        t.createdAt,
			DevStartedAt:     devStarted,
			LastDevAt:        last,
			IdeaToDev:        ideaToDev,
			DevToRelease:     devToRelease,
			DevStartedReason: reason,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastDevAt.Before(out[j].LastDevAt) })
	return out, nil
}

// WeeklyCycles buckets cycles by ISO week of LastDevAt and returns one
// row per week with median + p10/p90 for both segments.
func WeeklyCycles(ctx context.Context, s *store.Store, cfg CycleConfig) ([]WeekCycle, error) {
	cycles, err := ComputeCycles(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	if len(cycles) == 0 {
		return nil, nil
	}
	type bucket struct {
		ideaToDev    []float64
		devToRelease []float64
		fallback     int
	}
	groups := map[time.Time]*bucket{}
	for _, c := range cycles {
		k := IsoWeekStart(c.LastDevAt)
		b, ok := groups[k]
		if !ok {
			b = &bucket{}
			groups[k] = b
		}
		b.ideaToDev = append(b.ideaToDev, c.IdeaToDev.Hours())
		b.devToRelease = append(b.devToRelease, c.DevToRelease.Hours())
		if c.DevStartedReason == DevStartedReasonFirstCommit {
			b.fallback++
		}
	}

	out := make([]WeekCycle, 0, len(groups))
	for week, b := range groups {
		out = append(out, WeekCycle{
			WeekStart:           week,
			TicketCount:         len(b.ideaToDev),
			FallbackTicketCount: b.fallback,
			MedianIdeaToDev:     hoursToDuration(percentile(b.ideaToDev, 50)),
			P10IdeaToDev:        hoursToDuration(percentile(b.ideaToDev, 10)),
			P90IdeaToDev:        hoursToDuration(percentile(b.ideaToDev, 90)),
			MedianDevToRelease:  hoursToDuration(percentile(b.devToRelease, 50)),
			P10DevToRelease:     hoursToDuration(percentile(b.devToRelease, 10)),
			P90DevToRelease:     hoursToDuration(percentile(b.devToRelease, 90)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WeekStart.Before(out[j].WeekStart) })
	return out, nil
}

func normaliseStatuses(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

type ticketForCycle struct {
	id        string
	createdAt time.Time
}

func loadTicketsForCycle(ctx context.Context, db *sql.DB) ([]ticketForCycle, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, created_at FROM tickets`)
	if err != nil {
		return nil, fmt.Errorf("load tickets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ticketForCycle
	for rows.Next() {
		var t ticketForCycle
		var ts int64
		if err := rows.Scan(&t.id, &ts); err != nil {
			return nil, fmt.Errorf("scan ticket: %w", err)
		}
		t.createdAt = time.Unix(ts, 0).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// loadDevStartTransitions returns ticket_id → earliest transition `at`
// matching the configured "dev started" status set. Reads every
// ticket_transitions row and reduces in-memory rather than per-ticket SQL
// — the table is small (one row per status change) and a single scan is
// much faster than N queries.
func loadDevStartTransitions(ctx context.Context, db *sql.DB, wanted map[string]struct{}) (map[string]time.Time, error) {
	if len(wanted) == 0 {
		return map[string]time.Time{}, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT ticket_id, at, to_status FROM ticket_transitions ORDER BY at`)
	if err != nil {
		return nil, fmt.Errorf("load transitions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, status string
		var ts int64
		if err := rows.Scan(&id, &ts, &status); err != nil {
			return nil, fmt.Errorf("scan transition: %w", err)
		}
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(status))]; !ok {
			continue
		}
		if _, exists := out[id]; exists {
			// Already have an earlier transition (rows sorted by at ASC).
			continue
		}
		out[id] = time.Unix(ts, 0).UTC()
	}
	return out, rows.Err()
}

// loadTicketCommitBounds returns (first-commit-time, last-commit-time)
// per ticket across every linked commit in ticket_commits.
func loadTicketCommitBounds(ctx context.Context, db *sql.DB) (first, last map[string]time.Time, err error) {
	rows, err := db.QueryContext(ctx, `
        SELECT tc.ticket_id,
               MIN(c.committed_at) AS first_at,
               MAX(c.committed_at) AS last_at
        FROM ticket_commits tc
        JOIN commits c ON c.sha = tc.commit_sha
        GROUP BY tc.ticket_id
    `)
	if err != nil {
		return nil, nil, fmt.Errorf("load ticket commit bounds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	first = map[string]time.Time{}
	last = map[string]time.Time{}
	for rows.Next() {
		var id string
		var firstTS, lastTS int64
		if err := rows.Scan(&id, &firstTS, &lastTS); err != nil {
			return nil, nil, fmt.Errorf("scan bounds: %w", err)
		}
		first[id] = time.Unix(firstTS, 0).UTC()
		last[id] = time.Unix(lastTS, 0).UTC()
	}
	return first, last, rows.Err()
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	v, err := stats.Percentile(values, p)
	if err != nil {
		return 0
	}
	return v
}

func hoursToDuration(h float64) time.Duration {
	return time.Duration(h * float64(time.Hour))
}
