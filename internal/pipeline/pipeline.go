// Package pipeline holds the shared ingest → analyze → render pipeline that
// backs both `loupe baseline` and `loupe run`. The two commands differ only in
// ergonomics (state-required, --since override, summary line); the actual work
// of fetching from providers, detecting AI signals, and rendering the deck is
// identical and lives here.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/term"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/deck"
	"github.com/StephanSchmidt/loupe/internal/githost"
	"github.com/StephanSchmidt/loupe/internal/ingest"
	"github.com/StephanSchmidt/loupe/internal/progress"
	"github.com/StephanSchmidt/loupe/internal/store"
	"github.com/StephanSchmidt/loupe/internal/tracker"
)

// Options carries everything the pipeline needs for one ingest+render pass.
// Both baseline and run construct it from their own flags; Kind distinguishes
// the two in the runs table, and Since (zero = use watermarks) backs
// `run --since`.
type Options struct {
	Cfg            *config.Config
	Override       time.Time // cutover override
	GitHostToken   string
	TrackerToken   string
	GitHostBaseURL string
	TrackerBaseURL string
	RepoFilter     string
	ProjectFilter  string
	Plain          bool
	Out            io.Writer
	Since          time.Time // override window start; zero = per-source watermarks
	Kind           string    // "baseline" | "run" — recorded in the runs table
}

// Result summarises one pipeline run for the caller to format. DeckDir is
// empty when rendering was skipped because the store was already up to date.
type Result struct {
	GitHost   ingest.GitHostStats
	Tracker   ingest.TrackerStats
	AISignals int
	DeckDir   string
	UpToDate  bool
}

// Summary renders the one-line, CI-greppable description of a run.
func (r Result) Summary() string {
	if r.DeckDir == "" {
		return fmt.Sprintf("%d commits, %d PRs, %d tickets, %d AI signals — already up to date",
			r.GitHost.Commits, r.GitHost.PullRequests, r.Tracker.Issues, r.AISignals)
	}
	return fmt.Sprintf("%d commits, %d PRs, %d tickets, %d AI signals → %s/index.html",
		r.GitHost.Commits, r.GitHost.PullRequests, r.Tracker.Issues, r.AISignals, r.DeckDir)
}

// errAlreadyUpToDate signals a clean, render-skipping exit: ingest fetched no
// new commits but the store already holds data, so a re-run has nothing to do.
var errAlreadyUpToDate = errors.New("already up to date")

// Run opens the state store and executes the full ingest → analyze → render
// pipeline, recording a row in the runs table. It returns a Result the caller
// formats; when ingest finds nothing new and the store already has data, the
// Result has UpToDate set, DeckDir empty, and Run returns a nil error.
func Run(ctx context.Context, opts *Options, gh githost.GitHost, trk tracker.Tracker) (Result, error) {
	s, err := store.Open(store.DefaultPath)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = s.Close() }()

	runID, err := s.StartRun(ctx, opts.Kind)
	if err != nil {
		return Result{}, err
	}
	var result Result
	finish := func(status string) {
		last, _ := s.MaxCommitTime(ctx, gh.Name())
		_ = s.FinishRun(ctx, runID, status, last, result.Summary())
	}

	ghStats, tStats, err := runIngest(ctx, opts, s, gh, trk)
	result.GitHost, result.Tracker = ghStats, tStats
	if err != nil {
		// Nothing new to index — the store is already current. Close the run
		// cleanly (status ok) without regenerating the deck.
		if errors.Is(err, errAlreadyUpToDate) {
			result.UpToDate = true
			finish(store.RunStatusOK)
			return result, nil
		}
		finish(store.RunStatusError)
		return result, err
	}

	weeks, cutover, signals, err := runAnalyze(ctx, s, opts)
	if err != nil {
		finish(store.RunStatusError)
		return result, err
	}
	result.AISignals = signals

	deckDir, err := renderAndAnnounce(ctx, opts, weeks, cutover, s)
	if err != nil {
		finish(store.RunStatusError)
		return result, err
	}
	result.DeckDir = deckDir
	finish(store.RunStatusOK)
	return result, nil
}

// newReporter returns an animated progress reporter when stdout is a real
// terminal and Plain wasn't set; otherwise a plain line-per-event one (so
// piped/CI output stays clean and greppable).
func newReporter(opts *Options) progress.Reporter {
	if !opts.Plain {
		if f, ok := opts.Out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
				return progress.Live(f, w)
			}
		}
	}
	return progress.Plain(opts.Out)
}

// ingestOutcome decides what to do after a git-host ingest pass. fetched is the
// commit count fetched this run; stored is the total already in the store for
// this provider. Returning skipRender requests a clean exit without
// re-rendering.
func ingestOutcome(fetched, stored int, repoFilter string) (skipRender bool, err error) {
	if fetched > 0 {
		return false, nil // new data — proceed to analyze + render
	}
	if stored > 0 {
		return true, nil // nothing new, but we have data — up to date
	}
	if repoFilter != "" {
		return false, fmt.Errorf("no commits indexed for %q — check the --repo value matches a repo the credential can see", repoFilter)
	}
	return false, fmt.Errorf("no commits indexed — is the credential correct?")
}

