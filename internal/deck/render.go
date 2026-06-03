package deck

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/config"
)

// reveal.css carries reveal's slide-layout primitives. We ship our own
// dark theme inline in template.html.tmpl rather than embedding one of
// reveal's bundled themes (white.css / black.css / …), so the embed
// glob deliberately excludes the theme/ subtree.
//
//go:embed assets/reveal/reveal.js assets/reveal/reveal.css assets/echarts/echarts.min.js assets/logo/loupe.svg
var revealAssets embed.FS

//go:embed template.html.tmpl
var deckTemplate string

// DeckData is the template payload for template.html.tmpl. Fields are
// pre-computed in RenderDeck so the template stays format-only.
type DeckData struct {
	OrgName             string
	Title               string // deck headline; config Title or Org
	Scope               string
	ReportDate          time.Time
	WindowStart         time.Time
	WindowEnd           time.Time
	TotalCommits        int
	AICommits           int
	AICommitPct         float64
	DistinctAuthorCount int
	AIAuthorCount       int
	Weeks               []analyze.WeekStats
	Cutover             analyze.Cutover
	CutoverText         string
	CutoverThresholdPct float64
	Charts              ChartPayload

	// Cycles is populated when ticket data is available; the template
	// hides the cycle slide when CyclesAvailable is false.
	Cycles              []analyze.WeekCycle
	CyclesAvailable     bool
	CycleTickets        int
	CycleFallbackPct    float64

	// Repo-adoption slide: stacked count of repos by AI adoption + activity.
	RepoAdoptionAvailable bool
	RepoAdoptionAdopted   int // repos AI-enabled as of the latest week
	RepoAdoptionTotal     int // repos in existence as of the latest week

	// Output-per-active-developer slide: commits/dev before vs after cutover.
	ProductivityHasCutover bool
	ProductivityBefore     float64
	ProductivityAfter      float64

	// Quality counterweight slide: revert + bug rate, before/after cutover.
	DefectsAvailable  bool
	DefectsHasBugs    bool
	DefectsHasCutover bool
	RevertRateOverall float64
	BugRateOverall    float64
	RevertRateBefore  float64
	RevertRateAfter   float64
	BugRateBefore     float64
	BugRateAfter      float64
	MedianIdeaToDevText string
	MedianDevToRelText  string

	// Bug-fix-speed slide: median engineering days for bug-class tickets,
	// before vs after cutover. BugFixAvailable mirrors Charts.HasBugFix.
	BugFixAvailable  bool
	BugFixHasCutover bool
	BugLeadBefore    float64 // median engineering days, bug cohort, pre-cutover
	BugLeadAfter     float64
	OtherLeadOverall float64 // median engineering days, non-bug cohort, whole window

	// Stats panel — distribution summaries derived from Weeks. Populated
	// when there are ≥2 weeks of data; the template hides the slide when
	// StatsAvailable is false.
	StatsAvailable        bool
	StatsCommits          analyze.Summary
	StatsAICommits        analyze.Summary
	StatsRatioPct         analyze.Summary // ratio multiplied by 100 for percentage display
	StatsTrendSlope       float64         // percentage-points-per-week
	StatsTrendDirection   string          // "rising" | "falling" | "flat"
	StatsTrendKnown       bool
	StatsCutoverAvailable bool
	StatsBeforeWeeks      int
	StatsBeforeCommits    float64
	StatsBeforeRatioPct   float64
	StatsAfterWeeks       int
	StatsAfterCommits     float64
	StatsAfterRatioPct    float64

	// Tool attribution shown on the Stats slide. Empty when no AI
	// signals have been recorded yet.
	Tools             []analyze.ToolSignal
	ToolsAvailable    bool
	ToolsCommitsTotal int // distinct AI-tagged commits across tools
}

