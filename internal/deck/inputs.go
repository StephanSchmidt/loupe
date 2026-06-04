package deck

import (
	"context"
	"fmt"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/store"
)

// Inputs bundles every store-derived series RenderDeck consumes beyond the
// weekly stats and cutover (which callers compute earlier, before deciding a
// deck is renderable at all).
type Inputs struct {
	Cycles       []analyze.WeekCycle
	RepoAdoption []analyze.RepoAdoptionWeek
	WIP          []analyze.WIPWeek
	Defects      []analyze.DefectWeek
	BugFix       []analyze.BugFixWeek
	PRCycle      []analyze.PRWeek
	Retention    []analyze.RetentionPoint
	TeamLanding  []analyze.Landing
	RepoLanding  []analyze.Landing
	Focus        []FocusData
	Tools        analyze.ToolBreakdownStats
}

// GatherInputs runs every per-slide analysis against the store. Shared by
// `loupe render` and the ingest pipeline so the two render paths cannot
// drift apart.
func GatherInputs(ctx context.Context, s *store.Store, cfg *config.Config) (Inputs, error) {
	cc := analyze.CycleConfig{
		DevStartedStatuses: cfg.CycleTime.DevStartedStatuses,
		DoneStatuses:       cfg.CycleTime.DoneStatuses,
		AbandonedStatuses:  cfg.CycleTime.AbandonedStatuses,
		BugTypes:           cfg.CycleTime.BugTypes,
	}
	var in Inputs
	var err error
	if in.Cycles, err = analyze.WeeklyCycles(ctx, s, cc); err != nil {
		return Inputs{}, fmt.Errorf("weekly cycles: %w", err)
	}
	if in.Tools, err = analyze.ToolBreakdown(ctx, s); err != nil {
		return Inputs{}, fmt.Errorf("tool breakdown: %w", err)
	}
	if in.RepoAdoption, err = analyze.WeeklyRepoAdoption(ctx, s); err != nil {
		return Inputs{}, fmt.Errorf("repo adoption: %w", err)
	}
	if in.WIP, err = analyze.WeeklyWIP(ctx, s, cc); err != nil {
		return Inputs{}, fmt.Errorf("work in progress: %w", err)
	}
	if in.Defects, err = analyze.WeeklyDefects(ctx, s, cfg.CycleTime.BugTypes); err != nil {
		return Inputs{}, fmt.Errorf("defects: %w", err)
	}
	if in.BugFix, err = analyze.WeeklyBugFix(ctx, s, cc); err != nil {
		return Inputs{}, fmt.Errorf("bug-fix speed: %w", err)
	}
	if in.PRCycle, err = analyze.WeeklyPRCycle(ctx, s); err != nil {
		return Inputs{}, fmt.Errorf("PR cycle: %w", err)
	}
	if in.Retention, err = analyze.ComputeRetention(ctx, s); err != nil {
		return Inputs{}, fmt.Errorf("retention: %w", err)
	}
	if in.RepoLanding, err = analyze.ComputeRepoLanding(ctx, s, LandingTopRepos, cfg.Windows.DisplayMonths); err != nil {
		return Inputs{}, fmt.Errorf("repo landing: %w", err)
	}
	if in.TeamLanding, err = analyze.ComputeTeamLanding(ctx, s, TeamSpecs(cfg), cfg.Windows.DisplayMonths); err != nil {
		return Inputs{}, fmt.Errorf("team landing: %w", err)
	}
	if in.Focus, err = ComputeFocus(ctx, s, cfg); err != nil {
		return Inputs{}, fmt.Errorf("focus: %w", err)
	}
	return in, nil
}
