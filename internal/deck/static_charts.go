package deck

// static_charts.go — server-side raster + vector exports rendered with
// go-analyze/charts. These exist alongside the interactive ECharts deck so
// the CTO can paste a throughput chart straight into Slack or drop a
// high-resolution SVG into a board doc without screenshotting the browser.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-analyze/charts"

	"github.com/StephanSchmidt/loupe/internal/analyze"
)

// intValueAxis renders the y-axis labels as whole numbers — count charts
// (open issues, repos, commits) otherwise show decimal gridlines like
// "5.000" or "0.1" from the library's automatic tick division.
func intValueAxis() []charts.ValueAxisOption {
	return []charts.ValueAxisOption{{
		ValueFormatter: func(v float64) string { return strconv.FormatInt(int64(math.Round(v)), 10) },
	}}
}

const (
	staticChartWidth  = 1200
	staticChartHeight = 480
)

// staticChartFormats lists the output extensions produced for every chart.
// Adding "jpg" here would extend coverage without further code changes.
var staticChartFormats = []string{"png", "svg"}

// RenderStaticCharts writes throughput, adoption, and (when cycle data
// is available) cycle charts under chartsDir in every format in
// staticChartFormats. PNG is the paste-into-Slack default; SVG is for
// high-resolution embedding.
func RenderStaticCharts(weeks []analyze.WeekStats, cutover analyze.Cutover, cycles []analyze.WeekCycle, repoAdoption []analyze.RepoAdoptionWeek, wip []analyze.WIPWeek, defects []analyze.DefectWeek, bugfix []analyze.BugFixWeek, prcycle []analyze.PRWeek, chartsDir string) error {
	if len(weeks) == 0 {
		return fmt.Errorf("RenderStaticCharts: no weekly data")
	}
	if err := os.MkdirAll(chartsDir, 0o750); err != nil {
		return fmt.Errorf("create charts dir %s: %w", chartsDir, err)
	}

	for _, format := range staticChartFormats {
		thru := filepath.Join(chartsDir, "throughput."+format)
		if err := renderStaticThroughput(weeks, cutover, thru, format); err != nil {
			return fmt.Errorf("throughput %s: %w", format, err)
		}
		adopt := filepath.Join(chartsDir, "adoption."+format)
		if err := renderStaticAdoption(weeks, cutover, adopt, format); err != nil {
			return fmt.Errorf("adoption %s: %w", format, err)
		}
		prod := filepath.Join(chartsDir, "productivity."+format)
		if err := renderStaticProductivity(weeks, cutover, prod, format); err != nil {
			return fmt.Errorf("productivity %s: %w", format, err)
		}
		if len(repoAdoption) > 0 {
			ra := filepath.Join(chartsDir, "repo-adoption."+format)
			if err := renderStaticRepoAdoption(repoAdoption, cutover, ra, format); err != nil {
				return fmt.Errorf("repo adoption %s: %w", format, err)
			}
		}
		if len(wip) > 0 {
			w := filepath.Join(chartsDir, "wip."+format)
			if err := renderStaticWIP(wip, cutover, w, format); err != nil {
				return fmt.Errorf("wip %s: %w", format, err)
			}
		}
		if len(defects) > 0 {
			d := filepath.Join(chartsDir, "defects."+format)
			if err := renderStaticDefects(defects, cutover, d, format); err != nil {
				return fmt.Errorf("defects %s: %w", format, err)
			}
		}
		// Same minBugCycles floor as the interactive slide, so the export and
		// the deck agree on when bug-fix speed has enough data to show.
		if _, _, bugN := analyze.BugFixLeadTimes(bugfix); bugN >= minBugCycles {
			b := filepath.Join(chartsDir, "bugfix."+format)
			if err := renderStaticBugFix(bugfix, cutover, b, format); err != nil {
				return fmt.Errorf("bugfix %s: %w", format, err)
			}
		}
		if _, _, n := analyze.PRCycleSummary(prcycle); n >= minMergedPRs {
			p := filepath.Join(chartsDir, "prcycle."+format)
			if err := renderStaticPRCycle(prcycle, cutover, p, format); err != nil {
				return fmt.Errorf("prcycle %s: %w", format, err)
			}
		}
		if len(cycles) == 0 {
			continue
		}
		cycle := filepath.Join(chartsDir, "cycle."+format)
		if err := renderStaticCycle(cycles, cutover, cycle, format); err != nil {
			return fmt.Errorf("cycle %s: %w", format, err)
		}
	}
	return nil
}