// RenderDeck writes a self-contained reveal.js deck under deckDir:
//
//	deckDir/
//	  index.html
//	  assets/reveal.js
//	  assets/reveal.css
//	  assets/echarts.min.js
//	  charts/throughput.png   (paste-into-Slack)
//	  charts/throughput.svg   (high-res embed)
//	  charts/adoption.png
//	  charts/adoption.svg
//
// The slide deck renders charts client-side with Apache ECharts. The
// PNG/SVG files under charts/ are server-side exports for sharing in
// Slack, email, or static docs — they're not referenced by index.html.
//
// deckDir is created if missing. An existing deckDir is overwritten in place.
func RenderDeck(
	deckDir string,
	cfg *config.Config,
	weeks []analyze.WeekStats,
	cutover analyze.Cutover,
	cycles []analyze.WeekCycle,
	repoAdoption []analyze.RepoAdoptionWeek,
	wip []analyze.WIPWeek,
	defects []analyze.DefectWeek,
	bugfix []analyze.BugFixWeek,
	focus []FocusData,
	tools analyze.ToolBreakdownStats,
	reportDate time.Time,
) error {
	if err := os.MkdirAll(deckDir, 0o750); err != nil {
		return fmt.Errorf("create deck dir %s: %w", deckDir, err)
	}

	// Restrict the time-series charts to the recent window so the axes stay
	// readable. Older data still lives in the store and fed cutover
	// detection upstream; it's only hidden from these charts.
	weeks = analyze.WindowWeeks(weeks, cfg.Windows.DisplayMonths)
	cycles = analyze.WindowCycles(cycles, cfg.Windows.DisplayMonths)
	repoAdoption = analyze.WindowRepoAdoption(repoAdoption, cfg.Windows.DisplayMonths)
	wip = analyze.WindowWIP(wip, cfg.Windows.DisplayMonths)
	defects = analyze.WindowDefects(defects, cfg.Windows.DisplayMonths)
	bugfix = analyze.WindowBugFix(bugfix, cfg.Windows.DisplayMonths)
	m := cfg.Windows.DisplayMonths
	for i := range focus {
		focus[i].Weeks = analyze.WindowWeeks(focus[i].Weeks, m)
		focus[i].Cycles = analyze.WindowCycles(focus[i].Cycles, m)
		focus[i].RepoAdoption = analyze.WindowRepoAdoption(focus[i].RepoAdoption, m)
		focus[i].WIP = analyze.WindowWIP(focus[i].WIP, m)
		focus[i].Defects = analyze.WindowDefects(focus[i].Defects, m)
		focus[i].BugFix = analyze.WindowBugFix(focus[i].BugFix, m)
	}

	if err := copyEmbeddedAssets(filepath.Join(deckDir, "assets")); err != nil {
		return err
	}

	payload, err := BuildChartPayload(weeks, cutover, cycles, repoAdoption, wip, defects, bugfix, focus)
	if err != nil {
		return fmt.Errorf("build chart payload: %w", err)
	}

	if err := RenderStaticCharts(weeks, cutover, cycles, repoAdoption, wip, defects, bugfix, filepath.Join(deckDir, "charts")); err != nil {
		return fmt.Errorf("render static charts: %w", err)
	}

	data := buildDeckData(cfg, weeks, cutover, cycles, repoAdoption, reportDate)
	populateDefectSummary(&data, defects, cutover)
	populateProductivitySummary(&data, weeks, cutover)
	populateBugFixSummary(&data, bugfix, cutover)
	data.Tools = tools.Tools
	data.ToolsAvailable = len(tools.Tools) > 0
	data.ToolsCommitsTotal = tools.DistinctCommits
	data.Charts = payload
	tmpl, err := template.New("deck").Parse(deckTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	indexPath := filepath.Join(deckDir, "index.html")
	f, err := os.Create(indexPath) // #nosec G304 -- indexPath is loupe-constructed under the caller-supplied deck dir
	if err != nil {
		return fmt.Errorf("create %s: %w", indexPath, err)
	}
	defer func() { _ = f.Close() }()

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("render template: %w", err)
	}
	return nil
}

// populateProductivitySummary fills the commits-per-active-dev before/after
// cutover headline (weighted by author-weeks so weeks with more devs count
// proportionally).
func populateProductivitySummary(d *DeckData, weeks []analyze.WeekStats, cutover analyze.Cutover) {
	if !cutover.Detected {
		return
	}
	before, after := analyze.SplitByCutover(weeks, cutover)
	if len(before) == 0 || len(after) == 0 {
		return
	}
	perDev := func(ws []analyze.WeekStats) float64 {
		var commits, authorWeeks int
		for _, w := range ws {
			commits += w.TotalCommits
			authorWeeks += w.DistinctAuthors
		}
		if authorWeeks == 0 {
			return 0
		}
		return float64(commits) / float64(authorWeeks)
	}
	d.ProductivityBefore = perDev(before)
	d.ProductivityAfter = perDev(after)
	d.ProductivityHasCutover = true
}

