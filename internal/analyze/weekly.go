package analyze

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
)

// WeekStats holds aggregate commit + adoption counts for a single ISO week.
//
// AICommits is the rollup count of commits with any AI signal (high or
// medium). AICommitsHigh / AICommitsMedium split that rollup so the deck
// can show evidence-based vs inferred adoption side by side.
//
// AdoptionRatio is left for callers to compute on demand — keeping it
// derived avoids the float64 drifting out of sync with the integer counts.
type WeekStats struct {
	WeekStart       time.Time
	TotalCommits    int
	AICommits       int
	AICommitsHigh   int
	AICommitsMedium int
	DistinctAuthors int
	AIAuthors       int
	AIAuthorsHigh   int
}

// CommitRatio is AICommits / TotalCommits. Returns 0 if TotalCommits is 0.
func (w WeekStats) CommitRatio() float64 {
	if w.TotalCommits == 0 {
		return 0
	}
	return float64(w.AICommits) / float64(w.TotalCommits)
}

// AdoptionRatio is AIAuthors / DistinctAuthors. Returns 0 if DistinctAuthors is 0.
func (w WeekStats) AdoptionRatio() float64 {
	if w.DistinctAuthors == 0 {
		return 0
	}
	return float64(w.AIAuthors) / float64(w.DistinctAuthors)
}

// IsoWeekStart returns the start (Monday 00:00 UTC) of the ISO week that
// contains t.
func IsoWeekStart(t time.Time) time.Time {
	t = t.UTC()
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7 // Sunday → end of ISO week
	}
	return time.Date(t.Year(), t.Month(), t.Day()-(wd-1), 0, 0, 0, 0, time.UTC)
}

// WeeklyStats returns one WeekStats row per ISO week with at least one
// commit, ordered chronologically.
//
// Bot filtering: non-AI bots (Dependabot, Renovate, …) are excluded from
// every count. AI bots (Copilot Coding Agent, Devin, …) stay in — their
// commits represent real AI throughput.
func WeeklyStats(ctx context.Context, s *store.Store) ([]WeekStats, error) {
	return WeeklyStatsScoped(ctx, s, Scope{})
}

// WeeklyStatsScoped is WeeklyStats restricted to scope.Repos (commit charts).
func WeeklyStatsScoped(ctx context.Context, s *store.Store, scope Scope) ([]WeekStats, error) {
	filt, args := scope.commitFilter("c.repo_name")
	rows, err := s.DB().QueryContext(ctx, `
        SELECT c.committed_at, c.author_email, c.author_name,
               MAX(CASE WHEN sig.confidence = 'high' THEN 1 ELSE 0 END) AS has_high,
               MAX(CASE WHEN sig.commit_sha IS NOT NULL THEN 1 ELSE 0 END) AS has_any
        FROM commits c
        LEFT JOIN ai_signals sig ON sig.commit_sha = c.sha
        WHERE 1=1`+filt+`
        GROUP BY c.sha, c.committed_at, c.author_email, c.author_name
        ORDER BY c.committed_at
    `, args...)
	if err != nil {
		return nil, fmt.Errorf("query weekly: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type weekAgg struct {
		WeekStats
		authors       map[string]struct{}
		aiAuthors     map[string]struct{}
		aiAuthorsHigh map[string]struct{}
	}
	buckets := make(map[time.Time]*weekAgg)

	for rows.Next() {
		var ts int64
		var email, name string
		var hasHigh, hasAny int
		if err := rows.Scan(&ts, &email, &name, &hasHigh, &hasAny); err != nil {
			return nil, fmt.Errorf("scan weekly row: %w", err)
		}
		if IsBot(email, name) {
			if _, isAIBot := IsAIBot(email, name); !isAIBot {
				continue
			}
		}
		wk := IsoWeekStart(time.Unix(ts, 0))
		b, ok := buckets[wk]
		if !ok {
			b = &weekAgg{
				WeekStats:     WeekStats{WeekStart: wk},
				authors:       make(map[string]struct{}),
				aiAuthors:     make(map[string]struct{}),
				aiAuthorsHigh: make(map[string]struct{}),
			}
			buckets[wk] = b
		}
		b.TotalCommits++
		b.authors[email] = struct{}{}
		if hasAny == 1 {
			b.AICommits++
			b.aiAuthors[email] = struct{}{}
			if hasHigh == 1 {
				b.AICommitsHigh++
				b.aiAuthorsHigh[email] = struct{}{}
			} else {
				b.AICommitsMedium++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate weekly: %w", err)
	}

	out := make([]WeekStats, 0, len(buckets))
	for _, b := range buckets {
		b.DistinctAuthors = len(b.authors)
		b.AIAuthors = len(b.aiAuthors)
		b.AIAuthorsHigh = len(b.aiAuthorsHigh)
		out = append(out, b.WeekStats)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WeekStart.Before(out[j].WeekStart) })
	return out, nil
}
