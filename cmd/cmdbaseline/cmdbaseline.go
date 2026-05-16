package cmdbaseline

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/auth"
	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/deck"
	"github.com/StephanSchmidt/loupe/internal/githost"
	"github.com/StephanSchmidt/loupe/internal/githost/bitbucket"
	ghHost "github.com/StephanSchmidt/loupe/internal/githost/github"
	"github.com/StephanSchmidt/loupe/internal/ingest"
	"github.com/StephanSchmidt/loupe/internal/store"
	"github.com/StephanSchmidt/loupe/internal/tracker"
	ghTracker "github.com/StephanSchmidt/loupe/internal/tracker/github"
	"github.com/StephanSchmidt/loupe/internal/tracker/jira"
)

const (
	defaultConfigPath = "loupe.yaml"
	stateDBPath       = ".loupe/state.db"
)

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

type baselineOpts struct {
	cfg            *config.Config
	override       time.Time
	gitHostToken   string
	trackerToken   string
	gitHostBaseURL string
	trackerBaseURL string
	repoFilter     string
	projectFilter  string
	out            io.Writer
}

func runBaseline(cmd *cobra.Command, args []string) error {
	opts, dryRun, err := loadBaselineOpts(cmd)
	if err != nil {
		return err
	}
	if dryRun {
		_, _ = fmt.Fprintln(opts.out, "config valid; --dry-run set, not writing state")
		return nil
	}
	// Fail fast on an unwritable reports dir, before kicking off a multi-minute
	// ingest. RenderDeck does its own MkdirAll later; this is purely a pre-flight.
	if err := os.MkdirAll(opts.cfg.Output.Path, 0o750); err != nil {
		return fmt.Errorf("create reports dir %s: %w", opts.cfg.Output.Path, err)
	}
	gh, err := buildGitHost(opts.cfg, opts.gitHostToken, opts.gitHostBaseURL)
	if err != nil {
		return err
	}
	trk, err := buildTracker(opts.cfg, opts.trackerToken, opts.trackerBaseURL)
	if err != nil {
		return err
	}
	return runPipeline(cmd.Context(), opts, gh, trk)
}

// loadBaselineOpts pulls config + flags + interactive prompts together.
// Returns dryRun as a separate bool so runBaseline can short-circuit
// before constructing API clients.
func loadBaselineOpts(cmd *cobra.Command) (*baselineOpts, bool, error) {
	configPath, _ := cmd.Flags().GetString("config")
	cutoverFlag, _ := cmd.Flags().GetString("cutover-date")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	repoFilter, _ := cmd.Flags().GetString("repo")
	projectFilter, _ := cmd.Flags().GetString("project")
	gitHostToken, _ := cmd.Flags().GetString(flagGitHostToken)
	trackerToken, _ := cmd.Flags().GetString(flagTrackerToken)
	gitHostBaseURL, _ := cmd.Flags().GetString(flagGitHostBaseURL)
	trackerBaseURL, _ := cmd.Flags().GetString(flagTrackerBaseURL)

	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, false, err
	}
	override, err := resolveCutoverOverride(cutoverFlag, cfg.AIAdoption.CutoverDate)
	if err != nil {
		return nil, false, err
	}
	if !dryRun {
		// When both slots use github, a single PAT covers both — prompt
		// once and copy the value across. Whichever CI flag was supplied
		// (if either) seeds ensureToken so no prompt fires in that case.
		if cfg.GitHost.Provider == config.ProviderGitHub && cfg.Tracker.Provider == config.ProviderGitHub {
			seed := gitHostToken
			if seed == "" {
				seed = trackerToken
			}
			tok, err := ensureToken(seed, "GitHub token")
			if err != nil {
				return nil, false, err
			}
			gitHostToken, trackerToken = tok, tok
		} else {
			gitHostToken, err = ensureToken(gitHostToken, gitHostTokenLabel(cfg.GitHost.Provider))
			if err != nil {
				return nil, false, err
			}
			trackerToken, err = ensureToken(trackerToken, trackerTokenLabel(cfg.Tracker.Provider))
			if err != nil {
				return nil, false, err
			}
		}
	}
	// Both-github convenience: when the git host and tracker are both
	// github and only --repo is supplied, treat it as the tracker project
	// too. Project keys for GitHub Issues are "owner/repo", matching the
	// repo filter's format exactly.
	if projectFilter == "" && repoFilter != "" &&
		cfg.GitHost.Provider == config.ProviderGitHub &&
		cfg.Tracker.Provider == config.ProviderGitHub {
		projectFilter = repoFilter
	}

	return &baselineOpts{
		cfg: cfg, override: override,
		gitHostToken: gitHostToken, trackerToken: trackerToken,
		gitHostBaseURL: gitHostBaseURL, trackerBaseURL: trackerBaseURL,
		repoFilter: repoFilter, projectFilter: projectFilter,
		out: cmd.OutOrStdout(),
	}, dryRun, nil
}