// populateBugFixSummary fills the bug-fix-speed headline: median engineering
// days for the bug cohort before vs after cutover, plus the non-bug baseline.
// Gated by the same minBugCycles floor that hides the chart, so the slide and
// its headline appear together (or not at all).
func populateBugFixSummary(d *DeckData, bugfix []analyze.BugFixWeek, cutover analyze.Cutover) {
	_, otherMed, bugN := analyze.BugFixLeadTimes(bugfix)
	if bugN < minBugCycles {
		return
	}
	d.BugFixAvailable = true
	d.OtherLeadOverall = otherMed.Hours() / 24
	if cutover.Detected {
		before, after := analyze.SplitBugFixByCutover(bugfix, cutover)
		if len(before) > 0 && len(after) > 0 {
			bBug, _, _ := analyze.BugFixLeadTimes(before)
			aBug, _, _ := analyze.BugFixLeadTimes(after)
			d.BugLeadBefore = bBug.Hours() / 24
			d.BugLeadAfter = aBug.Hours() / 24
			d.BugFixHasCutover = true
		}
	}
}

// populateDefectSummary fills the quality-counterweight headline: overall
// revert/bug rates and, when a cutover splits the window, before vs after.
func populateDefectSummary(d *DeckData, defects []analyze.DefectWeek, cutover analyze.Cutover) {
	if len(defects) == 0 {
		return
	}
	d.DefectsAvailable = true
	d.RevertRateOverall, d.BugRateOverall, d.DefectsHasBugs = analyze.DefectRates(defects)
	if cutover.Detected {
		before, after := analyze.SplitDefectsByCutover(defects, cutover)
		if len(before) > 0 && len(after) > 0 {
			d.DefectsHasCutover = true
			d.RevertRateBefore, d.BugRateBefore, _ = analyze.DefectRates(before)
			d.RevertRateAfter, d.BugRateAfter, _ = analyze.DefectRates(after)
		}
	}
}

func buildDeckData(
	cfg *config.Config,
	weeks []analyze.WeekStats,
	cutover analyze.Cutover,
	cycles []analyze.WeekCycle,
	repoAdoption []analyze.RepoAdoptionWeek,
	reportDate time.Time,
) DeckData {
	title := cfg.Title
	if strings.TrimSpace(title) == "" {
		title = cfg.Org
	}
	d := DeckData{
		OrgName:               cfg.Org,
		Title:                 title,
		Scope:                 cfg.Org,
		ReportDate:            reportDate,
		Weeks:                 weeks,
		Cutover:               cutover,
		CutoverThresholdPct:   cutover.Threshold * 100,
		Cycles:                cycles,
		CyclesAvailable:       len(cycles) > 0,
		RepoAdoptionAvailable: len(repoAdoption) > 0,
	}
	if n := len(repoAdoption); n > 0 {
		last := repoAdoption[n-1]
		d.RepoAdoptionAdopted = last.AIEnabled
		d.RepoAdoptionTotal = last.AIEnabled + last.Active + last.Inactive
	}

	if len(cycles) > 0 {
		populateCycleSummary(&d, cycles)
	}

	populateStats(&d, weeks, cutover)

	// Over-window distinct-author counts would need raw commit data; for v0 we
	// take the max of any single week as a conservative proxy — fine for the
	// headline number and keeps this code path query-free.
	for _, w := range weeks {
		d.TotalCommits += w.TotalCommits
		d.AICommits += w.AICommits
		if w.DistinctAuthors > d.DistinctAuthorCount {
			d.DistinctAuthorCount = w.DistinctAuthors
		}
		if w.AIAuthors > d.AIAuthorCount {
			d.AIAuthorCount = w.AIAuthors
		}
	}

	if d.TotalCommits > 0 {
		d.AICommitPct = float64(d.AICommits) / float64(d.TotalCommits) * 100
	}
	if len(weeks) > 0 {
		d.WindowStart = weeks[0].WeekStart
		d.WindowEnd = weeks[len(weeks)-1].WeekStart.AddDate(0, 0, 6)
	} else {
		d.WindowStart = reportDate
		d.WindowEnd = reportDate
	}

	switch cutover.Reason {
	case analyze.CutoverReasonOverride:
		d.CutoverText = fmt.Sprintf("Set in config to %s", cutover.Date.Format("Jan 2, 2006"))
	case analyze.CutoverReasonAuto:
		d.CutoverText = fmt.Sprintf("Auto-detected at %s — first week with ≥%.0f%% AI commits",
			cutover.Date.Format("Jan 2, 2006"), cutover.Threshold*100)
	default:
		d.CutoverText = "No AI cutover detected in this window — adoption trailers may be missing"
	}
	return d
}

