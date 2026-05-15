// Package cmdstats exposes `loupe stats`: distribution summaries of the
// weekly commit / AI-commit / adoption series. Pure read against the
// existing sqlite state — no API calls, no deck output.
package cmdstats

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/montanaflynn/stats"
	"github.com/spf13/cobra"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/store"
)

const (
	defaultConfigPath = "loupe.yaml"
	stateDBPath       = ".loupe/state.db"
)

func BuildStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "stats",
		Short:        "Distribution stats (mean/median/p10/p90) over the weekly series",
		SilenceUsage: true,
		RunE:         runStats,
	}
	cmd.Flags().String("config", defaultConfigPath, "path to loupe.yaml")
	return cmd
}

func runStats(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	configPath, _ := cmd.Flags().GetString("config")

	if _, err := os.Stat(stateDBPath); err != nil {
		_, _ = fmt.Fprintf(out, "No state yet at %s — run `loupe baseline` first.\n", stateDBPath)
		return nil
	}

	threshold, override, err := loadCutoverConfig(configPath)
	if err != nil {
		return err
	}

	s, err := store.Open(stateDBPath)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	return WriteStats(cmd.Context(), s, out, threshold, override)
}

// loadCutoverConfig returns the cutover threshold + override the same way
// `loupe render` derives them, but degrades to defaults if loupe.yaml is
// missing — `loupe stats` is a read-only diagnostic and shouldn't refuse
// to run just because a config hasn't been written.
func loadCutoverConfig(path string) (threshold float64, override time.Time, _ error) {
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0.05, time.Time{}, nil
		}
		return 0, time.Time{}, err
	}
	threshold = *cfg.AIAdoption.MinWeeklyCommitsForCutover
	if cfg.AIAdoption.CutoverDate != "" {
		override, err = config.ParseCutoverDate(cfg.AIAdoption.CutoverDate)
		if err != nil {
			return 0, time.Time{}, err
		}
	}
	return threshold, override, nil
}

// WriteStats is the format-and-print path, separated from runStats so
// tests can drive it against a seeded in-memory store.
func WriteStats(ctx context.Context, s *store.Store, out io.Writer, threshold float64, override time.Time) error {
	weeks, err := analyze.WeeklyStats(ctx, s)
	if err != nil {
		return err
	}
	if len(weeks) == 0 {
		_, _ = fmt.Fprintln(out, "No weekly data yet — run `loupe baseline` first.")
		return nil
	}

	cutover, err := analyze.DetectCutover(ctx, s, threshold, override)
	if err != nil {
		return err
	}

	commits, ai, ratio := seriesFromWeeks(weeks)

	_, _ = fmt.Fprintf(out, "Weeks: %d  (%s → %s)\n",
		len(weeks),
		weeks[0].WeekStart.Format("2006-01-02"),
		weeks[len(weeks)-1].WeekStart.Format("2006-01-02"),
	)
	_, _ = fmt.Fprintln(out)
	writeRow(out, "Weekly commits   ", commits, false)
	writeRow(out, "Weekly AI commits", ai, false)
	writeRow(out, "AI commit ratio  ", ratio, true)
	writeTrend(out, ratio)

	if err := writeToolBreakdown(ctx, s, out); err != nil {
		return err
	}
	if err := writeAuthorAdoption(ctx, s, out); err != nil {
		return err
	}
	if err := writeRevertRate(ctx, s, out); err != nil {
		return err
	}

	if cutover.Detected {
		writeCutoverSplit(out, weeks, cutover)
	}
	return nil
}