func runIngest(ctx context.Context, opts *Options, s *store.Store, gh githost.GitHost, trk tracker.Tracker) (ingest.GitHostStats, ingest.TrackerStats, error) {
	out := opts.Out
	_, _ = fmt.Fprintf(out, "Indexing git host (%s)...\n", gh.Name())
	reporter := newReporter(opts)
	ghStats, err := ingest.IngestGitHost(ctx, s, gh, reporter, ingest.GitHostFilter{
		Repo:                opts.RepoFilter,
		SquashMergeRecovery: detectionConfigFor(opts.Cfg).SquashMergeRecovery,
		Since:               opts.Since,
	})
	reporter.Stop()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintln(out, "\nInterrupted — progress saved. Run the command again to continue.")
		}
		return ghStats, ingest.TrackerStats{}, fmt.Errorf("ingest git host: %w", err)
	}
	_, _ = fmt.Fprintf(out, "  %d workspaces, %d repos, %d commits, %d PRs\n",
		ghStats.Workspaces, ghStats.Repos, ghStats.Commits, ghStats.PullRequests)

	// Always index the tracker too — even when git-host had nothing new the
	// tracker may have new tickets, or may never have been ingested at all.
	_, _ = fmt.Fprintf(out, "Indexing tracker (%s)...\n", trk.Name())
	tStats, err := ingest.IngestTracker(ctx, s, trk, out, ingest.TrackerFilter{
		Project: opts.ProjectFilter,
		Since:   opts.Since,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintln(out, "\nInterrupted — progress saved. Run the command again to continue.")
		}
		return ghStats, tStats, fmt.Errorf("ingest tracker: %w", err)
	}
	_, _ = fmt.Fprintf(out, "  %d projects, %d tickets\n", tStats.Projects, tStats.Issues)

	stored, err := s.CommitCount(ctx, gh.Name())
	if err != nil {
		return ghStats, tStats, err
	}
	// Nothing new from either side and we already have data → up to date.
	skipRender, err := ingestOutcome(ghStats.Commits+tStats.Issues, stored, opts.RepoFilter)
	if err != nil {
		return ghStats, tStats, err
	}
	if skipRender {
		_, _ = fmt.Fprintf(out, "Already up to date (%d commits indexed). Run `loupe present` to view the latest deck.\n", stored)
		return ghStats, tStats, errAlreadyUpToDate
	}
	return ghStats, tStats, nil
}

func runAnalyze(ctx context.Context, s *store.Store, opts *Options) ([]analyze.WeekStats, analyze.Cutover, int, error) {
	total, err := analyze.RunAllDetectors(ctx, s, detectionConfigFor(opts.Cfg))
	if err != nil {
		return nil, analyze.Cutover{}, 0, err
	}
	_, _ = fmt.Fprintf(opts.Out, "  %d AI signals detected\n", total)

	weeks, err := analyze.WeeklyStats(ctx, s)
	if err != nil {
		return nil, analyze.Cutover{}, 0, err
	}
	cutover, err := analyze.DetectCutover(ctx, s, *opts.Cfg.AIAdoption.MinWeeklyCommitsForCutover, opts.Override)
	if err != nil {
		return nil, analyze.Cutover{}, 0, err
	}
	logCutover(opts.Out, cutover)
	return weeks, cutover, total, nil
}

// detectionConfigFor maps the on-disk config.DetectionConfig to the analyze
// package's DetectionConfig so analyze doesn't have to import config.
// SquashMergeRecovery defaults to true via the config package's applyDefaults,
// so the nil check here is defence in depth.
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

// renderAndAnnounce computes the remaining deck inputs, renders the deck, and
// prints where it landed. It returns the deck directory.
func renderAndAnnounce(ctx context.Context, opts *Options, weeks []analyze.WeekStats, cutover analyze.Cutover, s *store.Store) (string, error) {
	runID := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	deckDir := filepath.Join(opts.Cfg.Output.Path, runID)
	if err := os.MkdirAll(filepath.Dir(deckDir), 0o750); err != nil {
		return "", fmt.Errorf("create reports dir: %w", err)
	}
	in, err := deck.GatherInputs(ctx, s, opts.Cfg)
	if err != nil {
		return "", err
	}
	if err := deck.RenderDeck(deckDir, opts.Cfg, weeks, cutover, in, time.Now().UTC()); err != nil {
		return "", fmt.Errorf("render deck: %w", err)
	}
	_, _ = fmt.Fprintf(opts.Out, "\nDeck ready: %s/index.html\n", deckDir)
	_, _ = fmt.Fprintln(opts.Out, "Run `loupe present` to view.")
	return deckDir, nil
}
