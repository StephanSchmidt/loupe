package analyze

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
)

// WIPWeek is the end-of-week snapshot of open (created, not yet resolved)
// tickets, split by whether work has started.
type WIPWeek struct {
	WeekStart  time.Time
	InProgress int // open tickets that have entered a "dev started" status
	NotStarted int // open tickets still waiting to start
}

// WeeklyWIP returns one WIPWeek per ISO week spanning the ticket history. A
// ticket is "in progress" from its first transition into a dev-started status
// (reusing the cycle-time config) and drops out once it becomes terminal —
// resolved (resolved_at/closed_at) or a configured done/abandoned status.
func WeeklyWIP(ctx context.Context, s *store.Store, cfg CycleConfig) ([]WIPWeek, error) {
	return WeeklyWIPScoped(ctx, s, cfg, Scope{})
}

// WeeklyWIPScoped is WeeklyWIP restricted to scope.TrackerProject. Scoping the
// tickets query is enough — the transition scan skips any ticket not in the
// loaded set.
func WeeklyWIPScoped(ctx context.Context, s *store.Store, cfg CycleConfig, scope Scope) ([]WIPWeek, error) {
	devWanted := normaliseStatuses(cfg.DevStartedStatuses)
	// Terminal = done OR abandoned: both remove a ticket from WIP. Resolved
	// timestamps below are the primary signal; this set is the fallback when
	// a ticket has no resolution date (e.g. won't-do without a resolution).
	terminal := normaliseStatuses(append(append([]string{}, cfg.DoneStatuses...), cfg.AbandonedStatuses...))

	type ticketState struct {
		created int64
		devAt   int64 // first dev-started transition; 0 = none
		doneAt  int64 // first done transition; 0 = none
	}
	tickets := map[string]*ticketState{}

	tFilt, tArgs := scope.ticketFilter("project_key")
	trows, err := s.DB().QueryContext(ctx, `SELECT id, created_at, resolved_at, closed_at FROM tickets WHERE 1=1`+tFilt, tArgs...)
	if err != nil {
		return nil, fmt.Errorf("load tickets: %w", err)
	}
	for trows.Next() {
		var id string
		var created int64
		var resolved, closed sql.NullInt64
		if err := trows.Scan(&id, &created, &resolved, &closed); err != nil {
			_ = trows.Close()
			return nil, fmt.Errorf("scan ticket: %w", err)
		}
		t := &ticketState{created: created}
		// Prefer the resolution timestamp — it captures every terminal
		// status (Won't Do, Archived, …), not just the ones in our
		// transition done-set. The transition fallback below only fires
		// when neither timestamp is present.
		switch {
		case resolved.Valid:
			t.doneAt = resolved.Int64
		case closed.Valid:
			t.doneAt = closed.Int64
		}
		tickets[id] = t
	}
	if err := trows.Err(); err != nil {
		_ = trows.Close()
		return nil, fmt.Errorf("iterate tickets: %w", err)
	}
	_ = trows.Close()
	if len(tickets) == 0 {
		return nil, nil
	}

	xrows, err := s.DB().QueryContext(ctx, `SELECT ticket_id, at, to_status FROM ticket_transitions ORDER BY at`)
	if err != nil {
		return nil, fmt.Errorf("load transitions: %w", err)
	}
	for xrows.Next() {
		var id, status string
		var at int64
		if err := xrows.Scan(&id, &at, &status); err != nil {
			_ = xrows.Close()
			return nil, fmt.Errorf("scan transition: %w", err)
		}
		t := tickets[id]
		if t == nil {
			continue
		}
		st := strings.ToLower(strings.TrimSpace(status))
		if _, ok := devWanted[st]; ok && t.devAt == 0 {
			t.devAt = at
		}
		if _, ok := terminal[st]; ok && t.doneAt == 0 {
			t.doneAt = at
		}
	}
	if err := xrows.Err(); err != nil {
		_ = xrows.Close()
		return nil, fmt.Errorf("iterate transitions: %w", err)
	}
	_ = xrows.Close()

	var minT, maxT int64
	first := true
	for _, t := range tickets {
		for _, v := range []int64{t.created, t.devAt, t.doneAt} {
			if v == 0 {
				continue
			}
			if first || v < minT {
				minT = v
			}
			if first || v > maxT {
				maxT = v
			}
			first = false
		}
	}
	if first {
		return nil, nil
	}

	startWk := IsoWeekStart(time.Unix(minT, 0))
	endWk := IsoWeekStart(time.Unix(maxT, 0))
	var out []WIPWeek
	for wk := startWk; !wk.After(endWk); wk = wk.AddDate(0, 0, 7) {
		asOf := wk.AddDate(0, 0, 7).Unix()
		var inProgress, notStarted int
		for _, t := range tickets {
			if t.created > asOf {
				continue // not created yet
			}
			if t.doneAt != 0 && t.doneAt <= asOf {
				continue // already resolved
			}
			if t.devAt != 0 && t.devAt <= asOf {
				inProgress++
			} else {
				notStarted++
			}
		}
		out = append(out, WIPWeek{WeekStart: wk, InProgress: inProgress, NotStarted: notStarted})
	}
	return out, nil
}