// populateCycleSummary fills the headline numbers shown on the cycle
// slide (ticket count, fallback-fraction footnote, two median labels).
// Medians are weighted by ticket count rather than averaging week
// medians, so a high-volume week dominates the headline figure.
func populateCycleSummary(d *DeckData, cycles []analyze.WeekCycle) {
	totalTickets := 0
	totalFallback := 0
	var sumIdeaHours, sumDevHours float64
	for _, w := range cycles {
		totalTickets += w.TicketCount
		totalFallback += w.FallbackTicketCount
		sumIdeaHours += w.MedianIdeaToDev.Hours() * float64(w.TicketCount)
		sumDevHours += w.MedianDevToRelease.Hours() * float64(w.TicketCount)
	}
	d.CycleTickets = totalTickets
	if totalTickets > 0 {
		d.CycleFallbackPct = float64(totalFallback) / float64(totalTickets) * 100
		d.MedianIdeaToDevText = formatHoursAsDays(sumIdeaHours / float64(totalTickets))
		d.MedianDevToRelText = formatHoursAsDays(sumDevHours / float64(totalTickets))
	}
}

// populateStats fills the distribution-summary fields shown on the Stats
// slide. Requires at least 2 weeks for Summarise to succeed.
func populateStats(d *DeckData, weeks []analyze.WeekStats, cutover analyze.Cutover) {
	if len(weeks) < 2 {
		return
	}
	commitsSer, aiSer, ratioSer := analyze.WeeklySeries(weeks)
	cSummary, errC := analyze.Summarise(commitsSer)
	aSummary, errA := analyze.Summarise(aiSer)
	rSummary, errR := analyze.Summarise(ratioSer)
	if errC != nil || errA != nil || errR != nil {
		return
	}
	d.StatsAvailable = true
	d.StatsCommits = cSummary
	d.StatsAICommits = aSummary
	// Convert ratio summary from 0..1 to 0..100 for display.
	d.StatsRatioPct = analyze.Summary{
		Mean:   rSummary.Mean * 100,
		Median: rSummary.Median * 100,
		P10:    rSummary.P10 * 100,
		P90:    rSummary.P90 * 100,
		Min:    rSummary.Min * 100,
		Max:    rSummary.Max * 100,
	}

	if slope, dir, ok := analyze.RatioTrend(ratioSer); ok {
		d.StatsTrendSlope = slope
		d.StatsTrendDirection = string(dir)
		d.StatsTrendKnown = true
	}

	if cutover.Reason != analyze.CutoverReasonNotDetected && !cutover.Date.IsZero() {
		before, after := analyze.SplitByCutover(weeks, cutover)
		bCommits, bRatio := analyze.CohortMeans(before)
		aCommits, aRatio := analyze.CohortMeans(after)
		d.StatsCutoverAvailable = len(before) > 0 && len(after) > 0
		d.StatsBeforeWeeks = len(before)
		d.StatsBeforeCommits = bCommits
		d.StatsBeforeRatioPct = bRatio * 100
		d.StatsAfterWeeks = len(after)
		d.StatsAfterCommits = aCommits
		d.StatsAfterRatioPct = aRatio * 100
	}
}

func formatHoursAsDays(hours float64) string {
	if hours < 24 {
		return fmt.Sprintf("%.1f h", hours)
	}
	return fmt.Sprintf("%.1f d", hours/24)
}

// copyEmbeddedAssets walks each embedded asset subtree (reveal.js,
// echarts.js) and writes its files under dst. The subtree-level prefix is
// stripped so e.g. assets/reveal/reveal.js → dst/reveal.js and
// assets/echarts/echarts.min.js → dst/echarts.min.js. That keeps the HTML
// template's relative-path references flat.
func copyEmbeddedAssets(dst string) error {
	for _, srcPrefix := range []string{"assets/reveal", "assets/echarts", "assets/logo"} {
		if err := copyEmbeddedSubtree(srcPrefix, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyEmbeddedSubtree(srcPrefix, dst string) error {
	return fs.WalkDir(revealAssets, srcPrefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcPrefix, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, err := revealAssets.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		return nil
	})
}