func renderStaticProductivity(weeks []analyze.WeekStats, cutover analyze.Cutover, outPath, format string) error {
	labels, _ := buildStaticLabels(weeks, cutover)
	human := make([]float64, len(weeks))
	ai := make([]float64, len(weeks))
	for i, w := range weeks {
		if w.DistinctAuthors > 0 {
			d := float64(w.DistinctAuthors)
			human[i] = float64(w.TotalCommits-w.AICommits) / d
			ai[i] = float64(w.AICommits) / d
		}
	}

	opt := charts.NewBarChartOptionWithData([][]float64{human, ai})
	opt.StackSeries = charts.Ptr(true)
	opt.Title = charts.TitleOption{Text: "Output per active developer — commits/dev per week"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("AI adoption cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
		LabelCount:    staticLabelCount(len(labels)),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"Human", "AI-tagged"}}

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.BarChart(opt); err != nil {
		return fmt.Errorf("bar chart: %w", err)
	}
	return writeStaticChart(p, outPath)
}

func renderStaticDefects(rows []analyze.DefectWeek, cutover analyze.Cutover, outPath, format string) error {
	labels := make([]string, len(rows))
	revert := make([]float64, len(rows))
	bug := make([]float64, len(rows))
	hasBugs := false
	prevYear := 0
	cutoverIdx := -1
	for i, r := range rows {
		layout := "Jan 02"
		if r.WeekStart.Year() != prevYear {
			layout = "Jan 02 2006"
		}
		labels[i] = r.WeekStart.Format(layout)
		prevYear = r.WeekStart.Year()
		if cutover.Detected && r.WeekStart.Equal(cutover.Date) {
			cutoverIdx = i
		}
		revert[i] = r.RevertRate()
		bug[i] = r.BugRate()
		if r.Tickets > 0 {
			hasBugs = true
		}
	}
	if cutoverIdx >= 0 {
		labels[cutoverIdx] = "▼ " + labels[cutoverIdx]
	}

	series := [][]float64{revert}
	names := []string{"Revert rate %"}
	if hasBugs {
		series = append(series, bug)
		names = append(names, "Bug rate %")
	}
	// Grouped bars (StackSeries left unset) — the two rates use different
	// denominators, so stacking would be meaningless.
	opt := charts.NewBarChartOptionWithData(series)
	opt.Title = charts.TitleOption{Text: "Quality counterweight — revert & bug rate"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("AI adoption cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
		LabelCount:    staticLabelCount(len(labels)),
	}
	opt.Legend = charts.LegendOption{SeriesNames: names}

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.BarChart(opt); err != nil {
		return fmt.Errorf("bar chart: %w", err)
	}
	return writeStaticChart(p, outPath)
}

func renderStaticBugFix(rows []analyze.BugFixWeek, cutover analyze.Cutover, outPath, format string) error {
	labels := make([]string, len(rows))
	bug := make([]float64, len(rows))
	other := make([]float64, len(rows))
	prevYear := 0
	cutoverIdx := -1
	for i, r := range rows {
		layout := "Jan 02"
		if r.WeekStart.Year() != prevYear {
			layout = "Jan 02 2006"
		}
		labels[i] = r.WeekStart.Format(layout)
		prevYear = r.WeekStart.Year()
		if cutover.Detected && r.WeekStart.Equal(cutover.Date) {
			cutoverIdx = i
		}
		bug[i] = r.BugMedianLead.Hours() / 24
		other[i] = r.OtherMedianLead.Hours() / 24
	}
	if cutoverIdx >= 0 {
		labels[cutoverIdx] = "▼ " + labels[cutoverIdx]
	}

	// Grouped bars (StackSeries left unset) — bug and non-bug are separate
	// cohorts, not parts of a whole.
	opt := charts.NewBarChartOptionWithData([][]float64{bug, other})
	opt.Title = charts.TitleOption{Text: "Bug-fix speed — median engineering days"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("AI adoption cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
		LabelCount:    staticLabelCount(len(labels)),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"Bug", "Other"}}

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.BarChart(opt); err != nil {
		return fmt.Errorf("bar chart: %w", err)
	}
	return writeStaticChart(p, outPath)
}

func renderStaticPRCycle(rows []analyze.PRWeek, cutover analyze.Cutover, outPath, format string) error {
	labels := make([]string, len(rows))
	days := make([]float64, len(rows))
	prevYear := 0
	cutoverIdx := -1
	for i, r := range rows {
		layout := "Jan 02"
		if r.WeekStart.Year() != prevYear {
			layout = "Jan 02 2006"
		}
		labels[i] = r.WeekStart.Format(layout)
		prevYear = r.WeekStart.Year()
		if cutover.Detected && r.WeekStart.Equal(cutover.Date) {
			cutoverIdx = i
		}
		days[i] = r.MedianToMerge.Hours() / 24
	}
	if cutoverIdx >= 0 {
		labels[cutoverIdx] = "▼ " + labels[cutoverIdx]
	}

	opt := charts.NewBarChartOptionWithData([][]float64{days})
	opt.Title = charts.TitleOption{Text: "PR cycle velocity — median days to merge"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("AI adoption cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
		LabelCount:    staticLabelCount(len(labels)),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"Median days to merge"}}

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.BarChart(opt); err != nil {
		return fmt.Errorf("bar chart: %w", err)
	}
	return writeStaticChart(p, outPath)
}

func renderStaticWIP(rows []analyze.WIPWeek, cutover analyze.Cutover, outPath, format string) error {
	labels := make([]string, len(rows))
	inProg := make([]float64, len(rows))
	notStarted := make([]float64, len(rows))
	prevYear := 0
	cutoverIdx := -1
	for i, r := range rows {
		layout := "Jan 02"
		if r.WeekStart.Year() != prevYear {
			layout = "Jan 02 2006"
		}
		labels[i] = r.WeekStart.Format(layout)
		prevYear = r.WeekStart.Year()
		if cutover.Detected && r.WeekStart.Equal(cutover.Date) {
			cutoverIdx = i
		}
		inProg[i] = float64(r.InProgress)
		notStarted[i] = float64(r.NotStarted)
	}
	if cutoverIdx >= 0 {
		labels[cutoverIdx] = "▼ " + labels[cutoverIdx]
	}

	opt := charts.NewBarChartOptionWithData([][]float64{inProg, notStarted})
	opt.StackSeries = charts.Ptr(true)
	opt.Title = charts.TitleOption{Text: "Work in progress — open issues"}
	opt.Title.Subtext = "Snapshot at end of each ISO week"
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
		LabelCount:    staticLabelCount(len(labels)),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"In Progress", "Not Started"}}
	opt.ValueAxis = intValueAxis()

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.BarChart(opt); err != nil {
		return fmt.Errorf("bar chart: %w", err)
	}
	return writeStaticChart(p, outPath)
}

