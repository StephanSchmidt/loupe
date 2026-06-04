package analyze

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
)

// PRWeek is the per-ISO-week pull-request velocity: how many PRs merged that
// week and the median time from open to merge. Bucketed by merge week.
type PRWeek struct {
	WeekStart     time.Time
	Merged        int
	MedianToMerge time.Duration
}

// WeeklyPRCycle returns one PRWeek per ISO week with merged PRs.
func WeeklyPRCycle(ctx context.Context, s *store.Store) ([]PRWeek, error) {
	return WeeklyPRCycleScoped(ctx, s, Scope{})
}

// WeeklyPRCycleScoped is WeeklyPRCycle restricted to scope.Repos (by
// prs.repo_name). Only MERGED PRs with a merge timestamp and a non-bot author
// count; merged_at is populated by ingest (NULL on stores indexed before the
// merge-time fix, in which case this returns nil and the slide self-hides).
func WeeklyPRCycleScoped(ctx context.Context, s *store.Store, scope Scope) ([]PRWeek, error) {
	filt, args := scope.commitFilter("repo_name")
	rows, err := s.DB().QueryContext(ctx, `
        SELECT created_at, merged_at FROM prs
        WHERE state = 'MERGED' AND merged_at IS NOT NULL AND author_is_bot = 0
              AND merged_at >= created_at`+filt, args...)
	if err != nil {
		return nil, fmt.Errorf("query PR cycle: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type bucket struct{ hours []float64 }
	weeks := map[time.Time]*bucket{}
	for rows.Next() {
		var created, merged int64
		if err := rows.Scan(&created, &merged); err != nil {
			return nil, fmt.Errorf("scan PR: %w", err)
		}
		wk := IsoWeekStart(time.Unix(merged, 0).UTC())
		b := weeks[wk]
		if b == nil {
			b = &bucket{}
			weeks[wk] = b
		}
		b.hours = append(b.hours, float64(merged-created)/3600)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(weeks) == 0 {
		return nil, nil
	}
	out := make([]PRWeek, 0, len(weeks))
	for wk, b := range weeks {
		out = append(out, PRWeek{
			WeekStart:     wk,
			Merged:        len(b.hours),
			MedianToMerge: hoursToDuration(percentile(b.hours, 50)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WeekStart.Before(out[j].WeekStart) })
	return out, nil
}

// SplitPRByCutover splits the weekly PR series at the cutover week (before is
// strictly earlier). Mirrors SplitDefectsByCutover.
func SplitPRByCutover(weeks []PRWeek, cutover Cutover) (before, after []PRWeek) {
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

// PRCycleSummary aggregates a weekly PR series: the median days-to-merge
// (median of the per-week medians, weighted by merge count), the mean merged
// PRs per week, and the total merged count (gates the slide).
func PRCycleSummary(weeks []PRWeek) (medianDays, mergedPerWeek float64, total int) {
	var hours []float64
	for _, w := range weeks {
		for i := 0; i < w.Merged; i++ {
			hours = append(hours, w.MedianToMerge.Hours())
		}
		total += w.Merged
	}
	if len(weeks) > 0 {
		mergedPerWeek = float64(total) / float64(len(weeks))
	}
	return percentile(hours, 50) / 24, mergedPerWeek, total
}

// PRToMergeSeries is the per-week median days-to-merge, for the significance
// bootstrap.
func PRToMergeSeries(weeks []PRWeek) []float64 {
	out := make([]float64, 0, len(weeks))
	for _, w := range weeks {
		if w.Merged > 0 {
			out = append(out, w.MedianToMerge.Hours()/24)
		}
	}
	return out
}
