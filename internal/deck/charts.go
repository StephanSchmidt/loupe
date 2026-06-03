package deck

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strings"

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
	ThroughputJSON         template.JS
	AdoptionJSON           template.JS
	CycleJSON              template.JS // empty when no ticket cycle data
	HasCycle               bool
	RepoAdoptionJSON       template.JS // empty when no repo data
	HasRepoAdoption        bool
	WIPJSON                template.JS // empty when no ticket data
	HasWIP                 bool
	DefectsJSON            template.JS // empty when no commit data
	HasDefects             bool
	BugFixJSON             template.JS // empty when too few bug-class tickets
	HasBugFix              bool
	ProductivityJSON       template.JS // commits-per-active-dev; always present
	ThroughputCutoverIdx   int         // -1 if no cutover detected
	AdoptionCutoverIdx     int
	ProductivityCutoverIdx int
	CycleCutoverIdx        int
	RepoAdoptionCutoverIdx int
	WIPCutoverIdx          int
	DefectsCutoverIdx      int
	BugFixCutoverIdx       int
	// Focus holds per-project scoped copies of the charts above, rendered as
	// extra slides after each org-wide chart. Empty unless `focus` is set.
	Focus []FocusChartPayload
}

// FocusChartPayload is one named project lens: scoped ECharts options for
// each chart, identified by a URL-safe Slug used in the slide's data-chart id.
type FocusChartPayload struct {
	Name string
	Slug string

	ThroughputJSON   template.JS
	AdoptionJSON     template.JS
	ProductivityJSON template.JS
	CycleJSON        template.JS
	HasCycle         bool
	WIPJSON          template.JS
	HasWIP           bool
	DefectsJSON      template.JS
	HasDefects       bool
	BugFixJSON       template.JS
	HasBugFix        bool
	RepoAdoptionJSON template.JS
	HasRepoAdoption  bool

	ThroughputCutoverIdx   int
	AdoptionCutoverIdx     int
	ProductivityCutoverIdx int
	CycleCutoverIdx        int
	WIPCutoverIdx          int
	DefectsCutoverIdx      int
	BugFixCutoverIdx       int
	RepoAdoptionCutoverIdx int
}

// FocusData is one lens's scoped weekly series, computed by the caller and
// turned into a FocusChartPayload here.
type FocusData struct {
	Name         string
	Weeks        []analyze.WeekStats
	Cycles       []analyze.WeekCycle
	WIP          []analyze.WIPWeek
	Defects      []analyze.DefectWeek
	BugFix       []analyze.BugFixWeek
	RepoAdoption []analyze.RepoAdoptionWeek
}

