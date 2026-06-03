package analyze

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
)

// BugFixWeek is the per-ISO-week defect-class lead time: median engineering
// time (dev start → last linked commit) for bug-class tickets vs everything
// else, bucketed by the week the work finished. It answers "does AI fix bugs
// faster?" by splitting the existing lead-time signal by ticket type.
type BugFixWeek struct {
	WeekStart       time.Time
	BugCount        int
	BugMedianLead   time.Duration // median DevToRelease of bug-class tickets
	OtherCount      int
	OtherMedianLead time.Duration
}

// isBug reports whether a ticket type matches the configured bug-type set
// (case-insensitive, trimmed). Shared by the bug-rate and bug-fix-speed
// charts so "bug" is defined in exactly one place.
func isBug(typ string, set map[string]struct{}) bool {
	if len(set) == 0 {
		return false
	}
	_, ok := set[strings.ToLower(strings.TrimSpace(typ))]
	return ok
}

// WeeklyBugFix returns one BugFixWeek per ISO week, splitting cycle lead time
// by whether the ticket type is in cfg.BugTypes.
func WeeklyBugFix(ctx context.Context, s *store.Store, cfg CycleConfig) ([]BugFixWeek, error) {
	return WeeklyBugFixScoped(ctx, s, cfg, Scope{})
}

// WeeklyBugFixScoped is WeeklyBugFix restricted to scope.TrackerProject.
func WeeklyBugFixScoped(ctx context.Context, s *store.Store, cfg CycleConfig, scope Scope) ([]BugFixWeek, error) {
	bugSet := normaliseStatuses(cfg.BugTypes)
	if len(bugSet) == 0 {
		return nil, nil
	}
	cycles, err := ComputeCyclesScoped(ctx, s, cfg, scope)
	if err != nil {
		return nil, err
	}
	if len(cycles) == 0 {
		return nil, nil
	}
	type bucket struct {
		bug   []float64
		other []float64
	}
	groups := map[time.Time]*bucket{}
	for _, c := range cycles {
		k := IsoWeekStart(c.LastDevAt)
		b, ok := groups[k]
		if !ok {
			b = &bucket{}
			groups[k] = b
		}
		h := c.DevToRelease.Hours()
		if isBug(c.Type, bugSet) {
			b.bug = append(b.bug, h)
		} else {
			b.other = append(b.other, h)
		}
	}

	out := make([]BugFixWeek, 0, len(groups))
	for week, b := range groups {
		out = append(out, BugFixWeek{
			WeekStart:       week,
			BugCount:        len(b.bug),
			BugMedianLead:   hoursToDuration(percentile(b.bug, 50)),
			OtherCount:      len(b.other),
			OtherMedianLead: hoursToDuration(percentile(b.other, 50)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WeekStart.Before(out[j].WeekStart) })
	return out, nil
}

// SplitBugFixByCutover splits the weekly bug-fix series at the cutover week
// (before is strictly earlier). Mirrors SplitDefectsByCutover.
func SplitBugFixByCutover(weeks []BugFixWeek, cutover Cutover) (before, after []BugFixWeek) {
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

// BugFixLeadTimes aggregates a slice of BugFixWeek into the overall median
// bug and other lead times plus the total bug-ticket count. The count gates
// the slide (auto-omit when too few bug-class tickets are present), and the
// medians drive the before/after headline. The overall median is computed
// from the per-week medians weighted by count — a close-enough summary that
// avoids reloading every ticket.
func BugFixLeadTimes(weeks []BugFixWeek) (bugMedian, otherMedian time.Duration, bugN int) {
	var bugHours, otherHours []float64
	var otherN int
	for _, w := range weeks {
		for i := 0; i < w.BugCount; i++ {
			bugHours = append(bugHours, w.BugMedianLead.Hours())
		}
		for i := 0; i < w.OtherCount; i++ {
			otherHours = append(otherHours, w.OtherMedianLead.Hours())
		}
		bugN += w.BugCount
		otherN += w.OtherCount
	}
	return hoursToDuration(percentile(bugHours, 50)),
		hoursToDuration(percentile(otherHours, 50)), bugN
}
