package deck

// static_charts.go — server-side raster + vector exports rendered with
// go-analyze/charts. These exist alongside the interactive ECharts deck so
// the CTO can paste a throughput chart straight into Slack or drop a
// high-resolution SVG into a board doc without screenshotting the browser.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-analyze/charts"

	"github.com/StephanSchmidt/loupe/internal/analyze"
)

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
func RenderStaticCharts(weeks []analyze.WeekStats, cutover analyze.Cutover, cycles []analyze.WeekCycle, chartsDir string) error {
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

func renderStaticAdoption(weeks []analyze.WeekStats, cutover analyze.Cutover, outPath, format string) error {
	labels, _ := buildStaticLabels(weeks, cutover)
	pct := make([]float64, len(weeks))
	for i, w := range weeks {
		pct[i] = w.AdoptionRatio() * 100
	}

	opt := charts.NewLineChartOptionWithData([][]float64{pct})
	opt.Title = charts.TitleOption{Text: "AI adoption — % of weekly active devs"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("Cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.XAxis = charts.XAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"AI-using devs %"}}

	p := charts.NewPainter(charts.PainterOptions{
		Width:        staticChartWidth,
		Height:       staticChartHeight,
		OutputFormat: format,
	})
	if err := p.LineChart(opt); err != nil {
		return fmt.Errorf("line chart: %w", err)
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
	opt.Title = charts.TitleOption{Text: "Cycle time per week (median days)"}
	if cutover.Detected {
		opt.Title.Subtext = fmt.Sprintf("AI adoption cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
	}
	opt.CategoryAxis = charts.CategoryAxisOption{
		Labels:        labels,
		LabelRotation: charts.DegreesToRadians(45),
	}
	opt.Legend = charts.LegendOption{SeriesNames: []string{"Dev → Release", "Idea → Dev"}}

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