// focusSlug turns a focus name into a URL-/id-safe token.
func focusSlug(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// Dark-theme palette — kept in sync with template.html.tmpl's CSS vars.
// Centralised here so chart styling matches the slide chrome.
const (
	chartBG         = "transparent"
	chartFG         = "#e5e7eb"
	chartMuted      = "#9ca3af"
	chartGridLine   = "#1f2937"
	chartAxisLine   = "#374151"
	chartTooltipBG  = "#11172a"
	chartAccent     = "#f59e0b" // amber — AI-tagged (high confidence) + cutover marker
	chartAccentSoft = "#fcd34d" // softer amber — inferred AI (medium confidence)
	chartAccent2    = "#3b82f6" // blue — human commits / AI-enabled repos
	chartRepoActive = "#93c5fd" // light blue — repos active in the last 90 days
	chartRepoInactive = "#6b7280" // gray — repos inactive >90 days
	chartLeadWait   = "#22c55e" // green — lead-time wait (Create → In Progress)
	chartWipActive  = "#c2410c" // burnt orange — issues in progress
	chartWipPending = "#fdba74" // light orange — issues not started
	chartDefectRevert = "#ef4444" // red — revert rate
	chartDefectBug    = "#f59e0b" // amber — bug rate
	chartDeckBG     = "#0b0f17" // slide background, used as label foreground on accent pill
	chartFontStack  = "-apple-system, BlinkMacSystemFont, Segoe UI, Roboto, Helvetica Neue, Arial, sans-serif"
)

// BuildChartPayload prepares the ECharts option payloads for the deck. The
// rendered template hands these to echarts.init().setOption() in the
// browser. The Go side does no PNG rasterisation.
func BuildChartPayload(weeks []analyze.WeekStats, cutover analyze.Cutover, cycles []analyze.WeekCycle, repoAdoption []analyze.RepoAdoptionWeek, wip []analyze.WIPWeek, defects []analyze.DefectWeek, bugfix []analyze.BugFixWeek, focus []FocusData) (ChartPayload, error) {
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
	prod, err := marshalOption(buildProductivityOption(weeks, cutover))
	if err != nil {
		return ChartPayload{}, fmt.Errorf("marshal productivity option: %w", err)
	}
	// json.Marshal escapes <, >, & as \u-sequences, so the payload cannot
	// break out of the enclosing <script>. No untrusted JS lives in either
	// option map (only numbers, ECharts keywords, and time-formatted
	// strings), so the template.JS conversion is safe here.
	out := ChartPayload{
		ThroughputJSON:         template.JS(thru),  // #nosec G203 -- JSON-encoded payload, see comment above
		AdoptionJSON:           template.JS(adopt), // #nosec G203 -- JSON-encoded payload, see comment above
		ProductivityJSON:       template.JS(prod),  // #nosec G203 -- JSON-encoded payload, see comment above
		ThroughputCutoverIdx:   cutoverIdx,
		AdoptionCutoverIdx:     cutoverIdx,
		ProductivityCutoverIdx: cutoverIdx,
		CycleCutoverIdx:        -1,
		RepoAdoptionCutoverIdx: -1,
		WIPCutoverIdx:          -1,
		DefectsCutoverIdx:      -1,
		BugFixCutoverIdx:       -1,
	}
	if len(cycles) > 0 {
		cycle, idx, err := marshalCycleOption(cycles, cutover)
		if err != nil {
			return ChartPayload{}, fmt.Errorf("marshal cycle option: %w", err)
		}
		out.CycleJSON = template.JS(cycle) // #nosec G203 -- JSON-encoded payload
		out.CycleCutoverIdx = idx
		out.HasCycle = true
	}
	if len(repoAdoption) > 0 {
		ra, idx, err := marshalRepoAdoptionOption(repoAdoption, cutover)
		if err != nil {
			return ChartPayload{}, fmt.Errorf("marshal repo adoption option: %w", err)
		}
		out.RepoAdoptionJSON = template.JS(ra) // #nosec G203 -- JSON-encoded payload
		out.RepoAdoptionCutoverIdx = idx
		out.HasRepoAdoption = true
	}
	if len(wip) > 0 {
		w, idx, err := marshalWIPOption(wip, cutover)
		if err != nil {
			return ChartPayload{}, fmt.Errorf("marshal WIP option: %w", err)
		}
		out.WIPJSON = template.JS(w) // #nosec G203 -- JSON-encoded payload
		out.WIPCutoverIdx = idx
		out.HasWIP = true
	}
	if len(defects) > 0 {
		d, idx, err := marshalDefectsOption(defects, cutover)
		if err != nil {
			return ChartPayload{}, fmt.Errorf("marshal defects option: %w", err)
		}
		out.DefectsJSON = template.JS(d) // #nosec G203 -- JSON-encoded payload
		out.DefectsCutoverIdx = idx
		out.HasDefects = true
	}
	// Bug-fix speed self-suppresses when too few tickets classify as bugs —
	// this is what hides the slide on trackers that don't supply a bug-shaped
	// type (Linear, GitLab) instead of rendering a misleading all-"Other" chart.
	if _, _, bugN := analyze.BugFixLeadTimes(bugfix); bugN >= minBugCycles {
		b, idx, err := marshalBugFixOption(bugfix, cutover)
		if err != nil {
			return ChartPayload{}, fmt.Errorf("marshal bug-fix option: %w", err)
		}
		out.BugFixJSON = template.JS(b) // #nosec G203 -- JSON-encoded payload
		out.BugFixCutoverIdx = idx
		out.HasBugFix = true
	}
	for _, f := range focus {
		fp, err := buildFocusPayload(f, cutover)
		if err != nil {
			return ChartPayload{}, err
		}
		out.Focus = append(out.Focus, fp)
	}
	return out, nil
}

// buildFocusPayload marshals one lens's scoped charts, reusing the org-wide
// chart builders against the lens's filtered series and the org cutover.
func buildFocusPayload(f FocusData, cutover analyze.Cutover) (FocusChartPayload, error) {
	fp := FocusChartPayload{
		Name: f.Name, Slug: focusSlug(f.Name),
		ThroughputCutoverIdx: -1, AdoptionCutoverIdx: -1, CycleCutoverIdx: -1,
		WIPCutoverIdx: -1, DefectsCutoverIdx: -1, BugFixCutoverIdx: -1, RepoAdoptionCutoverIdx: -1,
	}
	if len(f.Weeks) > 0 {
		_, idx := axisLabelsAndCutover(f.Weeks, cutover)
		thru, err := marshalOption(buildThroughputOption(f.Weeks, cutover))
		if err != nil {
			return fp, fmt.Errorf("focus %q throughput: %w", f.Name, err)
		}
		adopt, err := marshalOption(buildAdoptionOption(f.Weeks, cutover))
		if err != nil {
			return fp, fmt.Errorf("focus %q adoption: %w", f.Name, err)
		}
		prod, err := marshalOption(buildProductivityOption(f.Weeks, cutover))
		if err != nil {
			return fp, fmt.Errorf("focus %q productivity: %w", f.Name, err)
		}
		fp.ThroughputJSON = template.JS(thru)   // #nosec G203
		fp.AdoptionJSON = template.JS(adopt)     // #nosec G203
		fp.ProductivityJSON = template.JS(prod)  // #nosec G203
		fp.ThroughputCutoverIdx = idx
		fp.AdoptionCutoverIdx = idx
		fp.ProductivityCutoverIdx = idx
	}
	// "AI-enabled repos" is a portfolio metric — a count of repos per week.
	// Scoped to a focus's handful of repos it's meaningless, so it's
	// deliberately omitted from focus slides (org-wide only).
	if len(f.WIP) > 0 {
		w, idx, err := marshalWIPOption(f.WIP, cutover)
		if err != nil {
			return fp, fmt.Errorf("focus %q wip: %w", f.Name, err)
		}
		fp.WIPJSON = template.JS(w) // #nosec G203
		fp.WIPCutoverIdx = idx
		fp.HasWIP = true
	}
	if len(f.Defects) > 0 {
		d, idx, err := marshalDefectsOption(f.Defects, cutover)
		if err != nil {
			return fp, fmt.Errorf("focus %q defects: %w", f.Name, err)
		}
		fp.DefectsJSON = template.JS(d) // #nosec G203
		fp.DefectsCutoverIdx = idx
		fp.HasDefects = true
	}
	if _, _, bugN := analyze.BugFixLeadTimes(f.BugFix); bugN >= minBugCycles {
		b, idx, err := marshalBugFixOption(f.BugFix, cutover)
		if err != nil {
			return fp, fmt.Errorf("focus %q bug-fix: %w", f.Name, err)
		}
		fp.BugFixJSON = template.JS(b) // #nosec G203
		fp.BugFixCutoverIdx = idx
		fp.HasBugFix = true
	}
	if len(f.Cycles) > 0 {
		c, idx, err := marshalCycleOption(f.Cycles, cutover)
		if err != nil {
			return fp, fmt.Errorf("focus %q cycle: %w", f.Name, err)
		}
		fp.CycleJSON = template.JS(c) // #nosec G203
		fp.CycleCutoverIdx = idx
		fp.HasCycle = true
	}
	return fp, nil
}

// buildDefectsOption produces the ECharts option for the quality
// counterweight: weekly revert rate (%) and, when ticket data exists, bug
// rate (%) — both as lines on a shared percentage axis.
func buildDefectsOption(rows []analyze.DefectWeek, cutover analyze.Cutover) (map[string]any, int) {
	labels := make([]string, len(rows))
	revert := make([]float64, len(rows))
	bug := make([]float64, len(rows))
	hasBugs := false
	cutoverIdx := -1
	prevYear := 0
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
		revert[i] = roundTo1(r.RevertRate())
		bug[i] = roundTo1(r.BugRate())
		if r.Tickets > 0 {
			hasBugs = true
		}
	}

	// Grouped (not stacked) bars: the two rates have different denominators
	// (commits vs tickets), so a stacked total would be meaningless.
	bar := func(name string, data []float64) map[string]any {
		return map[string]any{
			"name": name, "type": "bar", "data": data,
			"itemStyle": map[string]any{"borderRadius": []int{3, 3, 0, 0}},
		}
	}

	opt := darkChartBase("Quality counterweight — revert & bug rate", cutover)
	opt["tooltip"] = darkTooltip(map[string]any{"type": "shadow"})
	opt["xAxis"] = darkCategoryAxis(labels)
	opt["yAxis"] = darkValueAxis(map[string]any{
		"min":       0,
		"axisLabel": map[string]any{"formatter": "{value}%", "color": chartMuted},
	})
	if hasBugs {
		opt["color"] = []string{chartDefectRevert, chartDefectBug}
		opt["legend"] = darkLegend([]string{"Revert rate", "Bug rate"})
		opt["series"] = []map[string]any{bar("Revert rate", revert), bar("Bug rate", bug)}
	} else {
		opt["color"] = []string{chartDefectRevert}
		opt["legend"] = darkLegend([]string{"Revert rate"})
		opt["series"] = []map[string]any{bar("Revert rate", revert)}
	}
	return opt, cutoverIdx
}