func renderStaticRepoAdoption(rows []analyze.RepoAdoptionWeek, cutover analyze.Cutover, outPath, format string) error {
	labels := make([]string, len(rows))
	ai := make([]float64, len(rows))
	active := make([]float64, len(rows))
	inactive := make([]float64, len(rows))
	prevYear := 0
	cutoverIdx := -1
	for i, r := range rows {
		layout := "Jan 02"
		if r.WeekStart.Year() != prevYear {
			layout = "Jan 02 2006"
		}
		labels[i] = r.WeekStart.Format(layout)
		prevYear = r.WeekStart.Year()
		if cutover.Detected && r.WeekStart.Equal(cutover.Date) {
			cutoverIdx = i
		}
		ai[i] = float64(r.AIEnabled)
		active[i] = float64(r.Active)
		inactive[i] = float64(r.Inactive)
	}
	if cutoverIdx >= 0 {
		labels[cutoverIdx] = "▼ " + labels[cutoverIdx]
	}

	opt := charts.NewBarChartOptionWithData([][]float64{ai, active, inactive})
	opt.StackSeries = charts.Ptr(true)
	opt.Title = charts.TitleOption{Text: "AI-enabled repos"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("AI adoption cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
		LabelCount:    staticLabelCount(len(labels)),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"AI-enabled", "Active (≤90d)", "Inactive (>90d)"}}
	opt.ValueAxis = intValueAxis()

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.BarChart(opt); err != nil {
		return fmt.Errorf("bar chart: %w", err)
	}
	return writeStaticChart(p, outPath)
}

