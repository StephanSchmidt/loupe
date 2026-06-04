package analyze

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
)

// RepoAdoptionWeek is the per-ISO-week breakdown of repositories by AI
// adoption and recent activity, as of the end of that week.
//
// AIEnabled is cumulative ("first adoption is a floor"): once a repo has had
// any AI commit it counts as AI-enabled for every later week, regardless of
// activity. The remaining repos that exist by that week are split by whether
// their most recent commit is within the activity window.
type RepoAdoptionWeek struct {
	WeekStart time.Time
	AIEnabled int // repos with ≥1 AI commit by this week (cumulative)
	Active    int // non-AI repos whose last commit is within 90 days
	Inactive  int // existing non-AI repos with no commit in 90 days
}

// repoActivityWindow is how recently a repo must have committed to count as
// "active" rather than "inactive".
const repoActivityWindow = 90 * 24 * time.Hour

// WeeklyRepoAdoption returns one RepoAdoptionWeek per ISO week spanning the
// commit history. A repo is only counted from the week of its first commit
// onward (so the stacked total grows as repos are created).
func WeeklyRepoAdoption(ctx context.Context, s *store.Store) ([]RepoAdoptionWeek, error) {
	return WeeklyRepoAdoptionScoped(ctx, s, Scope{})
}

// repoAdoptionState is one repo's commit timeline for the adoption snapshot.
type repoAdoptionState struct {
	times   []int64 // commit unix times, ascending (query is ordered)
	firstAI int64   // earliest AI-commit time; 0 means never
}

// WeeklyRepoAdoptionScoped is WeeklyRepoAdoption restricted to scope.Repos.
func WeeklyRepoAdoptionScoped(ctx context.Context, s *store.Store, scope Scope) ([]RepoAdoptionWeek, error) {
	repos, minT, maxT, err := loadRepoAdoption(ctx, s, scope)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, nil
	}
	return repoAdoptionSnapshots(repos, minT, maxT), nil
}

// loadRepoAdoption loads each (scoped) repo's commit timeline plus the overall
// min/max commit time.
func loadRepoAdoption(ctx context.Context, s *store.Store, scope Scope) (map[string]*repoAdoptionState, int64, int64, error) {
	filt, args := scope.commitFilter("c.repo_name")
	rows, err := s.DB().QueryContext(ctx, `
        SELECT c.repo_name, c.committed_at,
               MAX(CASE WHEN sig.commit_sha IS NOT NULL THEN 1 ELSE 0 END) AS has_ai
        FROM commits c
        LEFT JOIN ai_signals sig ON sig.commit_sha = c.sha
        WHERE 1=1`+filt+`
        GROUP BY c.sha, c.repo_name, c.committed_at
        ORDER BY c.committed_at
    `, args...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("query repo adoption: %w", err)
	}
	defer func() { _ = rows.Close() }()

	repos := map[string]*repoAdoptionState{}
	var minT, maxT int64
	first := true
	for rows.Next() {
		var name string
		var ts int64
		var hasAI int
		if err := rows.Scan(&name, &ts, &hasAI); err != nil {
			return nil, 0, 0, fmt.Errorf("scan repo adoption row: %w", err)
		}
		r := repos[name]
		if r == nil {
			r = &repoAdoptionState{}
			repos[name] = r
		}
		r.times = append(r.times, ts)
		if hasAI == 1 && (r.firstAI == 0 || ts < r.firstAI) {
			r.firstAI = ts
		}
		if first || ts < minT {
			minT = ts
		}
		if first || ts > maxT {
			maxT = ts
		}
		first = false
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("iterate repo adoption: %w", err)
	}
	return repos, minT, maxT, nil
}

// repoAdoptionSnapshots takes the end-of-week adoption/activity snapshot for
// every ISO week between the two timestamps.
func repoAdoptionSnapshots(repos map[string]*repoAdoptionState, minT, maxT int64) []RepoAdoptionWeek {
	windowSecs := int64(repoActivityWindow / time.Second)
	startWk := IsoWeekStart(time.Unix(minT, 0))
	endWk := IsoWeekStart(time.Unix(maxT, 0))

	var out []RepoAdoptionWeek
	for wk := startWk; !wk.After(endWk); wk = wk.AddDate(0, 0, 7) {
		asOf := wk.AddDate(0, 0, 7).Unix() // end of this ISO week
		var aiEnabled, active, inactive int
		for _, r := range repos {
			latest, exists := latestAtOrBefore(r.times, asOf)
			if !exists {
				continue // repo's first commit is in a later week
			}
			switch {
			case r.firstAI != 0 && r.firstAI <= asOf:
				aiEnabled++
			case asOf-latest <= windowSecs:
				active++
			default:
				inactive++
			}
		}
		out = append(out, RepoAdoptionWeek{
			WeekStart: wk, AIEnabled: aiEnabled, Active: active, Inactive: inactive,
		})
	}
	return out
}

// latestAtOrBefore returns the largest value in the ascending slice that is
// <= target, and whether such a value exists.
func latestAtOrBefore(sorted []int64, target int64) (int64, bool) {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] > target })
	if i == 0 {
		return 0, false
	}
	return sorted[i-1], true
}
