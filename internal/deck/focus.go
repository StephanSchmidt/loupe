package deck

import (
	"context"
	"fmt"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/store"
)

// ComputeFocus runs the scoped analysis for each configured `focus` lens and
// returns the per-lens series the deck renders as extra slides. Returns nil
// when no focus is configured.
func ComputeFocus(ctx context.Context, s *store.Store, cfg *config.Config) ([]FocusData, error) {
	if len(cfg.Focus) == 0 {
		return nil, nil
	}
	cc := analyze.CycleConfig{
		DevStartedStatuses: cfg.CycleTime.DevStartedStatuses,
		DoneStatuses:       cfg.CycleTime.DoneStatuses,
		AbandonedStatuses:  cfg.CycleTime.AbandonedStatuses,
	}
	out := make([]FocusData, 0, len(cfg.Focus))
	for _, f := range cfg.Focus {
		scope := analyze.Scope{Repos: f.RepoFullNames(cfg.Org), TrackerProject: f.TrackerProject}

		weeks, err := analyze.WeeklyStatsScoped(ctx, s, scope)
		if err != nil {
			return nil, fmt.Errorf("focus %q weekly stats: %w", f.Name, err)
		}
		cycles, err := analyze.WeeklyCyclesScoped(ctx, s, cc, scope)
		if err != nil {
			return nil, fmt.Errorf("focus %q cycles: %w", f.Name, err)
		}
		repoAdoption, err := analyze.WeeklyRepoAdoptionScoped(ctx, s, scope)
		if err != nil {
			return nil, fmt.Errorf("focus %q repo adoption: %w", f.Name, err)
		}
		wip, err := analyze.WeeklyWIPScoped(ctx, s, cc, scope)
		if err != nil {
			return nil, fmt.Errorf("focus %q wip: %w", f.Name, err)
		}
		defects, err := analyze.WeeklyDefectsScoped(ctx, s, scope)
		if err != nil {
			return nil, fmt.Errorf("focus %q defects: %w", f.Name, err)
		}
		out = append(out, FocusData{
			Name: f.DisplayTitle(), Weeks: weeks, Cycles: cycles,
			RepoAdoption: repoAdoption, WIP: wip, Defects: defects,
		})
	}
	return out, nil
}
