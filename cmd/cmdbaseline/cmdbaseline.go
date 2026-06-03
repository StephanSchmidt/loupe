package cmdbaseline

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/pipeline"
)

const defaultConfigPath = "loupe.yaml"

// Hidden flag names — used by smoke tests / CI; not advertised in `--help`.
// They're provider-neutral because the configured provider determines
// which credentials are expected.
const (
	flagGitHostToken   = "git-host-token"
	flagTrackerToken   = "tracker-token"
	flagGitHostBaseURL = "git-host-base-url"
	flagTrackerBaseURL = "tracker-base-url"
)

func BuildBaselineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "baseline",
		Short: "First run — ingest from configured providers and render the deck",
		Long: `Indexes every workspace + project the supplied credentials can see,
detects AI adoption, and renders a reveal.js deck under <output.path>/<timestamp>/.

Tokens are prompted (echo off) every invocation — no env vars in v0.`,
		SilenceUsage: true,
		RunE:         runBaseline,
	}

	cmd.Flags().String("config", defaultConfigPath, "path to loupe.yaml")
	cmd.Flags().String("cutover-date", "", "override AI-adoption cutover (YYYY-MM-DD)")
	cmd.Flags().Bool("dry-run", false, "validate config without writing state")
	cmd.Flags().String("repo", "", "limit to a single repo (e.g. owner/slug); skips every other repo before any commit API call")
	cmd.Flags().String("project", "", "limit to a single tracker project key (e.g. ENG, or owner/repo for GitHub Issues); defaults to --repo when both providers are github")
	cmd.Flags().Bool("plain", false, "disable the animated progress display; print plain lines (auto-disabled when stdout isn't a terminal)")
	cmd.Flags().Int("months", 0, "months of recent history to show in the deck charts (overrides config display_months; default 6)")

	// Hidden test-only flags. Documented surface stays "every invocation prompts".
	cmd.Flags().String(flagGitHostToken, "", "")
	cmd.Flags().String(flagTrackerToken, "", "")
	cmd.Flags().String(flagGitHostBaseURL, "", "")
	cmd.Flags().String(flagTrackerBaseURL, "", "")
	for _, f := range []string{flagGitHostToken, flagTrackerToken, flagGitHostBaseURL, flagTrackerBaseURL} {
		_ = cmd.Flags().MarkHidden(f)
	}

	return cmd
}

func runBaseline(cmd *cobra.Command, args []string) error {
	opts, dryRun, err := loadBaselineOpts(cmd)
	if err != nil {
		return err
	}
	if dryRun {
		_, _ = fmt.Fprintln(opts.Out, "config valid; --dry-run set, not writing state")
		return nil
	}
	// Fail fast on an unwritable reports dir, before kicking off a multi-minute
	// ingest. RenderDeck does its own MkdirAll later; this is purely a pre-flight.
	if err := os.MkdirAll(opts.Cfg.Output.Path, 0o750); err != nil {
		return fmt.Errorf("create reports dir %s: %w", opts.Cfg.Output.Path, err)
	}
	gh, err := pipeline.BuildGitHost(opts.Cfg, opts.GitHostToken, opts.GitHostBaseURL)
	if err != nil {
		return err
	}
	trk, err := pipeline.BuildTracker(opts.Cfg, opts.TrackerToken, opts.TrackerBaseURL)
	if err != nil {
		return err
	}
	_, err = pipeline.Run(cmd.Context(), opts, gh, trk)
	return err
}

// loadBaselineOpts pulls config + flags + interactive prompts together.
// Returns dryRun as a separate bool so runBaseline can short-circuit
// before constructing API clients.
func loadBaselineOpts(cmd *cobra.Command) (*pipeline.Options, bool, error) {
	configPath, _ := cmd.Flags().GetString("config")
	cutoverFlag, _ := cmd.Flags().GetString("cutover-date")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	repoFilter, _ := cmd.Flags().GetString("repo")
	projectFilter, _ := cmd.Flags().GetString("project")
	plain, _ := cmd.Flags().GetBool("plain")
	gitHostToken, _ := cmd.Flags().GetString(flagGitHostToken)
	trackerToken, _ := cmd.Flags().GetString(flagTrackerToken)
	gitHostBaseURL, _ := cmd.Flags().GetString(flagGitHostBaseURL)
	trackerBaseURL, _ := cmd.Flags().GetString(flagTrackerBaseURL)

	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, false, err
	}
	if months, _ := cmd.Flags().GetInt("months"); months > 0 {
		cfg.Windows.DisplayMonths = months
	}
	override, err := resolveCutoverOverride(cutoverFlag, cfg.AIAdoption.CutoverDate)
	if err != nil {
		return nil, false, err
	}
	gitHostToken, trackerToken, err = pipeline.ResolveTokens(cfg, gitHostToken, trackerToken, dryRun)
	if err != nil {
		return nil, false, err
	}
	// Same-provider convenience: when the git host and tracker are both
	// github (project key = "owner/repo") or both gitlab (project key =
	// "group/.../project" matching path_with_namespace), --repo doubles as
	// --project.
	if projectFilter == "" && repoFilter != "" && pipeline.SameSinglePATProvider(cfg) {
		projectFilter = repoFilter
	}

	return &pipeline.Options{
		Cfg: cfg, Override: override,
		GitHostToken: gitHostToken, TrackerToken: trackerToken,
		GitHostBaseURL: gitHostBaseURL, TrackerBaseURL: trackerBaseURL,
		RepoFilter: repoFilter, ProjectFilter: projectFilter,
		Plain: plain,
		Out:   cmd.OutOrStdout(),
		Kind:  "baseline",
	}, dryRun, nil
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