// writeTrend reports the slope of a least-squares fit through the weekly
// AI-ratio series. Slope is the change in ratio per week index — converted
// to percentage points for human readability. Single data point or a flat
// series produces no useful slope, so we skip the line in that case.
func writeTrend(out io.Writer, ratio []float64) {
	if len(ratio) < 2 {
		return
	}
	series := make(stats.Series, len(ratio))
	for i, v := range ratio {
		series[i] = stats.Coordinate{X: float64(i), Y: v}
	}
	fitted, err := stats.LinearRegression(series)
	if err != nil || len(fitted) < 2 {
		return
	}
	dx := fitted[len(fitted)-1].X - fitted[0].X
	if dx == 0 {
		return
	}
	slope := (fitted[len(fitted)-1].Y - fitted[0].Y) / dx
	direction := "flat"
	switch {
	case slope > 0.005:
		direction = "rising"
	case slope < -0.005:
		direction = "falling"
	}
	_, _ = fmt.Fprintf(out, "Trend (AI ratio):   %+5.1f pp/week (%s)\n", slope*100, direction)
}

func writeToolBreakdown(ctx context.Context, s *store.Store, out io.Writer) error {
	rows, err := s.DB().QueryContext(ctx, `
        SELECT sig.signal_source, sig.commit_sha, c.author_email, c.author_name
        FROM ai_signals sig
        JOIN commits c ON c.sha = sig.commit_sha
    `)
	if err != nil {
		return fmt.Errorf("query ai tool breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// (source → set of commit shas) so multiple signals on one commit don't
	// double-count. Bot commits drop out before we ever record them.
	perSource := make(map[string]map[string]struct{})
	distinctAI := make(map[string]struct{})

	for rows.Next() {
		var source, sha, email, name string
		if err := rows.Scan(&source, &sha, &email, &name); err != nil {
			return fmt.Errorf("scan tool row: %w", err)
		}
		if analyze.IsBot(email, name) {
			continue
		}
		distinctAI[sha] = struct{}{}
		if _, ok := perSource[source]; !ok {
			perSource[source] = make(map[string]struct{})
		}
		perSource[source][sha] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(perSource) == 0 {
		return nil
	}

	type toolRow struct {
		source string
		n      int
	}
	tools := make([]toolRow, 0, len(perSource))
	total := 0
	for src, shas := range perSource {
		tools = append(tools, toolRow{src, len(shas)})
		total += len(shas)
	}
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].n != tools[j].n {
			return tools[i].n > tools[j].n
		}
		return tools[i].source < tools[j].source
	})

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "AI tools (%d signals across %d commits):\n", total, len(distinctAI))
	for _, r := range tools {
		_, _ = fmt.Fprintf(out, "  %-8s  %4d  (%5.1f%%)\n", r.source, r.n, float64(r.n)/float64(total)*100)
	}
	return nil
}