func marshalDefectsOption(rows []analyze.DefectWeek, cutover analyze.Cutover) ([]byte, int, error) {
	opt, idx := buildDefectsOption(rows, cutover)
	b, err := marshalOption(opt)
	return b, idx, err
}

// minBugCycles is the floor of bug-class tickets (with linked commits) below
// which the bug-fix-speed slide is suppressed — too few to draw a meaningful
// median trend, and the mechanism that hides the slide for trackers that
// don't populate a bug-shaped ticket type.
const minBugCycles = 15

// buildBugFixOption produces the ECharts option for bug-fix speed: median
// engineering time (dev start → last linked commit), in days, split into
// bug-class tickets vs everything else. Grouped (not stacked) bars — the two
// are separate cohorts, not parts of a whole.
func buildBugFixOption(rows []analyze.BugFixWeek, cutover analyze.Cutover) (map[string]any, int) {
	labels := make([]string, len(rows))
	bug := make([]float64, len(rows))
	other := make([]float64, len(rows))
	cutoverIdx := -1
	prevYear := 0
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
		bug[i] = roundTo1(r.BugMedianLead.Hours() / 24)
		other[i] = roundTo1(r.OtherMedianLead.Hours() / 24)
	}

	bar := func(name string, data []float64) map[string]any {
		return map[string]any{
			"name": name, "type": "bar", "data": data,
			"itemStyle": map[string]any{"borderRadius": []int{3, 3, 0, 0}},
		}
	}

	opt := darkChartBase("Bug-fix speed — median engineering days per ISO week", cutover)
	opt["color"] = []string{chartDefectBug, chartLeadWait}
	opt["legend"] = darkLegend([]string{"Bug", "Other"})
	opt["tooltip"] = darkTooltip(map[string]any{"type": "shadow"})
	opt["xAxis"] = darkCategoryAxis(labels)
	opt["yAxis"] = darkValueAxis(map[string]any{
		"min":       0,
		"axisLabel": map[string]any{"formatter": "{value} d", "color": chartMuted},
	})
	opt["series"] = []map[string]any{bar("Bug", bug), bar("Other", other)}
	return opt, cutoverIdx
}

