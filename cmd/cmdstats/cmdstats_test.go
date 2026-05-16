package cmdstats

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/store"
)

// seedWeeklyStore plants a deterministic four-week series with mixed AI
// tools and a revert that backlinks to an AI commit.
//
// w0 (2026-01-05): 4 commits, 0 AI, 2 authors
// w1 (2026-01-12): 4 commits, 0 AI, 2 authors
// w2 (2026-01-19): 4 commits, 3 AI, 2 authors (cutover week — ratio 75%)
// w3 (2026-01-26): 4 commits, 2 AI, 2 authors
//
// Author a@x writes AI commits; b@x is human-only. AI tools used:
// 3× claude, 1× copilot, 1× cursor. One human commit in w3 reverts the
// AI commit w2a — so AI-revert-rate is 1/5 = 20%, human is 0/11 = 0%.
func seedWeeklyStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB().Exec(q, args...); err != nil {
			t.Fatalf("seed: %s: %v", q, err)
		}
	}

	exec(`INSERT INTO workspaces (provider, slug, name, discovered_at, last_indexed_at)
        VALUES ('github', 'acme', 'Acme', 1700000000, 1700003000)`)
	exec(`INSERT INTO repos (provider, full_name, workspace, slug, name, discovered_at)
        VALUES ('github', 'acme/repo', 'acme', 'repo', 'repo', 1700000000)`)

	monday := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC).Unix()
	week := int64(7 * 24 * 3600)

	type row struct {
		sha     string
		email   string
		name    string
		offset  int64
		message string
		aiTools []string // empty = human commit
	}
	// SHAs must be valid hex (>=7 chars) so the revert-pattern regex
	// recognises backlinks against them.
	rows := []row{
		// w0
		{"aaa0001", "alice@x", "alice", 0*week + 0, "feat", nil},
		{"aaa0002", "alice@x", "alice", 0*week + 100, "feat", nil},
		{"aaa0003", "carol@x", "carol", 0*week + 200, "feat", nil},
		{"aaa0004", "carol@x", "carol", 0*week + 300, "feat", nil},
		// w1
		{"bbb0001", "alice@x", "alice", 1*week + 0, "feat", nil},
		{"bbb0002", "alice@x", "alice", 1*week + 100, "feat", nil},
		{"bbb0003", "carol@x", "carol", 1*week + 200, "feat", nil},
		{"bbb0004", "carol@x", "carol", 1*week + 300, "feat", nil},
		// w2 — three AI commits, mixed tools
		{"ccc0001", "alice@x", "alice", 2*week + 0, "feat", []string{"claude"}},
		{"ccc0002", "alice@x", "alice", 2*week + 100, "feat", []string{"claude"}},
		{"ccc0003", "bob@x", "bob", 2*week + 200, "feat", []string{"copilot"}},
		{"ccc0004", "carol@x", "carol", 2*week + 300, "feat", nil},
		// w3 — two AI commits (claude + cursor); one human revert of ccc0001
		{"ddd0001", "alice@x", "alice", 3*week + 0, "feat", []string{"claude"}},
		{"ddd0002", "alice@x", "alice", 3*week + 100, "feat", []string{"cursor"}},
		{"ddd0003", "carol@x", "carol", 3*week + 200, "Revert \"feat\"\n\nThis reverts commit ccc0001.", nil},
		{"ddd0004", "carol@x", "carol", 3*week + 300, "feat", nil},
		// Bots — three commits across two bots, one with a Claude trailer.
		// Every assertion below must show these rows excluded.
		{"eee0001", "49699333+dependabot[bot]@users.noreply.github.com", "dependabot[bot]", 3*week + 400, "chore(deps): bump foo", []string{"claude"}},
		{"eee0002", "49699333+dependabot[bot]@users.noreply.github.com", "dependabot[bot]", 3*week + 500, "chore(deps): bump bar", nil},
		{"eee0003", "29139614+renovate[bot]@users.noreply.github.com", "renovate[bot]", 3*week + 600, "chore: pin baz", nil},
	}
	for _, r := range rows {
		exec(`INSERT INTO commits (sha, repo_name, author_email, author_name, committed_at, message)
            VALUES (?, 'acme/repo', ?, ?, ?, ?)`,
			r.sha, r.email, r.name, monday+r.offset, r.message)
		for _, tool := range r.aiTools {
			exec(`INSERT INTO ai_signals (commit_sha, signal_kind, signal_source, confidence)
                VALUES (?, 'co_author_trailer', ?, 'high')`, r.sha, tool)
		}
	}
	return s
}

