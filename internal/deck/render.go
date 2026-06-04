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
	DisplayMonths       int // chart window length (config display_months / --months)
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

	// Lead-time before/after (median dev→release days) — added for #3 so the
	// Lead-time slide gets a before/after verdict like the others.
	LeadTimeHasCutover bool
	LeadTimeBefore     float64
	LeadTimeAfter      float64

	// Significance verdicts (#3) on each before↔after delta. Hidden when no
	// cutover or too few weeks to test.
	ProductivityVerdict Verdict
	RevertVerdict       Verdict
	BugRateVerdict      Verdict
	BugFixVerdict       Verdict
	LeadTimeVerdict     Verdict

	// AI Impact Scorecard (#10): three-axis synthesis of the labelled deltas.
	ScorecardAvailable bool
	ScorecardSummary   string // one-line verdict across the three axes
	Scorecard          []ScorecardAxis

	// PR cycle velocity (#5). Available only once merge-time data is ingested.
	PRCycleAvailable  bool
	PRCycleHasCutover bool
	PRDaysBefore      float64 // median days-to-merge before cutover
	PRDaysAfter       float64
	PRMergedPerWeek   float64 // mean merged PRs/week over the window
	PRCycleVerdict    Verdict

	// AI-adoption retention (#7) headline.
	RetentionAvailable bool
	RetentionWeeks     int     // K — weeks-since-adoption used for the headline
	RetentionRate      float64 // % still using AI at week K
	RetentionAdopters  int     // cohort size at week 0

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
	in Inputs,
	reportDate time.Time,
) error {
	if err := os.MkdirAll(deckDir, 0o750); err != nil {
		return fmt.Errorf("create deck dir %s: %w", deckDir, err)
	}

	// Restrict the time-series charts to the recent window so the axes stay
	// readable. Older data still lives in the store and fed cutover
	// detection upstream; it's only hidden from these charts.
	m := cfg.Windows.DisplayMonths
	weeks = analyze.WindowWeeks(weeks, m)
	in.Cycles = analyze.WindowCycles(in.Cycles, m)
	in.RepoAdoption = analyze.WindowRepoAdoption(in.RepoAdoption, m)
	in.WIP = analyze.WindowWIP(in.WIP, m)
	in.Defects = analyze.WindowDefects(in.Defects, m)
	in.BugFix = analyze.WindowBugFix(in.BugFix, m)
	in.PRCycle = analyze.WindowPRCycle(in.PRCycle, m)
	// retention (weeks-since-adoption) and landing (window aggregate) are not
	// calendar-windowed here.
	for i := range in.Focus {
		in.Focus[i].Weeks = analyze.WindowWeeks(in.Focus[i].Weeks, m)
		in.Focus[i].Cycles = analyze.WindowCycles(in.Focus[i].Cycles, m)
		in.Focus[i].RepoAdoption = analyze.WindowRepoAdoption(in.Focus[i].RepoAdoption, m)
		in.Focus[i].WIP = analyze.WindowWIP(in.Focus[i].WIP, m)
		in.Focus[i].Defects = analyze.WindowDefects(in.Focus[i].Defects, m)
		in.Focus[i].BugFix = analyze.WindowBugFix(in.Focus[i].BugFix, m)
		in.Focus[i].PRCycle = analyze.WindowPRCycle(in.Focus[i].PRCycle, m)
	}

	if err := copyEmbeddedAssets(filepath.Join(deckDir, "assets")); err != nil {
		return err
	}

	payload, err := BuildChartPayload(weeks, cutover, in.Cycles, in.RepoAdoption, in.WIP, in.Defects, in.BugFix, in.PRCycle, in.Retention, in.TeamLanding, in.RepoLanding, in.Focus)
	if err != nil {
		return fmt.Errorf("build chart payload: %w", err)
	}

	if err := RenderStaticCharts(weeks, cutover, in.Cycles, in.RepoAdoption, in.WIP, in.Defects, in.BugFix, in.PRCycle, filepath.Join(deckDir, "charts")); err != nil {
		return fmt.Errorf("render static charts: %w", err)
	}

	data := buildDeckData(cfg, weeks, cutover, in.Cycles, in.RepoAdoption, reportDate)
	populateDefectSummary(&data, in.Defects, cutover)
	populateProductivitySummary(&data, weeks, cutover)
	populateBugFixSummary(&data, in.BugFix, cutover)
	populatePRSummary(&data, in.PRCycle, cutover)
	populateRetentionSummary(&data, in.Retention)
	// Significance + scorecard run last: they read BugFixHasCutover set above
	// and operate on the same windowed cohorts the headlines use.
	populateSignificance(&data, weeks, in.Defects, in.BugFix, in.Cycles, cutover)
	data.Tools = in.Tools.Tools
	data.ToolsAvailable = len(in.Tools.Tools) > 0
	data.ToolsCommitsTotal = in.Tools.DistinctCommits
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

// populatePRSummary fills the PR-velocity headline: mean merged/week, and
// median days-to-merge before vs after cutover with a significance verdict.
// Gated by minMergedPRs, so it self-hides until merge-time data is ingested.
func populatePRSummary(d *DeckData, prcycle []analyze.PRWeek, cutover analyze.Cutover) {
	_, perWeek, n := analyze.PRCycleSummary(prcycle)
	if n < minMergedPRs {
		return
	}
	d.PRCycleAvailable = true
	d.PRMergedPerWeek = perWeek
	if cutover.Detected {
		before, after := analyze.SplitPRByCutover(prcycle, cutover)
		if len(before) > 0 && len(after) > 0 {
			bd, _, _ := analyze.PRCycleSummary(before)
			ad, _, _ := analyze.PRCycleSummary(after)
			d.PRDaysBefore = bd
			d.PRDaysAfter = ad
			d.PRCycleHasCutover = true
			delta := analyze.BootstrapDelta(analyze.PRToMergeSeries(before), analyze.PRToMergeSeries(after), true)
			d.PRCycleVerdict = verdictFromDelta(delta)
		}
	}
}

// populateRetentionSummary fills the retention headline: of the adopter cohort,
// the share still using AI at the furthest week with an adequate cohort.
func populateRetentionSummary(d *DeckData, retention []analyze.RetentionPoint) {
	if len(retention) < 3 {
		return
	}
	d.RetentionAvailable = true
	d.RetentionAdopters = retention[0].Cohort
	// Headline the 1-month (4-week) horizon — the standard retention point and
	// the one that exposes a honeymoon drop. Fall back to the last available
	// point when the curve is shorter.
	pick := retention[len(retention)-1]
	for _, p := range retention {
		if p.WeeksSinceAdoption == retentionHeadlineWeek {
			pick = p
			break
		}
	}
	d.RetentionWeeks = pick.WeeksSinceAdoption
	d.RetentionRate = pick.Rate
}

// retentionHeadlineWeek is the weeks-since-adoption horizon used for the
// retention headline (1 month).
const retentionHeadlineWeek = 4

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

// Verdict is the rendered significance label for one before↔after delta — a
// short phrase plus a CSS state class (good/bad/noise). Show is false when the
// cohort is too thin to test, so the template can omit the chip entirely.
type Verdict struct {
	Show  bool
	Label string
	Class string // "good" | "bad" | "noise"
}

// ScorecardAxis is one row of the AI Impact Scorecard: a metric's before→after
// with a direction arrow, good/bad/flat state, and a confidence label.
type ScorecardAxis struct {
	Name       string // metric, direction-intuitive: Output / dev | Lead time | Reverts & bugs
	Headline   string // sub-label, e.g. "commits per active dev"
	BeforeText string
	AfterText  string
	ChangeText string // "+36%" etc., or "—" when undefined
	Arrow      string // ▲ | ▼ | ◼
	State      string // good | bad | flat
	Verdict    string // "improved · high confidence" | "within noise" | "too few weeks"
	Sub        string // optional secondary line (e.g. bug rate)
}

// scoreVerdict turns a good/bad/flat state plus a significance level into the
// explicit badge phrase, so meaning never rests on color alone — a green "−48%"
// on "Reverts & bugs" reads "improved", not "quality fell".
func scoreVerdict(state string, conf analyze.SigLevel) string {
	switch conf {
	case analyze.SigInsufficient:
		return "too few weeks"
	case analyze.SigNoise:
		return "within noise"
	}
	word := "improved"
	if state == "bad" {
		word = "regressed"
	}
	if conf == analyze.SigMedium {
		return word + " · likely (90%)"
	}
	return word + " · high confidence"
}

func stateClass(better bool) string {
	if better {
		return "good"
	}
	return "bad"
}

func verdictFromDelta(d analyze.Delta) Verdict {
	switch d.Confidence {
	case analyze.SigHigh:
		return Verdict{Show: true, Label: "statistically meaningful", Class: stateClass(d.Better)}
	case analyze.SigMedium:
		return Verdict{Show: true, Label: "likely meaningful (90%)", Class: stateClass(d.Better)}
	case analyze.SigNoise:
		return Verdict{Show: true, Label: "within noise", Class: "noise"}
	default: // insufficient
		return Verdict{Show: false}
	}
}

func confidenceLabel(c analyze.SigLevel) string {
	switch c {
	case analyze.SigHigh:
		return "high confidence"
	case analyze.SigMedium:
		return "likely (90%)"
	case analyze.SigNoise:
		return "within noise"
	default:
		return "too few weeks"
	}
}

// axisFromValues builds a scorecard axis whose displayed before/after EXACTLY
// match the corresponding slide's headline (the aggregate numbers), while the
// significance verdict comes from the bootstrap on per-week samples. Direction
// is derived from the displayed values so the arrow can never contradict the
// numbers; it's shown only when the bootstrap clears the noise bar.
func axisFromValues(name, headline string, before, after float64, lowerIsBetter bool, conf analyze.SigLevel, fmtVal func(float64) string) ScorecardAxis {
	abs := after - before
	meaningful := conf == analyze.SigHigh || conf == analyze.SigMedium
	arrow, state := "◼", "flat"
	if meaningful && abs != 0 {
		if abs > 0 {
			arrow = "▲"
		} else {
			arrow = "▼"
		}
		better := (abs > 0) != lowerIsBetter
		state = stateClass(better)
	}
	change := "—"
	if before != 0 {
		denom := before
		if denom < 0 {
			denom = -denom
		}
		change = fmt.Sprintf("%+.0f%%", abs/denom*100)
	}
	return ScorecardAxis{
		Name: name, Headline: headline,
		BeforeText: fmtVal(before), AfterText: fmtVal(after),
		ChangeText: change, Arrow: arrow, State: state,
		Verdict: scoreVerdict(state, conf),
	}
}

// populateSignificance (#3 + #10) bootstraps a confidence verdict for each
// before↔after delta on the windowed cohorts, sets the per-slide Verdict
// chips, and synthesises the three-axis AI Impact Scorecard. Reads
// BugFixHasCutover, so it must run after populateBugFixSummary.
func populateSignificance(d *DeckData, weeks []analyze.WeekStats, defects []analyze.DefectWeek, bugfix []analyze.BugFixWeek, cycles []analyze.WeekCycle, cutover analyze.Cutover) {
	if !cutover.Detected {
		return
	}
	bw, aw := analyze.SplitByCutover(weeks, cutover)
	if len(bw) == 0 || len(aw) == 0 {
		return
	}
	days := func(v float64) string { return fmt.Sprintf("%.1f d", v) }
	pct := func(v float64) string { return fmt.Sprintf("%.1f%%", v) }
	num := func(v float64) string { return fmt.Sprintf("%.1f", v) }

	prodDelta := analyze.BootstrapDelta(analyze.ProductivitySeries(bw), analyze.ProductivitySeries(aw), false)
	d.ProductivityVerdict = verdictFromDelta(prodDelta)

	var revDelta, bugDelta analyze.Delta
	haveQuality := false
	if bd, ad := analyze.SplitDefectsByCutover(defects, cutover); len(bd) > 0 && len(ad) > 0 {
		revDelta = analyze.BootstrapDelta(analyze.RevertSeries(bd), analyze.RevertSeries(ad), true)
		bugDelta = analyze.BootstrapDelta(analyze.BugRateSeries(bd), analyze.BugRateSeries(ad), true)
		d.RevertVerdict = verdictFromDelta(revDelta)
		d.BugRateVerdict = verdictFromDelta(bugDelta)
		haveQuality = true
	}

	var bugfixDelta analyze.Delta
	if d.BugFixHasCutover {
		bb, ab := analyze.SplitBugFixByCutover(bugfix, cutover)
		bugfixDelta = analyze.BootstrapDelta(analyze.BugFixSeries(bb), analyze.BugFixSeries(ab), true)
		d.BugFixVerdict = verdictFromDelta(bugfixDelta)
	}

	var leadDelta analyze.Delta
	haveSpeed := false
	if bc, ac := analyze.SplitCycleByCutover(cycles, cutover); len(bc) > 0 && len(ac) > 0 {
		leadDelta = analyze.BootstrapDelta(analyze.LeadTimeSeries(bc), analyze.LeadTimeSeries(ac), true)
		d.LeadTimeBefore = leadDelta.Before
		d.LeadTimeAfter = leadDelta.After
		d.LeadTimeHasCutover = true
		d.LeadTimeVerdict = verdictFromDelta(leadDelta)
		haveSpeed = true
	}

	// Display the SAME before/after numbers the individual slides show
	// (aggregate where the slide uses aggregate); the bootstrap supplies only
	// the confidence + direction. This keeps the scorecard consistent with
	// every slide it summarises.
	axes := []ScorecardAxis{
		axisFromValues("Output / dev", "commits per active dev",
			d.ProductivityBefore, d.ProductivityAfter, false, prodDelta.Confidence, num),
	}
	if haveSpeed {
		ax := axisFromValues("Lead time", "dev → release",
			d.LeadTimeBefore, d.LeadTimeAfter, true, leadDelta.Confidence, days)
		if d.BugFixHasCutover {
			ax.Sub = fmt.Sprintf("Bug-fix %s→%s (%s)", days(d.BugLeadBefore), days(d.BugLeadAfter), confidenceLabel(bugfixDelta.Confidence))
		}
		axes = append(axes, ax)
	}
	if haveQuality {
		// Bug rate is the headline quality metric (more meaningful than reverts);
		// revert rate rides as the secondary line. Fall back to revert when the
		// tracker has no bug-typed tickets.
		if d.DefectsHasBugs {
			ax := axisFromValues("Bugs & reverts", "bug rate",
				d.BugRateBefore, d.BugRateAfter, true, bugDelta.Confidence, pct)
			ax.Sub = fmt.Sprintf("Revert rate %s→%s (%s)", pct(d.RevertRateBefore), pct(d.RevertRateAfter), confidenceLabel(revDelta.Confidence))
			axes = append(axes, ax)
		} else {
			ax := axisFromValues("Reverts", "revert rate",
				d.RevertRateBefore, d.RevertRateAfter, true, revDelta.Confidence, pct)
			axes = append(axes, ax)
		}
	}
	d.Scorecard = axes
	d.ScorecardAvailable = true

	parts := make([]string, 0, len(axes))
	for _, a := range axes {
		parts = append(parts, fmt.Sprintf("%s %s %s (%s)", a.Name, a.ChangeText, a.Arrow, a.Verdict))
	}
	d.ScorecardSummary = strings.Join(parts, " · ")
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
	// Direct len() in the condition — nilaway can't see the guard through
	// an intermediate `n := len(...)` binding.
	if len(repoAdoption) > 0 {
		last := repoAdoption[len(repoAdoption)-1]
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

	d.DisplayMonths = cfg.Windows.DisplayMonths
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