func marshalBugFixOption(rows []analyze.BugFixWeek, cutover analyze.Cutover) ([]byte, int, error) {
	opt, idx := buildBugFixOption(rows, cutover)
	b, err := marshalOption(opt)
	return b, idx, err
}

// buildWIPOption produces the ECharts option for the per-week work-in-progress
// snapshot (open issues, In Progress + Not Started).
func buildWIPOption(rows []analyze.WIPWeek, cutover analyze.Cutover) (map[string]any, int) {
	labels := make([]string, len(rows))
	inProg := make([]int, len(rows))
	notStarted := make([]int, len(rows))
	cutoverIdx := -1
	prevYear := 0
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
		inProg[i] = r.InProgress
		notStarted[i] = r.NotStarted
	}

	bar := func(name string, data []int, radius []int) map[string]any {
		return map[string]any{
			"name": name, "type": "bar", "stack": "wip", "data": data,
			"itemStyle": map[string]any{"borderRadius": radius},
		}
	}

	opt := darkChartBase("Work in progress — open issues", cutover)
	opt["tooltip"] = darkTooltip(map[string]any{"type": "shadow"})
	opt["xAxis"] = darkCategoryAxis(labels)
	opt["yAxis"] = darkValueAxis(map[string]any{"minInterval": 1})
	opt["color"] = []string{chartWipActive, chartWipPending}
	opt["legend"] = darkLegend([]string{"In Progress", "Not Started"})
	opt["series"] = []map[string]any{
		bar("In Progress", inProg, []int{0, 0, 0, 0}),
		bar("Not Started", notStarted, []int{3, 3, 0, 0}),
	}
	return opt, cutoverIdx
}

