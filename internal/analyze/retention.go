package analyze

import (
	"context"
	"fmt"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
)

// RetentionPoint is one x-position of the AI-adoption survival curve: of the
// developers who had enough elapsed time to reach week k after their first AI
// commit, the share whose AI use lasted at least that long (their most recent
// AI commit is ≥ k weeks after their first).
type RetentionPoint struct {
	WeeksSinceAdoption int
	Cohort             int     // adopters with the opportunity to reach week k (denominator)
	StillUsing         int     // …whose AI-usage span reached week k (numerator)
	Rate               float64 // StillUsing / Cohort * 100
}

// retentionMinCohort is the floor of eligible adopters below which the curve is
// cut — past it the rate is too noisy to read.
const retentionMinCohort = 5

// retentionMaxWeeks caps the curve length regardless of cohort size.
const retentionMaxWeeks = 52

// ComputeRetention builds the per-developer AI-adoption survival curve. For
// each non-bot developer it takes their first and most-recent AI-evidenced
// commit. Survival at week k is, among adopters who started ≥ k weeks before
// the latest data week (so they had the chance to reach week k — censoring
// correction), the fraction whose AI use still spanned to week k. This is a
// rolling-retention survival curve: monotonically non-increasing, starting at
// 100%, and reads directly as "X% of adopters were still using AI k weeks
// after they started". A fast drop = honeymoon; a flat line = durable.
// Org-wide only — per-project cohorts are too thin.
func ComputeRetention(ctx context.Context, s *store.Store) ([]RetentionPoint, error) {
	rows, err := loadAuthorWeekAI(ctx, s)
	if err != nil {
		return nil, err
	}

	// Per adopter: first and last AI-active week. Also track the latest data
	// week across everyone, for the censoring cutoff.
	type span struct{ first, last time.Time }
	devs := map[string]*span{}
	var latest time.Time
	for _, r := range rows {
		if r.week.After(latest) {
			latest = r.week
		}
		if !r.hasAI {
			continue
		}
		d := devs[r.email]
		if d == nil {
			devs[r.email] = &span{first: r.week, last: r.week}
			continue
		}
		if r.week.Before(d.first) {
			d.first = r.week
		}
		if r.week.After(d.last) {
			d.last = r.week
		}
	}

	// Stop the curve once the eligible cohort thins past half of all adopters:
	// beyond that the few long-tenured devs left are a self-selected (stickier)
	// sample, which makes the tail bend back up and misread. Keeping eligibility
	// high keeps the curve representative and cleanly declining.
	total := len(devs)
	minEligible := total / 2
	if minEligible < retentionMinCohort {
		minEligible = retentionMinCohort
	}

	out := make([]RetentionPoint, 0, retentionMaxWeeks)
	for k := 0; k <= retentionMaxWeeks; k++ {
		eligible, survived := 0, 0
		for _, d := range devs {
			target := d.first.AddDate(0, 0, 7*k)
			if target.After(latest) {
				continue // censored — not enough time elapsed to reach week k
			}
			eligible++
			if !d.last.Before(target) {
				survived++ // last AI week is at or beyond week k
			}
		}
		if k > 0 && eligible < minEligible {
			break // eligibility only shrinks with k; stop before the tail skews
		}
		out = append(out, RetentionPoint{
			WeeksSinceAdoption: k,
			Cohort:             eligible,
			StillUsing:         survived,
			Rate:               float64(survived) / float64(eligible) * 100,
		})
	}
	return out, nil
}

type authorWeekAI struct {
	email string
	week  time.Time
	hasAI bool
}

// loadAuthorWeekAI returns one row per (author, ISO week) that the author
// committed, with hasAI true when any of that week's commits carried an AI
// signal. Bots are excluded so the curve tracks human developers.
func loadAuthorWeekAI(ctx context.Context, s *store.Store) ([]authorWeekAI, error) {
	rows, err := s.DB().QueryContext(ctx, `
        SELECT c.author_email, c.author_name, c.committed_at,
               MAX(CASE WHEN sig.commit_sha IS NOT NULL THEN 1 ELSE 0 END) AS has_ai
        FROM commits c
        LEFT JOIN ai_signals sig ON sig.commit_sha = c.sha
        GROUP BY c.sha, c.author_email, c.author_name, c.committed_at
    `)
	if err != nil {
		return nil, fmt.Errorf("query author-week AI: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Reduce to (email, week) → hasAI in memory, skipping bots.
	type key struct {
		email string
		week  time.Time
	}
	agg := map[key]bool{}
	for rows.Next() {
		var email, name string
		var ts int64
		var hasAI int
		if err := rows.Scan(&email, &name, &ts, &hasAI); err != nil {
			return nil, fmt.Errorf("scan author-week: %w", err)
		}
		if IsBot(email, name) {
			continue
		}
		k := key{email, IsoWeekStart(time.Unix(ts, 0).UTC())}
		if hasAI == 1 {
			agg[k] = true
		} else if _, ok := agg[k]; !ok {
			agg[k] = false
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]authorWeekAI, 0, len(agg))
	for k, ai := range agg {
		out = append(out, authorWeekAI{email: k.email, week: k.week, hasAI: ai})
	}
	return out, nil
}
