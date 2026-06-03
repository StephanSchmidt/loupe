package analyze

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
)

// DefectWeek is the per-ISO-week quality counterweight: revert rate (from
// commit messages) and bug rate (from Jira issue types). Both are floors —
// reverts miss roll-forward fixes and squash-hidden reverts; bug counts
// depend on filing discipline and lag the work that caused them.
type DefectWeek struct {
	WeekStart time.Time
	Commits   int
	Reverts   int
	Tickets   int
	Bugs      int
}

// RevertRate is reverts / commits as a percentage (0 if no commits).
func (d DefectWeek) RevertRate() float64 {
	if d.Commits == 0 {
		return 0
	}
	return float64(d.Reverts) / float64(d.Commits) * 100
}

// BugRate is bug tickets / all tickets created that week, as a percentage.
func (d DefectWeek) BugRate() float64 {
	if d.Tickets == 0 {
		return 0
	}
	return float64(d.Bugs) / float64(d.Tickets) * 100
}

// WeeklyDefects returns one DefectWeek per ISO week with any commit or ticket
// activity. Revert detection is a "Revert " message prefix; bug detection is
// a case-insensitive issue type of "bug".
func WeeklyDefects(ctx context.Context, s *store.Store) ([]DefectWeek, error) {
	return WeeklyDefectsScoped(ctx, s, Scope{})
}

// WeeklyDefectsScoped is WeeklyDefects restricted to scope (repos for the
// revert rate, tracker project for the bug rate).
func WeeklyDefectsScoped(ctx context.Context, s *store.Store, scope Scope) ([]DefectWeek, error) {
	cFilt, cArgs := scope.commitFilter("repo_name")
	tFilt, tArgs := scope.ticketFilter("project_key")
	type agg struct {
		commits, reverts, tickets, bugs int
	}
	weeks := map[time.Time]*agg{}
	get := func(wk time.Time) *agg {
		a, ok := weeks[wk]
		if !ok {
			a = &agg{}
			weeks[wk] = a
		}
		return a
	}

	// Commits — dedupe shared SHAs (forks/mirrors) like WeeklyStats does.
	crows, err := s.DB().QueryContext(ctx, `SELECT committed_at, message FROM commits WHERE 1=1`+cFilt+` GROUP BY sha`, cArgs...)
	if err != nil {
		return nil, fmt.Errorf("query defect commits: %w", err)
	}
	for crows.Next() {
		var ts int64
		var msg string
		if err := crows.Scan(&ts, &msg); err != nil {
			_ = crows.Close()
			return nil, fmt.Errorf("scan defect commit: %w", err)
		}
		a := get(IsoWeekStart(time.Unix(ts, 0)))
		a.commits++
		if isRevert(msg) {
			a.reverts++
		}
	}
	if err := crows.Err(); err != nil {
		_ = crows.Close()
		return nil, fmt.Errorf("iterate defect commits: %w", err)
	}
	_ = crows.Close()

	// Tickets — bucket by created week; bugs by issue type.
	trows, err := s.DB().QueryContext(ctx, `SELECT created_at, type FROM tickets WHERE 1=1`+tFilt, tArgs...)
	if err != nil {
		return nil, fmt.Errorf("query defect tickets: %w", err)
	}
	for trows.Next() {
		var ts int64
		var typ string
		if err := trows.Scan(&ts, &typ); err != nil {
			_ = trows.Close()
			return nil, fmt.Errorf("scan defect ticket: %w", err)
		}
		a := get(IsoWeekStart(time.Unix(ts, 0)))
		a.tickets++
		if strings.EqualFold(strings.TrimSpace(typ), "bug") {
			a.bugs++
		}
	}
	if err := trows.Err(); err != nil {
		_ = trows.Close()
		return nil, fmt.Errorf("iterate defect tickets: %w", err)
	}
	_ = trows.Close()

	if len(weeks) == 0 {
		return nil, nil
	}
	out := make([]DefectWeek, 0, len(weeks))
	for wk, a := range weeks {
		out = append(out, DefectWeek{
			WeekStart: wk, Commits: a.commits, Reverts: a.reverts,
			Tickets: a.tickets, Bugs: a.bugs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WeekStart.Before(out[j].WeekStart) })
	return out, nil
}

func isRevert(msg string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(msg)), "revert ")
}

// DefectRates aggregates a slice of DefectWeek into overall revert and bug
// rates (percentages) plus whether any ticket data was present.
func DefectRates(weeks []DefectWeek) (revertRate, bugRate float64, hasBugs bool) {
	var commits, reverts, tickets, bugs int
	for _, w := range weeks {
		commits += w.Commits
		reverts += w.Reverts
		tickets += w.Tickets
		bugs += w.Bugs
	}
	if commits > 0 {
		revertRate = float64(reverts) / float64(commits) * 100
	}
	if tickets > 0 {
		bugRate = float64(bugs) / float64(tickets) * 100
	}
	return revertRate, bugRate, tickets > 0
}

// SplitDefectsByCutover splits the weekly defect series at the cutover week
// (before is strictly earlier). Mirrors SplitByCutover for WeekStats.
func SplitDefectsByCutover(weeks []DefectWeek, cutover Cutover) (before, after []DefectWeek) {
	if !cutover.Detected {
		return weeks, nil
	}
	for _, w := range weeks {
		if w.WeekStart.Before(cutover.Date) {
			before = append(before, w)
		} else {
			after = append(after, w)
		}
	}
	return before, after
}