func marshalWIPOption(rows []analyze.WIPWeek, cutover analyze.Cutover) ([]byte, int, error) {
	opt, idx := buildWIPOption(rows, cutover)
	b, err := marshalOption(opt)
	return b, idx, err
}

// buildRepoAdoptionOption produces the ECharts option for the per-week
// stacked bar of repositories by AI adoption + activity. Returns the cutover
// label index (-1 if outside the window).
func buildRepoAdoptionOption(rows []analyze.RepoAdoptionWeek, cutover analyze.Cutover) (map[string]any, int) {
	labels := make([]string, len(rows))
	ai := make([]int, len(rows))
	active := make([]int, len(rows))
	inactive := make([]int, len(rows))
	cutoverIdx := -1
	prevYear := 0
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
		ai[i] = r.AIEnabled
		active[i] = r.Active
		inactive[i] = r.Inactive
	}

	bar := func(name string, data []int, radius []int) map[string]any {
		return map[string]any{
			"name": name, "type": "bar", "stack": "repos", "data": data,
			"itemStyle": map[string]any{"borderRadius": radius},
		}
	}

	opt := darkChartBase("AI-enabled repos", cutover)
	opt["tooltip"] = darkTooltip(map[string]any{"type": "shadow"})
	opt["xAxis"] = darkCategoryAxis(labels)
	opt["yAxis"] = darkValueAxis(map[string]any{"minInterval": 1})
	opt["color"] = []string{chartAccent2, chartRepoActive, chartRepoInactive}
	opt["legend"] = darkLegend([]string{"AI-enabled", "Active (≤90d)", "Inactive (>90d)"})
	opt["series"] = []map[string]any{
		bar("AI-enabled", ai, []int{0, 0, 0, 0}),
		bar("Active (≤90d)", active, []int{0, 0, 0, 0}),
		bar("Inactive (>90d)", inactive, []int{3, 3, 0, 0}),
	}
	return opt, cutoverIdx
}

func marshalRepoAdoptionOption(rows []analyze.RepoAdoptionWeek, cutover analyze.Cutover) ([]byte, int, error) {
	opt, idx := buildRepoAdoptionOption(rows, cutover)
	b, err := marshalOption(opt)
	return b, idx, err
}

func marshalCycleOption(cycles []analyze.WeekCycle, cutover analyze.Cutover) ([]byte, int, error) {
	opt, idx := buildCycleOption(cycles, cutover)
	b, err := json.Marshal(opt)
	return b, idx, err
}

func marshalOption(opt map[string]any) ([]byte, error) {
	return json.Marshal(opt)
}

