package deck

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/StephanSchmidt/loupe/internal/analyze"
)

func sampleWeeks() []analyze.WeekStats {
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	out := make([]analyze.WeekStats, 12)
	for i := range out {
		out[i] = analyze.WeekStats{
			WeekStart:       base.AddDate(0, 0, 7*i),
			TotalCommits:    20 + i*2,
			AICommits:       i,
			DistinctAuthors: 5,
			AIAuthors:       (i + 1) / 2,
		}
	}
	return out
}

func sampleCutover(weeks []analyze.WeekStats) analyze.Cutover {
	return analyze.Cutover{
		Detected:  true,
		Date:      weeks[6].WeekStart,
		Reason:    analyze.CutoverReasonAuto,
		Threshold: 0.05,
	}
}

// decodeOption parses the option blob back into a generic map for shape
// assertions. Tests inspect a few load-bearing fields rather than the full
// ECharts schema, which is too broad and changes upstream.
func decodeOption(t *testing.T, blob string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(blob), &out); err != nil {
		t.Fatalf("unmarshal option: %v\nblob: %s", err, blob)
	}
	return out
}

func TestBuildChartPayload_ThroughputShape(t *testing.T) {
	weeks := sampleWeeks()
	cutover := sampleCutover(weeks)

	got, err := BuildChartPayload(weeks, cutover, nil)
	if err != nil {
		t.Fatalf("BuildChartPayload: %v", err)
	}

	opt := decodeOption(t, string(got.ThroughputJSON))

	series, _ := opt["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("throughput series len = %d, want 2", len(series))
	}

	human, _ := series[0].(map[string]any)
	if human["name"] != "Human" || human["stack"] != "total" {
		t.Errorf("human series misconfigured: %+v", human)
	}
	humanData, _ := human["data"].([]any)
	if len(humanData) != len(weeks) {
		t.Errorf("human series data len = %d, want %d", len(humanData), len(weeks))
	}

	ai, _ := series[1].(map[string]any)
	if ai["name"] != "AI-tagged" || ai["stack"] != "total" {
		t.Errorf("ai series misconfigured: %+v", ai)
	}
	// markLine is deliberately not in the option — the cutover marker is
	// drawn as a JS graphic overlay (template.html.tmpl: overlayCutover)
	// because markLine on a category axis would snap through a bar.
	if _, hasMark := ai["markLine"]; hasMark {
		t.Errorf("AI series carries markLine; expected it to be overlay-only: %+v", ai)
	}
	if got.ThroughputCutoverIdx != 6 {
		t.Errorf("ThroughputCutoverIdx = %d, want 6 (sampleCutover at week 6)", got.ThroughputCutoverIdx)
	}

	title, _ := opt["title"].(map[string]any)
	if title["text"] != "Weekly commits" {
		t.Errorf("title.text = %v, want %q", title["text"], "Weekly commits")
	}
	subtext, _ := title["subtext"].(string)
	if !strings.Contains(subtext, cutover.Date.Format("Jan 2, 2006")) {
		t.Errorf("title.subtext = %q, want it to reference cutover date", subtext)
	}
}

func TestBuildChartPayload_AdoptionShape(t *testing.T) {
	weeks := sampleWeeks()
	cutover := sampleCutover(weeks)

	got, err := BuildChartPayload(weeks, cutover, nil)
	if err != nil {
		t.Fatalf("BuildChartPayload: %v", err)
	}

	opt := decodeOption(t, string(got.AdoptionJSON))

	series, _ := opt["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("adoption series len = %d, want 1", len(series))
	}
	s0, _ := series[0].(map[string]any)
	if s0["type"] != "line" {
		t.Errorf("adoption series type = %v, want line", s0["type"])
	}
	if _, hasMark := s0["markLine"]; hasMark {
		t.Errorf("adoption series carries markLine; expected overlay-only: %+v", s0)
	}
	if got.AdoptionCutoverIdx != 6 {
		t.Errorf("AdoptionCutoverIdx = %d, want 6", got.AdoptionCutoverIdx)
	}

	y, _ := opt["yAxis"].(map[string]any)
	if y["max"].(float64) != 100 {
		t.Errorf("yAxis.max = %v, want 100", y["max"])
	}
}

func TestBuildChartPayload_NoCutoverSignalsAbsence(t *testing.T) {
	weeks := sampleWeeks()
	got, err := BuildChartPayload(weeks, analyze.Cutover{Detected: false}, nil)
	if err != nil {
		t.Fatalf("BuildChartPayload: %v", err)
	}
	if got.ThroughputCutoverIdx != -1 || got.AdoptionCutoverIdx != -1 {
		t.Errorf("expected CutoverIdx == -1 when not detected, got %d/%d",
			got.ThroughputCutoverIdx, got.AdoptionCutoverIdx)
	}
	opt := decodeOption(t, string(got.ThroughputJSON))
	title, _ := opt["title"].(map[string]any)
	if _, hasSubtext := title["subtext"]; hasSubtext {
		t.Errorf("title.subtext should be absent when cutover not detected; got: %+v", title)
	}
}

func TestBuildChartPayload_NoData(t *testing.T) {
	if _, err := BuildChartPayload(nil, analyze.Cutover{}, nil); err == nil {
		t.Errorf("expected error for empty weeks, got nil")
	}
}

func sampleCycles() []analyze.WeekCycle {
	base := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	out := make([]analyze.WeekCycle, 3)
	for i := range out {
		out[i] = analyze.WeekCycle{
			WeekStart:           base.AddDate(0, 0, 7*i),
			TicketCount:         5,
			FallbackTicketCount: 1,
			MedianIdeaToDev:     time.Duration(i+1) * 24 * time.Hour,
			MedianDevToRelease:  time.Duration(i+2) * 24 * time.Hour,
			P10IdeaToDev:        12 * time.Hour,
			P90IdeaToDev:        48 * time.Hour,
			P10DevToRelease:     24 * time.Hour,
			P90DevToRelease:     96 * time.Hour,
		}
	}
	return out
}

func TestBuildChartPayload_CycleShape(t *testing.T) {
	weeks := sampleWeeks()
	cutover := sampleCutover(weeks)
	cycles := sampleCycles()

	got, err := BuildChartPayload(weeks, cutover, cycles)
	if err != nil {
		t.Fatalf("BuildChartPayload: %v", err)
	}
	if !got.HasCycle {
		t.Fatal("expected HasCycle=true when cycles supplied")
	}
	if got.CycleJSON == "" {
		t.Fatal("expected CycleJSON to be populated")
	}

	opt := decodeOption(t, string(got.CycleJSON))
	series, _ := opt["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("expected 2 stacked cycle series (Dev→Release, Idea→Dev), got %d", len(series))
	}
	if !strings.Contains(string(got.CycleJSON), "Idea") {
		t.Errorf("CycleJSON missing 'Idea': %s", got.CycleJSON)
	}
	if !strings.Contains(string(got.CycleJSON), "Release") {
		t.Errorf("CycleJSON missing 'Release': %s", got.CycleJSON)
	}
}

func TestBuildChartPayload_NoCycleWhenNil(t *testing.T) {
	weeks := sampleWeeks()
	got, err := BuildChartPayload(weeks, sampleCutover(weeks), nil)
	if err != nil {
		t.Fatalf("BuildChartPayload: %v", err)
	}
	if got.HasCycle {
		t.Error("expected HasCycle=false when cycles is nil")
	}
	if got.CycleCutoverIdx != -1 {
		t.Errorf("CycleCutoverIdx = %d, want -1", got.CycleCutoverIdx)
	}
}
