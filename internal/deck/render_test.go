package deck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/config"
)

func TestRenderDeck_ProducesAllArtifacts(t *testing.T) {
	cfg := &config.Config{
		Org:     "acme-eng",
		GitHost: config.GitHostConfig{Provider: config.ProviderBitbucketCloud},
	}
	weeks := sampleWeeks()
	cutover := sampleCutover(weeks)
	reportDate := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	if err := RenderDeck(dir, cfg, weeks, cutover, nil, reportDate); err != nil {
		t.Fatalf("RenderDeck: %v", err)
	}

	wantFiles := []string{
		"index.html",
		"assets/reveal.js",
		"assets/reveal.css",
		"assets/echarts.min.js",
		"assets/loupe.svg",
		"charts/throughput.png",
		"charts/throughput.svg",
		"charts/adoption.png",
		"charts/adoption.svg",
	}
	for _, f := range wantFiles {
		p := filepath.Join(dir, f)
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("missing artifact %s: %v", f, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("artifact %s is empty", f)
		}
	}

	// Smoke-test the rendered HTML: verifies template fields were substituted
	// and the page wires up ECharts on the expected containers.
	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	body := string(html)
	for _, want := range []string{
		"acme-eng",
		"AI Impact Report",
		`id="throughput-chart"`,
		`id="adoption-chart"`,
		"assets/echarts.min.js",
		"echarts.init",
		`class="deck-logo"`,
		`src="assets/loupe.svg"`,
		"Co-Authored-By",
		"Auto-detected",
		// Dark theme markers — guard against accidental reversion to a
		// light theme template.
		`color-scheme" content="dark"`,
		"--bg: #0b0f17",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// Lock-in: the deck no longer ships a theme stylesheet — the dark
	// theme lives inline in the template.
	if strings.Contains(body, "theme/white.css") || strings.Contains(body, "theme/black.css") {
		t.Errorf("index.html still references a reveal theme stylesheet — should be inline-only")
	}
}

func TestRenderDeck_IncludesCycleSlideWhenCyclesPresent(t *testing.T) {
	cfg := &config.Config{
		Org:     "acme-eng",
		GitHost: config.GitHostConfig{Provider: config.ProviderBitbucketCloud},
	}
	weeks := sampleWeeks()
	cutover := sampleCutover(weeks)
	cycles := sampleCycles()
	reportDate := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	if err := RenderDeck(dir, cfg, weeks, cutover, cycles, reportDate); err != nil {
		t.Fatalf("RenderDeck: %v", err)
	}

	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	body := string(html)
	for _, want := range []string{
		`id="cycle-chart"`,
		"Cycle time",
		"Idea → Dev",
		"Dev → Release",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q on rendered deck", want)
		}
	}

	for _, rel := range []string{"charts/cycle.png", "charts/cycle.svg"} {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Errorf("missing %s: %v", rel, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", rel)
		}
	}
}

func TestRenderDeck_NoCutoverDetected(t *testing.T) {
	cfg := &config.Config{
		Org:     "acme-eng",
		GitHost: config.GitHostConfig{Provider: config.ProviderBitbucketCloud},
	}
	weeks := sampleWeeks()
	cutover := analyze.Cutover{Detected: false, Reason: analyze.CutoverReasonNotDetected, Threshold: 0.05}
	reportDate := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	if err := RenderDeck(dir, cfg, weeks, cutover, nil, reportDate); err != nil {
		t.Fatalf("RenderDeck: %v", err)
	}
	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(html), "No AI cutover detected") {
		t.Errorf("expected not-detected cutover text in slide; got: %s", html)
	}
}
