// Package cmdrender exposes `loupe render`: regenerate the deck from the
// existing local sqlite state, no API calls. Fast inner loop for tweaking
// the deck template, chart options, or cutover threshold without
// re-ingesting.
package cmdrender

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/deck"
	"github.com/StephanSchmidt/loupe/internal/store"
)

const (
	defaultConfigPath = "loupe.yaml"
	stateDBPath       = ".loupe/state.db"
)

func BuildRenderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Re-render the deck from existing sqlite state — no API calls",
		Long: `Loads .loupe/state.db, recomputes weekly stats + cutover, and
produces a fresh deck under <output.path>/<timestamp>/. Useful when iterating
on the deck template or chart options — no provider credentials needed, no
provider API calls made.`,
		SilenceUsage: true,
		RunE:         runRender,
	}
	cmd.Flags().String("config", defaultConfigPath, "path to loupe.yaml")
	cmd.Flags().String("cutover-date", "", "override AI-adoption cutover (YYYY-MM-DD)")
	return cmd
}

func runRender(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	cutoverFlag, _ := cmd.Flags().GetString("cutover-date")

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	override, err := resolveCutoverOverride(cutoverFlag, cfg.AIAdoption.CutoverDate)
	if err != nil {
		return err
	}

	if _, err := os.Stat(stateDBPath); err != nil {
		return fmt.Errorf("no state at %s — run `loupe baseline` first", stateDBPath)
	}
	s, err := store.Open(stateDBPath)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	return renderFromStore(cmd.Context(), cmd.OutOrStdout(), cfg, s, override)
}

func renderFromStore(ctx context.Context, out io.Writer, cfg *config.Config, s *store.Store, override time.Time) error {
	// Re-run detectors so changes to the detection config (new label
	// patterns, seat-inference toggle, …) take effect on the next render
	// without forcing a fresh ingest. All detectors are idempotent.
	if _, err := analyze.RunAllDetectors(ctx, s, detectionConfigFor(cfg)); err != nil {
		return fmt.Errorf("re-detect AI signals: %w", err)
	}

	weeks, err := analyze.WeeklyStats(ctx, s)
	if err != nil {
		return err
	}
	cutover, err := analyze.DetectCutover(ctx, s, *cfg.AIAdoption.MinWeeklyCommitsForCutover, override)
	if err != nil {
		return err
	}
	if len(weeks) == 0 {
		return fmt.Errorf("state has no weekly data — was `loupe baseline` ever completed?")
	}

	runID := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	deckDir := filepath.Join(cfg.Output.Path, runID)
	if err := os.MkdirAll(filepath.Dir(deckDir), 0o750); err != nil {
		return fmt.Errorf("create reports dir: %w", err)
	}
	cycles, err := analyze.WeeklyCycles(ctx, s, analyze.CycleConfig{
		DevStartedStatuses: cfg.CycleTime.DevStartedStatuses,
	})
	if err != nil {
		return fmt.Errorf("weekly cycles: %w", err)
	}
	if err := deck.RenderDeck(deckDir, cfg, weeks, cutover, cycles, time.Now().UTC()); err != nil {
		return fmt.Errorf("render deck: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Deck ready: %s/index.html\n", deckDir)
	_, _ = fmt.Fprintln(out, "Run `loupe present` to view.")
	return nil
}

// detectionConfigFor mirrors the helper in cmdbaseline. Kept here as a
// small duplicate rather than introducing a shared cmd-internal package
// for two callers — extract when a third use site shows up.
func detectionConfigFor(cfg *config.Config) analyze.DetectionConfig {
	d := cfg.AIAdoption.Detection
	squash := true
	if d.SquashMergeRecovery != nil {
		squash = *d.SquashMergeRecovery
	}
	return analyze.DetectionConfig{
		PRLabels:            d.PRLabels,
		BranchPrefixes:      d.BranchPrefixes,
		SquashMergeRecovery: squash,
		SeatInference:       d.SeatInference,
	}
}

func resolveCutoverOverride(cliFlag, configValue string) (time.Time, error) {
	value := cliFlag
	if value == "" {
		value = configValue
	}
	if value == "" {
		return time.Time{}, nil
	}
	t, err := config.ParseCutoverDate(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse cutover date %q: %w", value, err)
	}
	return t, nil
}
