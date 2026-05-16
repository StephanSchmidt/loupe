package deck

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path/filepath"
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
	MedianIdeaToDevText string
	MedianDevToRelText  string
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
	reportDate time.Time,
) error {
	if err := os.MkdirAll(deckDir, 0o750); err != nil {
		return fmt.Errorf("create deck dir %s: %w", deckDir, err)
	}

	if err := copyEmbeddedAssets(filepath.Join(deckDir, "assets")); err != nil {
		return err
	}

	payload, err := BuildChartPayload(weeks, cutover, cycles)
	if err != nil {
		return fmt.Errorf("build chart payload: %w", err)
	}

	if err := RenderStaticCharts(weeks, cutover, cycles, filepath.Join(deckDir, "charts")); err != nil {
		return fmt.Errorf("render static charts: %w", err)
	}

	data := buildDeckData(cfg, weeks, cutover, cycles, reportDate)
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

func buildDeckData(
	cfg *config.Config,
	weeks []analyze.WeekStats,
	cutover analyze.Cutover,
	cycles []analyze.WeekCycle,
	reportDate time.Time,
) DeckData {
	d := DeckData{
		OrgName:             cfg.Org,
		Scope:               cfg.Org,
		ReportDate:          reportDate,
		Weeks:               weeks,
		Cutover:             cutover,
		CutoverThresholdPct: cutover.Threshold * 100,
		Cycles:              cycles,
		CyclesAvailable:     len(cycles) > 0,
	}

	if len(cycles) > 0 {
		populateCycleSummary(&d, cycles)
	}

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