func TestWriteStats_FullSeries(t *testing.T) {
	s := seedWeeklyStore(t)
	defer func() { _ = s.Close() }()

	var buf bytes.Buffer
	if err := WriteStats(context.Background(), s, &buf, 0.05, time.Time{}, analyze.CycleConfig{}); err != nil {
		t.Fatalf("WriteStats: %v", err)
	}
	out := buf.String()
	t.Logf("output:\n%s", out)

	mustContain := []string{
		"Weeks: 4",
		"2026-01-05 → 2026-01-26",
		"Weekly commits",
		"Weekly AI commits",
		"AI commit ratio",
		// Cutover at week 2 because 3/4 = 75% >= 5%.
		"Cutover: 2026-01-19",
		"auto",
		"threshold 5.0%",
		"Before",
		"After",
		// AI tool breakdown — three tools.
		"AI tools (",
		"claude",
		"copilot",
		"cursor",
		// Trend line present (sign + direction can vary slightly, but
		// "pp/week" is the stable token).
		"pp/week",
		// Author adoption — 2 of 3 (alice + bob), carol is non-AI.
		"Authors with AI commits: 2 of 3",
		"Authors without AI commits (1):",
		"carol@x",
		// Revert backlinks — one AI commit reverted, zero human.
		"Reverts: 1 of 16 commits",
		"AI commits reverted:    1 of    5",
		"Human commits reverted: 0 of   11",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		"(no weeks)", // both cutover halves are populated
		"alice@x\n",  // alice is an AI author — should not appear in non-AI list
		// Bot rows must not leak into any section.
		"dependabot",
		"renovate",
		// Bot AI-signal must not bump claude count above 3.
		"claude       4",
		// Bot commits must not inflate the totals (16 human commits, not 19).
		"Reverts: 1 of 19",
		// Bot authors must not appear in the adoption denominator (was 2/3, not 2/5).
		"Authors with AI commits: 2 of 5",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unexpected %q in output:\n%s", unwanted, out)
		}
	}
}

func TestWriteStats_OverrideCutover(t *testing.T) {
	s := seedWeeklyStore(t)
	defer func() { _ = s.Close() }()

	override := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := WriteStats(context.Background(), s, &buf, 0.05, override, analyze.CycleConfig{}); err != nil {
		t.Fatalf("WriteStats: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "Cutover: 2026-01-12") {
		t.Errorf("expected override cutover 2026-01-12, got:\n%s", out)
	}
	if !strings.Contains(out, "config-override") {
		t.Errorf("expected reason 'config-override' in output:\n%s", out)
	}
}

func TestWriteStats_EmptyStore(t *testing.T) {
	s, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var buf bytes.Buffer
	if err := WriteStats(context.Background(), s, &buf, 0.05, time.Time{}, analyze.CycleConfig{}); err != nil {
		t.Fatalf("WriteStats: %v", err)
	}
	if !strings.Contains(buf.String(), "No weekly data yet") {
		t.Errorf("expected empty-state message, got: %q", buf.String())
	}
}

func TestSummarise_BasicSeries(t *testing.T) {
	s, err := summarise([]float64{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if s.mean != 3 {
		t.Errorf("mean = %v, want 3", s.mean)
	}
	if s.median != 3 {
		t.Errorf("median = %v, want 3", s.median)
	}
	if s.min != 1 || s.max != 5 {
		t.Errorf("min/max = %v/%v, want 1/5", s.min, s.max)
	}
}

func TestSummarise_EmptyErrors(t *testing.T) {
	if _, err := summarise(nil); err == nil {
		t.Errorf("summarise(nil) should error")
	}
}