// gitHostTokenLabel / trackerTokenLabel return the interactive-prompt
// label for the configured provider. Centralising this keeps the prompt
// wording consistent and makes adding a third provider a one-line change.
func gitHostTokenLabel(provider string) string {
	switch provider {
	case config.ProviderBitbucketCloud:
		return "Bitbucket app password"
	case config.ProviderGitHub:
		return "GitHub token (git host)"
	default:
		return "git host token"
	}
}

func trackerTokenLabel(provider string) string {
	switch provider {
	case config.ProviderJiraCloud:
		return "Jira API token"
	case config.ProviderGitHub:
		return "GitHub token (tracker)"
	default:
		return "tracker token"
	}
}

func runPipeline(ctx context.Context, opts *baselineOpts, gh githost.GitHost, trk tracker.Tracker) error {
	s, err := store.Open(stateDBPath)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	if err := runIngest(ctx, opts, s, gh, trk); err != nil {
		return err
	}
	weeks, cutover, err := runAnalyze(ctx, s, opts)
	if err != nil {
		return err
	}
	return renderAndAnnounce(ctx, opts, weeks, cutover, s)
}

func runIngest(ctx context.Context, opts *baselineOpts, s *store.Store, gh githost.GitHost, trk tracker.Tracker) error {
	out := opts.out
	_, _ = fmt.Fprintf(out, "Indexing git host (%s)...\n", gh.Name())
	ghStats, err := ingest.IngestGitHost(ctx, s, gh, out, ingest.GitHostFilter{Repo: opts.repoFilter})
	if err != nil {
		return fmt.Errorf("ingest git host: %w", err)
	}
	_, _ = fmt.Fprintf(out, "  %d workspaces, %d repos, %d commits, %d PRs\n",
		ghStats.Workspaces, ghStats.Repos, ghStats.Commits, ghStats.PullRequests)
	if ghStats.Commits == 0 {
		if opts.repoFilter != "" {
			return fmt.Errorf("no commits indexed for %q — check the --repo value matches a repo the credential can see", opts.repoFilter)
		}
		return fmt.Errorf("no commits indexed — is the credential correct?")
	}
	_, _ = fmt.Fprintf(out, "Indexing tracker (%s)...\n", trk.Name())
	tStats, err := ingest.IngestTracker(ctx, s, trk, out, ingest.TrackerFilter{Project: opts.projectFilter})
	if err != nil {
		return fmt.Errorf("ingest tracker: %w", err)
	}
	_, _ = fmt.Fprintf(out, "  %d projects, %d tickets\n", tStats.Projects, tStats.Issues)
	return nil
}

func runAnalyze(ctx context.Context, s *store.Store, opts *baselineOpts) ([]analyze.WeekStats, analyze.Cutover, error) {
	total, err := analyze.RunAllDetectors(ctx, s, detectionConfigFor(opts.cfg))
	if err != nil {
		return nil, analyze.Cutover{}, err
	}
	_, _ = fmt.Fprintf(opts.out, "  %d AI signals detected\n", total)

	weeks, err := analyze.WeeklyStats(ctx, s)
	if err != nil {
		return nil, analyze.Cutover{}, err
	}
	cutover, err := analyze.DetectCutover(ctx, s, *opts.cfg.AIAdoption.MinWeeklyCommitsForCutover, opts.override)
	if err != nil {
		return nil, analyze.Cutover{}, err
	}
	logCutover(opts.out, cutover)
	return weeks, cutover, nil
}

