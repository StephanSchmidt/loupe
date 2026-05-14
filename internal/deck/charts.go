package deck

import (
	"encoding/json"
	"fmt"
	"html/template"

	"github.com/StephanSchmidt/loupe/internal/analyze"
)

// ChartPayload holds JSON-encoded Apache ECharts option objects for the
// charts on the deck. Each option field is marked template.JS so the
// HTML template can embed it verbatim inside a <script> block — JSON
// object literals are valid JS expressions.
//
// The CutoverIdx fields are not part of the ECharts option — they're
// passed alongside so the template's JS can draw the cutover marker as
// a graphic overlay (positioned via convertToPixel at the category
// boundary, not snapped to a bar center as markLine would be).
type ChartPayload struct {
	ThroughputJSON       template.JS
	AdoptionJSON         template.JS
	ThroughputCutoverIdx int // -1 if no cutover detected
	AdoptionCutoverIdx   int
}

// Dark-theme palette — kept in sync with template.html.tmpl's CSS vars.
// Centralised here so chart styling matches the slide chrome.
const (
	chartBG        = "transparent"
	chartFG        = "#e5e7eb"
	chartMuted     = "#9ca3af"
	chartGridLine  = "#1f2937"
	chartAxisLine  = "#374151"
	chartTooltipBG = "#11172a"
	chartAccent    = "#f59e0b" // amber — AI-tagged + cutover marker
	chartAccent2   = "#3b82f6" // blue   — human commits
	chartDeckBG    = "#0b0f17" // slide background, used as label foreground on accent pill
	chartFontStack = "-apple-system, BlinkMacSystemFont, Segoe UI, Roboto, Helvetica Neue, Arial, sans-serif"
)

// BuildChartPayload prepares the ECharts option payloads for the deck. The
// rendered template hands these to echarts.init().setOption() in the
// browser. The Go side does no PNG rasterisation.
func BuildChartPayload(weeks []analyze.WeekStats, cutover analyze.Cutover) (ChartPayload, error) {
	if len(weeks) == 0 {
		return ChartPayload{}, fmt.Errorf("BuildChartPayload: no weekly data")
	}
	_, cutoverIdx := axisLabelsAndCutover(weeks, cutover)
	thru, err := marshalOption(buildThroughputOption(weeks, cutover))
	if err != nil {
		return ChartPayload{}, fmt.Errorf("marshal throughput option: %w", err)
	}
	adopt, err := marshalOption(buildAdoptionOption(weeks, cutover))
	if err != nil {
		return ChartPayload{}, fmt.Errorf("marshal adoption option: %w", err)
	}
	// json.Marshal escapes <, >, & as \u-sequences, so the payload cannot
	// break out of the enclosing <script>. No untrusted JS lives in either
	// option map (only numbers, ECharts keywords, and time-formatted
	// strings), so the template.JS conversion is safe here.
	return ChartPayload{
		ThroughputJSON:       template.JS(thru),  // #nosec G203 -- JSON-encoded payload, see comment above
		AdoptionJSON:         template.JS(adopt), // #nosec G203 -- JSON-encoded payload, see comment above
		ThroughputCutoverIdx: cutoverIdx,
		AdoptionCutoverIdx:   cutoverIdx,
	}, nil
}

func marshalOption(opt map[string]any) ([]byte, error) {
	return json.Marshal(opt)
}

// buildThroughputOption produces the ECharts option for the stacked
// weekly commit bar chart (human vs AI-tagged). The cutover marker is
// drawn as a JS graphic overlay (see overlayCutover in the template)
// rather than an ECharts markLine — markLine on a category axis snaps
// to integer indices, which would force the line through a bar centre.
func buildThroughputOption(weeks []analyze.WeekStats, cutover analyze.Cutover) map[string]any {
	labels, _ := axisLabelsAndCutover(weeks, cutover)
	human := make([]int, len(weeks))
	ai := make([]int, len(weeks))
	for i, w := range weeks {
		human[i] = w.TotalCommits - w.AICommits
		ai[i] = w.AICommits
	}

	humanSeries := map[string]any{
		"name": "Human", "type": "bar", "stack": "total", "data": human,
		"itemStyle": map[string]any{"borderRadius": []int{0, 0, 0, 0}},
	}
	aiSeries := map[string]any{
		"name": "AI-tagged", "type": "bar", "stack": "total", "data": ai,
		"itemStyle": map[string]any{"borderRadius": []int{3, 3, 0, 0}},
	}

	opt := darkChartBase("Weekly commits", cutover)
	opt["color"] = []string{chartAccent2, chartAccent}
	opt["legend"] = darkLegend([]string{"Human", "AI-tagged"})
	opt["tooltip"] = darkTooltip(map[string]any{"type": "shadow"})
	opt["xAxis"] = darkCategoryAxis(labels)
	opt["yAxis"] = darkValueAxis(nil)
	opt["series"] = []map[string]any{humanSeries, aiSeries}
	return opt
}

