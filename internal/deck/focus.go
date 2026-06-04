package deck

import (
	"context"
	"fmt"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/store"
)

// LandingTopRepos caps how many repos the "where AI lands by repo" chart shows
// (109 repos is too many to plot).
const LandingTopRepos = 12

// TeamSpecs converts the config's teams into the analyze package's TeamSpec
// (analyze stays free of a config import).
func TeamSpecs(cfg *config.Config) []analyze.TeamSpec {
	out := make([]analyze.TeamSpec, 0, len(cfg.Teams))
	for _, t := range cfg.Teams {
		out = append(out, analyze.TeamSpec{Name: t.Name, Members: t.Members})
	}
	return out
}

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
		BugTypes:           cfg.CycleTime.BugTypes,
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
		defects, err := analyze.WeeklyDefectsScoped(ctx, s, scope, cfg.CycleTime.BugTypes)
		if err != nil {
			return nil, fmt.Errorf("focus %q defects: %w", f.Name, err)
		}
		bugfix, err := analyze.WeeklyBugFixScoped(ctx, s, cc, scope)
		if err != nil {
			return nil, fmt.Errorf("focus %q bug-fix: %w", f.Name, err)
		}
		prcycle, err := analyze.WeeklyPRCycleScoped(ctx, s, scope)
		if err != nil {
			return nil, fmt.Errorf("focus %q PR cycle: %w", f.Name, err)
		}
		out = append(out, FocusData{
			Name: f.DisplayTitle(), Weeks: weeks, Cycles: cycles,
			RepoAdoption: repoAdoption, WIP: wip, Defects: defects, BugFix: bugfix, PRCycle: prcycle,
		})
	}
	return out, nil
}