// detectionConfigFor maps the on-disk config.DetectionConfig to the
// analyze package's DetectionConfig so the analyze package doesn't have
// to import config. SquashMergeRecovery defaults to true via the config
// package's applyDefaults, so the nil check here is defence in depth.
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

func logCutover(out io.Writer, c analyze.Cutover) {
	switch c.Reason {
	case analyze.CutoverReasonAuto:
		_, _ = fmt.Fprintf(out, "  cutover: %s (auto)\n", c.Date.Format("2006-01-02"))
	case analyze.CutoverReasonOverride:
		_, _ = fmt.Fprintf(out, "  cutover: %s (config override)\n", c.Date.Format("2006-01-02"))
	default:
		_, _ = fmt.Fprintln(out, "  cutover: not detected — proceeding with throughput-only view")
	}
}

func renderAndAnnounce(ctx context.Context, opts *baselineOpts, weeks []analyze.WeekStats, cutover analyze.Cutover, s *store.Store) error {
	cycles, err := analyze.WeeklyCycles(ctx, s, analyze.CycleConfig{
		DevStartedStatuses: opts.cfg.CycleTime.DevStartedStatuses,
	})
	if err != nil {
		return fmt.Errorf("weekly cycles: %w", err)
	}

	runID := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	deckDir := filepath.Join(opts.cfg.Output.Path, runID)
	if err := os.MkdirAll(filepath.Dir(deckDir), 0o750); err != nil {
		return fmt.Errorf("create reports dir: %w", err)
	}
	if err := deck.RenderDeck(deckDir, opts.cfg, weeks, cutover, cycles, time.Now().UTC()); err != nil {
		return fmt.Errorf("render deck: %w", err)
	}
	_, _ = fmt.Fprintf(opts.out, "\nDeck ready: %s/index.html\n", deckDir)
	_, _ = fmt.Fprintln(opts.out, "Run `loupe present` to view.")
	return nil
}

// ensureToken returns existing if non-empty, otherwise interactively
// prompts. Smoke tests pass tokens via the hidden flags so no prompt fires.
func ensureToken(existing, label string) (string, error) {
	if existing != "" {
		return existing, nil
	}
	tok, err := auth.PromptToken(label)
	if err != nil {
		return "", err
	}
	return tok, nil
}

// buildGitHost is the explicit registry for git-host providers. Adding a
// case (e.g. ProviderGitLabCloud) is the full plug-in surface.
func buildGitHost(cfg *config.Config, token, baseURLOverride string) (githost.GitHost, error) {
	base := cfg.GitHost.BaseURL
	if baseURLOverride != "" {
		base = baseURLOverride
	}
	switch cfg.GitHost.Provider {
	case config.ProviderBitbucketCloud:
		return bitbucket.New(base, cfg.GitHost.Username, token)
	case config.ProviderGitHub:
		return ghHost.New(base, token)
	default:
		return nil, fmt.Errorf("unsupported git_host.provider %q", cfg.GitHost.Provider)
	}
}

func buildTracker(cfg *config.Config, token, baseURLOverride string) (tracker.Tracker, error) {
	switch cfg.Tracker.Provider {
	case config.ProviderJiraCloud:
		if baseURLOverride != "" {
			return jira.NewWithBaseURL(baseURLOverride, cfg.Tracker.Email, token)
		}
		if cfg.Tracker.BaseURL != "" {
			return jira.NewWithBaseURL(cfg.Tracker.BaseURL, cfg.Tracker.Email, token)
		}
		return jira.New(cfg.Tracker.Site, cfg.Tracker.Email, token)
	case config.ProviderGitHub:
		base := cfg.Tracker.BaseURL
		if baseURLOverride != "" {
			base = baseURLOverride
		}
		return ghTracker.New(base, token)
	default:
		return nil, fmt.Errorf("unsupported tracker.provider %q", cfg.Tracker.Provider)
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