// buildThroughputOption produces the ECharts option for the stacked
// weekly commit bar chart (human vs AI-tagged). When any week in the
// window contains medium-confidence (inferred) AI commits, a third
// stacked series renders them in a softer amber so the deck can show
// evidence-based and inferred adoption side by side without conflating
// them.
//
// The cutover marker is drawn as a JS graphic overlay (see overlayCutover
// in the template) rather than an ECharts markLine — markLine on a
// category axis snaps to integer indices, which would force the line
// through a bar centre.
func buildThroughputOption(weeks []analyze.WeekStats, cutover analyze.Cutover) map[string]any {
	labels, _ := axisLabelsAndCutover(weeks, cutover)
	human := make([]int, len(weeks))
	aiHigh := make([]int, len(weeks))
	aiMed := make([]int, len(weeks))
	hasMedium := false
	for i, w := range weeks {
		human[i] = w.TotalCommits - w.AICommits
		aiHigh[i] = w.AICommitsHigh
		aiMed[i] = w.AICommitsMedium
		if w.AICommitsMedium > 0 {
			hasMedium = true
		}
	}

	humanSeries := map[string]any{
		"name": "Human", "type": "bar", "stack": "total", "data": human,
		"itemStyle": map[string]any{"borderRadius": []int{0, 0, 0, 0}},
	}

	opt := darkChartBase("Weekly commits", cutover)
	opt["tooltip"] = darkTooltip(map[string]any{"type": "shadow"})
	opt["xAxis"] = darkCategoryAxis(labels)
	opt["yAxis"] = darkValueAxis(map[string]any{"minInterval": 1})

	if hasMedium {
		aiHighSeries := map[string]any{
			"name": "AI (evidence)", "type": "bar", "stack": "total", "data": aiHigh,
			"itemStyle": map[string]any{"borderRadius": []int{0, 0, 0, 0}},
		}
		aiMedSeries := map[string]any{
			"name": "AI (inferred)", "type": "bar", "stack": "total", "data": aiMed,
			"itemStyle": map[string]any{"borderRadius": []int{3, 3, 0, 0}},
		}
		opt["color"] = []string{chartAccent2, chartAccent, chartAccentSoft}
		opt["legend"] = darkLegend([]string{"Human", "AI (evidence)", "AI (inferred)"})
		opt["series"] = []map[string]any{humanSeries, aiHighSeries, aiMedSeries}
	} else {
		aiSeries := map[string]any{
			"name": "AI-tagged", "type": "bar", "stack": "total", "data": aiHigh,
			"itemStyle": map[string]any{"borderRadius": []int{3, 3, 0, 0}},
		}
		opt["color"] = []string{chartAccent2, chartAccent}
		opt["legend"] = darkLegend([]string{"Human", "AI-tagged"})
		opt["series"] = []map[string]any{humanSeries, aiSeries}
	}
	return opt
}

// buildProductivityOption is the throughput chart normalised by active
// developers — commits per active dev per week, split human vs AI-tagged.
// Normalising by headcount separates "more output" from "more people".
func buildProductivityOption(weeks []analyze.WeekStats, cutover analyze.Cutover) map[string]any {
	labels, _ := axisLabelsAndCutover(weeks, cutover)
	human := make([]float64, len(weeks))
	ai := make([]float64, len(weeks))
	for i, w := range weeks {
		if w.DistinctAuthors > 0 {
			d := float64(w.DistinctAuthors)
			human[i] = roundTo1(float64(w.TotalCommits-w.AICommits) / d)
			ai[i] = roundTo1(float64(w.AICommits) / d)
		}
	}
	humanSeries := map[string]any{
		"name": "Human", "type": "bar", "stack": "perdev", "data": human,
		"itemStyle": map[string]any{"borderRadius": []int{0, 0, 0, 0}},
	}
	aiSeries := map[string]any{
		"name": "AI-tagged", "type": "bar", "stack": "perdev", "data": ai,
		"itemStyle": map[string]any{"borderRadius": []int{3, 3, 0, 0}},
	}
	opt := darkChartBase("Output per active developer — commits/dev per week", cutover)
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
	human := make([]float64, len(weeks))
	aiHigh := make([]float64, len(weeks))
	aiMed := make([]float64, len(weeks))
	hasMedium := false
	for i, w := range weeks {
		if w.TotalCommits > 0 {
			t := float64(w.TotalCommits)
			human[i] = float64(w.TotalCommits-w.AICommits) / t * 100
			aiHigh[i] = float64(w.AICommitsHigh) / t * 100
			aiMed[i] = float64(w.AICommitsMedium) / t * 100
		}
		if w.AICommitsMedium > 0 {
			hasMedium = true
		}
	}

	humanSeries := map[string]any{
		"name": "Human", "type": "bar", "stack": "total", "data": human,
		"itemStyle": map[string]any{"borderRadius": []int{0, 0, 0, 0}},
	}

	opt := darkChartBase("AI adoption — % of weekly commits", cutover)
	opt["tooltip"] = darkTooltip(map[string]any{"type": "shadow"})
	opt["xAxis"] = darkCategoryAxis(labels)
	// Percentages sum to 100; pin the axis so the bars read as composition.
	opt["yAxis"] = darkValueAxis(map[string]any{
		"min":       0,
		"max":       100,
		"axisLabel": map[string]any{"formatter": "{value}%", "color": chartMuted},
	})

	if hasMedium {
		aiHighSeries := map[string]any{
			"name": "AI (evidence)", "type": "bar", "stack": "total", "data": aiHigh,
			"itemStyle": map[string]any{"borderRadius": []int{0, 0, 0, 0}},
		}
		aiMedSeries := map[string]any{
			"name": "AI (inferred)", "type": "bar", "stack": "total", "data": aiMed,
			"itemStyle": map[string]any{"borderRadius": []int{3, 3, 0, 0}},
		}
		opt["color"] = []string{chartAccent2, chartAccent, chartAccentSoft}
		opt["legend"] = darkLegend([]string{"Human", "AI (evidence)", "AI (inferred)"})
		opt["series"] = []map[string]any{humanSeries, aiHighSeries, aiMedSeries}
	} else {
		aiSeries := map[string]any{
			"name": "AI-tagged", "type": "bar", "stack": "total", "data": aiHigh,
			"itemStyle": map[string]any{"borderRadius": []int{3, 3, 0, 0}},
		}
		opt["color"] = []string{chartAccent2, chartAccent}
		opt["legend"] = darkLegend([]string{"Human", "AI-tagged"})
		opt["series"] = []map[string]any{humanSeries, aiSeries}
	}
	return opt
}

