package deck

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StephanSchmidt/loupe/internal/analyze"
)

// pngMagic is the 8-byte PNG signature.
var pngMagic = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

func assertPNG(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 1000 {
		t.Errorf("%s suspiciously small (%d bytes)", path, len(data))
	}
	if !bytes.HasPrefix(data, pngMagic) {
		t.Errorf("%s is not a PNG: header = %x", path, data[:min(8, len(data))])
	}
}

func assertSVG(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 200 {
		t.Errorf("%s suspiciously small (%d bytes)", path, len(data))
	}
	// Tolerate both bare <svg> and <?xml … ?><svg>.
	head := string(data[:min(200, len(data))])
	if !strings.Contains(head, "<svg") {
		t.Errorf("%s does not look like SVG, first 200 bytes: %q", path, head)
	}
}

func TestRenderStaticCharts_ProducesPngAndSvg(t *testing.T) {
	weeks := sampleWeeks()
	cutover := sampleCutover(weeks)
	dir := t.TempDir()

	if err := RenderStaticCharts(weeks, cutover, nil, dir); err != nil {
		t.Fatalf("RenderStaticCharts: %v", err)
	}

	assertPNG(t, filepath.Join(dir, "throughput.png"))
	assertPNG(t, filepath.Join(dir, "adoption.png"))
	assertSVG(t, filepath.Join(dir, "throughput.svg"))
	assertSVG(t, filepath.Join(dir, "adoption.svg"))
}

func TestRenderStaticCharts_NoCutover(t *testing.T) {
	weeks := sampleWeeks()
	dir := t.TempDir()

	if err := RenderStaticCharts(weeks, analyze.Cutover{Detected: false}, nil, dir); err != nil {
		t.Fatalf("RenderStaticCharts without cutover: %v", err)
	}
	assertPNG(t, filepath.Join(dir, "throughput.png"))
	assertSVG(t, filepath.Join(dir, "adoption.svg"))
}

func TestRenderStaticCharts_NoData(t *testing.T) {
	if err := RenderStaticCharts(nil, analyze.Cutover{}, nil, t.TempDir()); err == nil {
		t.Errorf("expected error for empty weeks, got nil")
	}
}

func TestRenderStaticCharts_EmitsCycleWhenSupplied(t *testing.T) {
	weeks := sampleWeeks()
	cutover := sampleCutover(weeks)
	cycles := sampleCycles()
	dir := t.TempDir()

	if err := RenderStaticCharts(weeks, cutover, cycles, dir); err != nil {
		t.Fatalf("RenderStaticCharts with cycle: %v", err)
	}
	assertPNG(t, filepath.Join(dir, "cycle.png"))
	assertSVG(t, filepath.Join(dir, "cycle.svg"))
}