func renderStaticThroughput(weeks []analyze.WeekStats, cutover analyze.Cutover, outPath, format string) error {
	labels, _ := buildStaticLabels(weeks, cutover)
	human := make([]float64, len(weeks))
	ai := make([]float64, len(weeks))
	for i, w := range weeks {
		human[i] = float64(w.TotalCommits - w.AICommits)
		ai[i] = float64(w.AICommits)
	}

	opt := charts.NewBarChartOptionWithData([][]float64{human, ai})
	opt.StackSeries = charts.Ptr(true)
	opt.Title = charts.TitleOption{Text: "Weekly commits"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("AI adoption cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
		LabelCount:    staticLabelCount(len(labels)),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"Human", "AI-tagged"}}
	opt.ValueAxis = intValueAxis()

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.BarChart(opt); err != nil {
		return fmt.Errorf("bar chart: %w", err)
	}
	return writeStaticChart(p, outPath)
}

func renderStaticAdoption(weeks []analyze.WeekStats, cutover analyze.Cutover, outPath, format string) error {
	labels, _ := buildStaticLabels(weeks, cutover)
	human := make([]float64, len(weeks))
	ai := make([]float64, len(weeks))
	for i, w := range weeks {
		if w.TotalCommits > 0 {
			t := float64(w.TotalCommits)
			human[i] = float64(w.TotalCommits-w.AICommits) / t * 100
			ai[i] = float64(w.AICommits) / t * 100
		}
	}

	// AI at the bottom of the stack (anchored at 0), Human on top — mirrors the
	// interactive adoption chart so the AI share reads from the baseline.
	opt := charts.NewBarChartOptionWithData([][]float64{ai, human})
	opt.StackSeries = charts.Ptr(true)
	opt.Title = charts.TitleOption{Text: "AI adoption — % of weekly commits"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("Cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
		LabelCount:    staticLabelCount(len(labels)),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"AI-tagged", "Human"}}

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.BarChart(opt); err != nil {
		return fmt.Errorf("bar chart: %w", err)
	}
	return writeStaticChart(p, outPath)
}

func renderStaticCycle(cycles []analyze.WeekCycle, cutover analyze.Cutover, outPath, format string) error {
	labels := make([]string, len(cycles))
	idea := make([]float64, len(cycles))
	dev := make([]float64, len(cycles))
	cutoverIdx := -1
	prevYear := 0
	for i, c := range cycles {
		layout := "Jan 02"
		if c.WeekStart.Year() != prevYear {
			layout = "Jan 02 2006"
		}
		labels[i] = c.WeekStart.Format(layout)
		prevYear = c.WeekStart.Year()
		idea[i] = c.MedianIdeaToDev.Hours() / 24
		dev[i] = c.MedianDevToRelease.Hours() / 24
		if cutover.Detected && c.WeekStart.Equal(cutover.Date) {
			cutoverIdx = i
		}
	}
	if cutoverIdx >= 0 {
		labels[cutoverIdx] = "▼ " + labels[cutoverIdx]
	}

	opt := charts.NewBarChartOptionWithData([][]float64{dev, idea})
	opt.StackSeries = charts.Ptr(true)
	opt.Title = charts.TitleOption{Text: "Lead time — median days per ISO week"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("AI adoption cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
		LabelCount:    staticLabelCount(len(labels)),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"Cycle (dev start → last commit)", "Wait (Create → dev start)"}}

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.BarChart(opt); err != nil {
		return fmt.Errorf("bar chart: %w", err)
	}
	return writeStaticChart(p, outPath)
}

// buildStaticLabels returns one axis label per week plus the cutover index
// (or -1). The cutover label is prefixed with "▼ " so it stands out in lieu
// of a vertical-line marker — go-analyze/charts has no first-class
// markLine on category axes.
func buildStaticLabels(weeks []analyze.WeekStats, cutover analyze.Cutover) ([]string, int) {
	labels := make([]string, len(weeks))
	cutoverIdx := -1
	prevYear := 0
	for i, w := range weeks {
		layout := "Jan 02"
		if w.WeekStart.Year() != prevYear {
			layout = "Jan 02 2006"
		}
		labels[i] = w.WeekStart.Format(layout)
		prevYear = w.WeekStart.Year()
		if cutover.Detected && w.WeekStart.Equal(cutover.Date) {
			cutoverIdx = i
		}
	}
	if cutoverIdx >= 0 {
		labels[cutoverIdx] = "▼ " + labels[cutoverIdx]
	}
	return labels, cutoverIdx
}

func writeStaticChart(p *charts.Painter, outPath string) error {
	buf, err := p.Bytes()
	if err != nil {
		return fmt.Errorf("painter.Bytes: %w", err)
	}
	if err := os.WriteFile(outPath, buf, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	return nil
}
