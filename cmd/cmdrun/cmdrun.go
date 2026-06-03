// Package cmdrun exposes `loupe run`: the weekly incremental update against an
// existing baseline. It fetches only git commits, PRs, and tickets newer than
// the last successful run (per-source watermarks), re-detects AI signals, and
// renders a fresh deck — then prints a single CI-greppable summary line.
package cmdrun

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/pipeline"
	"github.com/StephanSchmidt/loupe/internal/store"
)

const defaultConfigPath = "loupe.yaml"

// Hidden flag names — shared with `baseline`, used by smoke tests / CI to drive
// the command non-interactively. Provider-neutral by design.
const (
	flagGitHostToken   = "git-host-token"
	flagTrackerToken   = "tracker-token"
	flagGitHostBaseURL = "git-host-base-url"
	flagTrackerBaseURL = "tracker-base-url"
)

func BuildRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Weekly incremental run — fetch new data and render an updated deck",
		Long: `Incremental update against the existing sqlite state. Fetches only
git commits, PRs, and tickets newer than the last successful run (per-source
watermarks), re-detects AI signals, and renders a fresh reveal.js deck whose
week-over-week charts show what changed.

Requires an existing baseline — run ` + "`loupe baseline`" + ` first.

CI-friendly: exit code 0 on success, non-zero on failure, plus a one-line
summary on stdout.`,
		SilenceUsage: true,
		RunE:         runRun,
	}

	cmd.Flags().String("config", defaultConfigPath, "path to loupe.yaml")
	cmd.Flags().String("since", "", "override window start (RFC3339 or YYYY-MM-DD); re-pulls data from this point regardless of the stored watermark")
	cmd.Flags().Bool("dry-run", false, "validate config and confirm a baseline exists without writing state")
	cmd.Flags().Bool("plain", false, "disable the animated progress display; print plain lines (auto-disabled when stdout isn't a terminal)")

	// Hidden test-only flags, mirroring `baseline`.
	cmd.Flags().String(flagGitHostToken, "", "")
	cmd.Flags().String(flagTrackerToken, "", "")
	cmd.Flags().String(flagGitHostBaseURL, "", "")
	cmd.Flags().String(flagTrackerBaseURL, "", "")
	for _, f := range []string{flagGitHostToken, flagTrackerToken, flagGitHostBaseURL, flagTrackerBaseURL} {
		_ = cmd.Flags().MarkHidden(f)
	}

	return cmd
}

func runRun(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	sinceFlag, _ := cmd.Flags().GetString("since")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	plain, _ := cmd.Flags().GetBool("plain")
	gitHostToken, _ := cmd.Flags().GetString(flagGitHostToken)
	trackerToken, _ := cmd.Flags().GetString(flagTrackerToken)
	gitHostBaseURL, _ := cmd.Flags().GetString(flagGitHostBaseURL)
	trackerBaseURL, _ := cmd.Flags().GetString(flagTrackerBaseURL)

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	override, err := resolveCutoverOverride(cfg.AIAdoption.CutoverDate)
	if err != nil {
		return err
	}
	since, err := parseSince(sinceFlag)
	if err != nil {
		return err
	}

	// `run` updates an existing baseline — it never indexes from scratch. Fail
	// fast (and clearly) when there's no state yet rather than silently doing a
	// full first ingest under the wrong command.
	if _, err := os.Stat(store.DefaultPath); err != nil {
		return fmt.Errorf("no state at %s — run `loupe baseline` first", store.DefaultPath)
	}

	out := cmd.OutOrStdout()
	if dryRun {
		_, _ = fmt.Fprintln(out, "config valid; baseline state present; --dry-run set, not writing state")
		return nil
	}

	gitHostToken, trackerToken, err = pipeline.ResolveTokens(cfg, gitHostToken, trackerToken, false)
	if err != nil {
		return err
	}
	gh, err := pipeline.BuildGitHost(cfg, gitHostToken, gitHostBaseURL)
	if err != nil {
		return err
	}
	trk, err := pipeline.BuildTracker(cfg, trackerToken, trackerBaseURL)
	if err != nil {
		return err
	}

	opts := &pipeline.Options{
		Cfg: cfg, Override: override,
		GitHostToken: gitHostToken, TrackerToken: trackerToken,
		GitHostBaseURL: gitHostBaseURL, TrackerBaseURL: trackerBaseURL,
		Plain: plain,
		Out:   out,
		Since: since,
		Kind:  "run",
	}
	result, err := pipeline.Run(cmd.Context(), opts, gh, trk)
	if err != nil {
		return err
	}
	// One-line, greppable summary for CI/cron logs (the pipeline already
	// printed the human-readable detail above).
	_, _ = fmt.Fprintf(out, "loupe run: %s\n", result.Summary())
	return nil
}

// parseSince accepts an RFC3339 timestamp or a YYYY-MM-DD date. An empty string
// returns the zero time, which tells the pipeline to use the stored watermarks.
func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := config.ParseCutoverDate(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse --since %q: want RFC3339 or YYYY-MM-DD", s)
	}
	return t.UTC(), nil
}

// resolveCutoverOverride mirrors the baseline helper but without a CLI flag —
// `run` takes the cutover only from config.
func resolveCutoverOverride(configValue string) (time.Time, error) {
	if configValue == "" {
		return time.Time{}, nil
	}
	t, err := config.ParseCutoverDate(configValue)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse cutover date %q: %w", configValue, err)
	}
	return t, nil
}