func writeAuthorAdoption(ctx context.Context, s *store.Store, out io.Writer) error {
	rows, err := s.DB().QueryContext(ctx, `
        SELECT c.author_email, c.author_name,
               MAX(CASE WHEN sig.commit_sha IS NOT NULL THEN 1 ELSE 0 END) AS has_ai
        FROM commits c
        LEFT JOIN ai_signals sig ON sig.commit_sha = c.sha
        GROUP BY c.author_email, c.author_name
    `)
	if err != nil {
		return fmt.Errorf("query author adoption: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// One author can appear under multiple display names. We collapse to a
	// single (email, hasAI) by OR'ing across name variants.
	aiByEmail := make(map[string]bool)
	for rows.Next() {
		var email, name string
		var hasAI int
		if err := rows.Scan(&email, &name, &hasAI); err != nil {
			return fmt.Errorf("scan adoption row: %w", err)
		}
		if analyze.IsBot(email, name) {
			continue
		}
		if hasAI == 1 {
			aiByEmail[email] = true
		} else if _, seen := aiByEmail[email]; !seen {
			aiByEmail[email] = false
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(aiByEmail) == 0 {
		return nil
	}

	var nonAI []string
	aiCount := 0
	for email, hasAI := range aiByEmail {
		if hasAI {
			aiCount++
		} else {
			nonAI = append(nonAI, email)
		}
	}
	sort.Strings(nonAI)
	pct := float64(aiCount) / float64(len(aiByEmail)) * 100

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Authors with AI commits: %d of %d  (%.1f%%)\n", aiCount, len(aiByEmail), pct)
	if len(nonAI) > 0 {
		_, _ = fmt.Fprintf(out, "Authors without AI commits (%d):\n", len(nonAI))
		for _, a := range nonAI {
			_, _ = fmt.Fprintf(out, "  %s\n", a)
		}
	}
	return nil
}

// revertSHAPattern extracts the original SHA from a `git revert`-style
// commit message body. Git writes "This reverts commit <sha>." — the
// line is present whether the revert was scripted (Revert "...") or
// hand-written.
var revertSHAPattern = regexp.MustCompile(`This reverts commit ([0-9a-fA-F]{7,40})`)

// revertCommit is the post-filter row shape we keep in memory to count
// reverts and resolve revert backlinks against. Bots have already been
// dropped by the time a row becomes a revertCommit.
type revertCommit struct {
	sha     string
	message string
	isAI    bool
}

type revertScan struct {
	all          map[string]revertCommit
	revertedSHAs map[string]struct{}
	totalCommits int
	totalAI      int
	revertCount  int
}

func writeRevertRate(ctx context.Context, s *store.Store, out io.Writer) error {
	scan, err := loadRevertScan(ctx, s)
	if err != nil {
		return err
	}
	if scan.totalCommits == 0 {
		return nil
	}

	aiReverted, humanReverted := classifyReverts(scan)

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Reverts: %d of %d commits  (%.1f%%)\n",
		scan.revertCount, scan.totalCommits,
		float64(scan.revertCount)/float64(scan.totalCommits)*100)
	humanCommits := scan.totalCommits - scan.totalAI
	if scan.totalAI > 0 {
		_, _ = fmt.Fprintf(out, "  AI commits reverted:    %d of %4d  (%5.1f%%)\n",
			aiReverted, scan.totalAI, float64(aiReverted)/float64(scan.totalAI)*100)
	}
	if humanCommits > 0 {
		_, _ = fmt.Fprintf(out, "  Human commits reverted: %d of %4d  (%5.1f%%)\n",
			humanReverted, humanCommits, float64(humanReverted)/float64(humanCommits)*100)
	}
	return nil
}

func loadRevertScan(ctx context.Context, s *store.Store) (revertScan, error) {
	rows, err := s.DB().QueryContext(ctx, `
        SELECT c.sha, c.message, c.author_email, c.author_name,
               CASE WHEN sig.commit_sha IS NOT NULL THEN 1 ELSE 0 END AS is_ai
        FROM commits c
        LEFT JOIN (SELECT DISTINCT commit_sha FROM ai_signals) sig
            ON sig.commit_sha = c.sha
    `)
	if err != nil {
		return revertScan{}, fmt.Errorf("query reverts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	scan := revertScan{
		all:          make(map[string]revertCommit),
		revertedSHAs: make(map[string]struct{}),
	}
	for rows.Next() {
		var r revertCommit
		var email, name string
		var ai int
		if err := rows.Scan(&r.sha, &r.message, &email, &name, &ai); err != nil {
			return revertScan{}, fmt.Errorf("scan revert row: %w", err)
		}
		if analyze.IsBot(email, name) {
			continue
		}
		r.isAI = ai == 1
		scan.all[r.sha] = r
		scan.totalCommits++
		if r.isAI {
			scan.totalAI++
		}
		if m := revertSHAPattern.FindStringSubmatch(r.message); m != nil {
			scan.revertCount++
			scan.revertedSHAs[m[1]] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return revertScan{}, err
	}
	return scan, nil
}

func classifyReverts(scan revertScan) (aiReverted, humanReverted int) {
	for sha := range scan.revertedSHAs {
		c, ok := lookupRevertTarget(scan.all, sha)
		if !ok {
			continue
		}
		if c.isAI {
			aiReverted++
		} else {
			humanReverted++
		}
	}
	return aiReverted, humanReverted
}

// lookupRevertTarget resolves a revert backlink to its target commit. A
// revert message may carry either a full SHA (direct map hit) or a 7+
// character prefix (linear prefix scan against indexed commits).
func lookupRevertTarget(all map[string]revertCommit, sha string) (revertCommit, bool) {
	if c, ok := all[sha]; ok {
		return c, true
	}
	if len(sha) < 7 {
		return revertCommit{}, false
	}
	for fullSHA, row := range all {
		if len(fullSHA) >= len(sha) && fullSHA[:len(sha)] == sha {
			return row, true
		}
	}
	return revertCommit{}, false
}

func seriesFromWeeks(weeks []analyze.WeekStats) (commits, ai, ratio []float64) {
	commits = make([]float64, len(weeks))
	ai = make([]float64, len(weeks))
	ratio = make([]float64, len(weeks))
	for i, w := range weeks {
		commits[i] = float64(w.TotalCommits)
		ai[i] = float64(w.AICommits)
		ratio[i] = w.CommitRatio()
	}
	return commits, ai, ratio
}

// summary holds the six descriptive numbers we print for each series.
// Each is wrapped in an error from montanaflynn/stats — only Percentile
// can fail under our inputs (empty slice or invalid percentile), and we
// short-circuit on empty slices before computing.
type summary struct {
	mean, median, p10, p90, min, max float64
}

func summarise(xs []float64) (summary, error) {
	d := stats.Float64Data(xs)
	mean, err := d.Mean()
	if err != nil {
		return summary{}, fmt.Errorf("mean: %w", err)
	}
	median, err := d.Median()
	if err != nil {
		return summary{}, fmt.Errorf("median: %w", err)
	}
	p10, err := d.Percentile(10)
	if err != nil {
		return summary{}, fmt.Errorf("p10: %w", err)
	}
	p90, err := d.Percentile(90)
	if err != nil {
		return summary{}, fmt.Errorf("p90: %w", err)
	}
	mn, err := d.Min()
	if err != nil {
		return summary{}, fmt.Errorf("min: %w", err)
	}
	mx, err := d.Max()
	if err != nil {
		return summary{}, fmt.Errorf("max: %w", err)
	}
	return summary{mean: mean, median: median, p10: p10, p90: p90, min: mn, max: mx}, nil
}

func writeRow(out io.Writer, label string, xs []float64, asPercent bool) {
	s, err := summarise(xs)
	if err != nil {
		_, _ = fmt.Fprintf(out, "%s  (n/a: %v)\n", label, err)
		return
	}
	f := formatPlain
	if asPercent {
		f = formatPercent
	}
	_, _ = fmt.Fprintf(out, "%s  mean %s  median %s  p10 %s  p90 %s  min %s  max %s\n",
		label, f(s.mean), f(s.median), f(s.p10), f(s.p90), f(s.min), f(s.max))
}

func writeCutoverSplit(out io.Writer, weeks []analyze.WeekStats, c analyze.Cutover) {
	var pre, post []analyze.WeekStats
	for _, w := range weeks {
		if w.WeekStart.Before(c.Date) {
			pre = append(pre, w)
		} else {
			post = append(post, w)
		}
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Cutover: %s (%s, threshold %.1f%%)\n",
		c.Date.Format("2006-01-02"), c.Reason, c.Threshold*100)
	writeCohort(out, "  Before", pre)
	writeCohort(out, "  After ", post)
}

func writeCohort(out io.Writer, label string, weeks []analyze.WeekStats) {
	if len(weeks) == 0 {
		_, _ = fmt.Fprintf(out, "%s  (no weeks)\n", label)
		return
	}
	commits, _, ratio := seriesFromWeeks(weeks)
	cMean, _ := stats.Float64Data(commits).Mean()
	rMean, _ := stats.Float64Data(ratio).Mean()
	_, _ = fmt.Fprintf(out, "%s (%2d weeks)  commits mean %s  AI ratio mean %s\n",
		label, len(weeks), formatPlain(cMean), formatPercent(rMean))
}

func formatPlain(v float64) string   { return fmt.Sprintf("%6.1f", v) }
func formatPercent(v float64) string { return fmt.Sprintf("%5.1f%%", v*100) }