// buildCycleOption produces the ECharts option for the per-week
// cycle-time stacked bar. Idea→Dev is the top segment (drawn second in
// the stack so the legend reads top-down) and Dev→Release is the
// bottom. Values are days (float, rounded to one decimal in the
// tooltip). Returns the cutover index for the overlay (-1 if none).
func buildCycleOption(cycles []analyze.WeekCycle, cutover analyze.Cutover) (map[string]any, int) {
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
		idea[i] = roundTo1(c.MedianIdeaToDev.Hours() / 24)
		dev[i] = roundTo1(c.MedianDevToRelease.Hours() / 24)
		if cutover.Detected && c.WeekStart.Equal(cutover.Date) {
			cutoverIdx = i
		}
	}

	cycleSeries := map[string]any{
		"name": "Cycle (dev start → last commit)", "type": "bar", "stack": "cycle", "data": dev,
		"itemStyle": map[string]any{"borderRadius": []int{0, 0, 0, 0}},
	}
	waitSeries := map[string]any{
		"name": "Wait (Create → dev start)", "type": "bar", "stack": "cycle", "data": idea,
		"itemStyle": map[string]any{"borderRadius": []int{3, 3, 0, 0}},
	}

	opt := darkChartBase("Lead time — median days per ISO week", cutover)
	opt["color"] = []string{chartAccent2, chartLeadWait}
	opt["legend"] = darkLegend([]string{"Cycle (dev start → last commit)", "Wait (Create → dev start)"})
	opt["tooltip"] = darkTooltip(map[string]any{"type": "shadow"})
	opt["xAxis"] = darkCategoryAxis(labels)
	opt["yAxis"] = darkValueAxis(map[string]any{
		"axisLabel": map[string]any{"formatter": "{value} d", "color": chartMuted},
	})
	opt["series"] = []map[string]any{cycleSeries, waitSeries}
	return opt, cutoverIdx
}

func roundTo1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

// axisLabelsAndCutover returns one axis label per week plus the index of
// the cutover week (-1 if no cutover detected). The first label of each
// calendar year carries the year so a multi-year window doesn't render
// ambiguous "Jan 05" / "Jan 05" pairs 52 weeks apart.
func axisLabelsAndCutover(weeks []analyze.WeekStats, cutover analyze.Cutover) ([]string, int) {
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
		"axisLabel": map[string]any{"rotate": 45, "interval": echartsLabelInterval(len(labels)), "color": chartMuted},
		"axisLine":  map[string]any{"lineStyle": map[string]any{"color": chartAxisLine}},
		"axisTick":  map[string]any{"lineStyle": map[string]any{"color": chartAxisLine}},
	}
}

// maxAxisLabels is the most x-axis labels any time-series chart draws before
// thinning kicks in — keeps even a wide custom range (e.g. --months 24)
// legible.
const maxAxisLabels = 14

// echartsLabelInterval returns the ECharts axisLabel `interval` (labels to
// skip between shown labels) so at most maxAxisLabels render. 0 = show all.
func echartsLabelInterval(n int) int {
	if n <= maxAxisLabels {
		return 0
	}
	return (n+maxAxisLabels-1)/maxAxisLabels - 1
}

// staticLabelCount caps how many labels the go-analyze static charts draw.
func staticLabelCount(n int) int {
	if n < maxAxisLabels {
		return n
	}
	return maxAxisLabels
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