// buildAdoptionOption produces the ECharts option for the AI-author
// adoption line chart (percentage of weekly active devs with at least
// one AI-tagged commit).
func buildAdoptionOption(weeks []analyze.WeekStats, cutover analyze.Cutover) map[string]any {
	labels, _ := axisLabelsAndCutover(weeks, cutover)
	pct := make([]float64, len(weeks))
	for i, w := range weeks {
		pct[i] = w.AdoptionRatio() * 100
	}

	series := map[string]any{
		"name":       "AI-using devs %",
		"type":       "line",
		"smooth":     true,
		"data":       pct,
		"symbol":     "circle",
		"symbolSize": 8,
		"lineStyle":  map[string]any{"width": 3},
		"areaStyle":  map[string]any{"opacity": 0.18},
	}

	opt := darkChartBase("AI adoption — % of weekly active devs", cutover)
	opt["color"] = []string{chartAccent}
	opt["legend"] = darkLegend([]string{"AI-using devs %"})
	opt["tooltip"] = darkTooltip(nil)
	opt["xAxis"] = darkCategoryAxis(labels)
	opt["yAxis"] = darkValueAxis(map[string]any{
		"min":       0,
		"max":       100,
		"axisLabel": map[string]any{"formatter": "{value}%", "color": chartMuted},
	})
	opt["series"] = []map[string]any{series}
	return opt
}

// axisLabelsAndCutover returns one axis label per week plus the index of
// the cutover week (-1 if no cutover detected).
func axisLabelsAndCutover(weeks []analyze.WeekStats, cutover analyze.Cutover) ([]string, int) {
	labels := make([]string, len(weeks))
	cutoverIdx := -1
	for i, w := range weeks {
		labels[i] = w.WeekStart.Format("Jan 02")
		if cutover.Detected && w.WeekStart.Equal(cutover.Date) {
			cutoverIdx = i
		}
	}
	return labels, cutoverIdx
}

// darkChartBase contains the option keys common to every chart on the
// deck: backgrounds, font, title with the cutover subtext, and grid
// padding. Callers add color, legend, tooltip, axes, series.
func darkChartBase(title string, cutover analyze.Cutover) map[string]any {
	return map[string]any{
		"backgroundColor": chartBG,
		"textStyle": map[string]any{
			"color":      chartFG,
			"fontFamily": chartFontStack,
		},
		"title": titleWithCutover(title, cutover),
		"grid": map[string]any{
			"left":         60,
			"right":        30,
			"top":          90,
			"bottom":       70,
			"containLabel": true,
		},
	}
}

func darkLegend(names []string) map[string]any {
	return map[string]any{
		"data":       names,
		"top":        54,
		"textStyle":  map[string]any{"color": "#d1d5db"},
		"icon":       "roundRect",
		"itemWidth":  14,
		"itemHeight": 8,
	}
}

func darkTooltip(axisPointer map[string]any) map[string]any {
	tt := map[string]any{
		"trigger":         "axis",
		"backgroundColor": chartTooltipBG,
		"borderColor":     chartGridLine,
		"borderWidth":     1,
		"textStyle":       map[string]any{"color": chartFG},
	}
	if axisPointer != nil {
		tt["axisPointer"] = axisPointer
	}
	return tt
}

func darkCategoryAxis(labels []string) map[string]any {
	return map[string]any{
		"type":      "category",
		"data":      labels,
		"axisLabel": map[string]any{"rotate": 45, "interval": 0, "color": chartMuted},
		"axisLine":  map[string]any{"lineStyle": map[string]any{"color": chartAxisLine}},
		"axisTick":  map[string]any{"lineStyle": map[string]any{"color": chartAxisLine}},
	}
}

// darkValueAxis returns a y-axis option. The extra map's keys override
// the defaults so callers can set min/max/axisLabel.formatter.
func darkValueAxis(extra map[string]any) map[string]any {
	a := map[string]any{
		"type":      "value",
		"axisLabel": map[string]any{"color": chartMuted},
		"axisLine":  map[string]any{"show": false},
		"splitLine": map[string]any{"lineStyle": map[string]any{"color": chartGridLine}},
	}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

func titleWithCutover(text string, cutover analyze.Cutover) map[string]any {
	title := map[string]any{
		"text":      text,
		"left":      "center",
		"top":       8,
		"textStyle": map[string]any{"color": chartFG, "fontSize": 20, "fontWeight": "bold"},
	}
	if cutover.Detected {
		title["subtext"] = fmt.Sprintf("AI adoption cutover: %s (%s)",
			cutover.Date.Format("Jan 2, 2006"), cutover.Reason)
		title["subtextStyle"] = map[string]any{"color": chartMuted, "fontSize": 13}
	}
	return title
}
