package cmdstatus

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/StephanSchmidt/loupe/internal/analyze"
	"github.com/StephanSchmidt/loupe/internal/selfupdate"
	"github.com/StephanSchmidt/loupe/internal/store"
)

const stateDBPath = ".loupe/state.db"

func BuildStatusCmd(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Summarise what's indexed locally — provider counts, last-import timestamps",
		SilenceUsage: true,
		RunE:         func(cmd *cobra.Command, args []string) error { return runStatus(cmd, version) },
	}
	return cmd
}

func runStatus(cmd *cobra.Command, version string) error {
	out := cmd.OutOrStdout()

	if _, err := os.Stat(stateDBPath); err != nil {
		_, _ = fmt.Fprintf(out, "No state yet at %s — run `loupe baseline` first.\n", stateDBPath)
		writeUpdateLine(cmd.Context(), out, version)
		return nil
	}

	s, err := store.Open(stateDBPath)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	if err := WriteStatus(cmd.Context(), s, out); err != nil {
		return err
	}
	writeUpdateLine(cmd.Context(), out, version)
	return nil
}

func writeUpdateLine(ctx context.Context, out io.Writer, version string) {
	latest, newer := selfupdate.Check(ctx, version)
	if !newer {
		return
	}
	_, _ = fmt.Fprintf(out, "Update available: %s (you are on %s). Run `brew upgrade loupe` or see https://github.com/StephanSchmidt/loupe/releases\n", latest, version)
}

// WriteStatus is the format-and-print path, separated from runStatus so
// tests can drive it against a seeded in-memory store.
func WriteStatus(ctx context.Context, s *store.Store, out io.Writer) error {
	data, err := loadStatusData(ctx, s)
	if err != nil {
		return err
	}
	if data.empty() {
		_, _ = fmt.Fprintln(out, "Empty state — run `loupe baseline` to index providers.")
		return nil
	}
	writeHostLines(out, data.hosts)
	writeTrackerLines(out, data.trackers)
	writeCommitLines(out, data.counts, data.tickets)
	writeSignalLines(out, data.signals)
	writeTicketLinkLine(out, data.ticketLinks, data.ticketTransitions)
	writeBotExclusion(out, data.counts)
	return nil
}

type statusData struct {
	hosts             []hostStats
	trackers          []trackerStats
	counts            commitSummary
	tickets           int
	signals           []signalCount
	ticketLinks       int
	ticketTransitions int
}

func (d statusData) empty() bool {
	return len(d.hosts) == 0 && len(d.trackers) == 0 && d.counts.total == 0 && d.tickets == 0
}

func loadStatusData(ctx context.Context, s *store.Store) (statusData, error) {
	var d statusData
	var err error
	if d.hosts, err = perGitHostStats(ctx, s.DB()); err != nil {
		return d, err
	}
	if d.trackers, err = perTrackerStats(ctx, s.DB()); err != nil {
		return d, err
	}
	if d.counts, err = commitCounts(ctx, s.DB()); err != nil {
		return d, err
	}
	if d.tickets, err = count(ctx, s.DB(), "SELECT COUNT(*) FROM tickets"); err != nil {
		return d, err
	}
	if d.signals, err = signalBreakdown(ctx, s.DB()); err != nil {
		return d, err
	}
	if d.ticketLinks, err = count(ctx, s.DB(), "SELECT COUNT(DISTINCT ticket_id) FROM ticket_commits"); err != nil {
		return d, err
	}
	if d.ticketTransitions, err = count(ctx, s.DB(), "SELECT COUNT(*) FROM ticket_transitions"); err != nil {
		return d, err
	}
	return d, nil
}

// writeTicketLinkLine surfaces ticket↔commit and changelog coverage. Both
// counts are zero before v0.3 — the line stays silent on those older DBs.
func writeTicketLinkLine(out io.Writer, links, transitions int) {
	if links == 0 && transitions == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "%-10s %d tickets linked to commits, %d status transitions recorded\n",
		"Cycle:", links, transitions)
}

func writeHostLines(out io.Writer, hosts []hostStats) {
	for _, h := range hosts {
		_, _ = fmt.Fprintf(out, "%-10s %d %s (%d %s)%s\n",
			displayName(h.provider)+":",
			h.workspaces, pluralise("workspace", h.workspaces),
			h.repos, pluralise("repo", h.repos),
			formatLastIndexed("last commit indexed", h.lastIndexed),
		)
	}
}

func writeTrackerLines(out io.Writer, trackers []trackerStats) {
	for _, t := range trackers {
		_, _ = fmt.Fprintf(out, "%-10s %d %s%s\n",
			displayName(t.provider)+":",
			t.projects, pluralise("project", t.projects),
			formatLastIndexed("last issue indexed", t.lastIndexed),
		)
	}
}

func writeCommitLines(out io.Writer, counts commitSummary, tickets int) {
	aiPct := 0.0
	if counts.total > 0 {
		aiPct = float64(counts.ai) / float64(counts.total) * 100
	}
	_, _ = fmt.Fprintf(out, "%-10s %d  (%d AI-tagged, %.1f%%)\n", "Commits:", counts.total, counts.ai, aiPct)
	_, _ = fmt.Fprintf(out, "%-10s %d\n", "Tickets:", tickets)
}

func writeSignalLines(out io.Writer, signals []signalCount) {
	if len(signals) == 0 {
		return
	}
	_, _ = fmt.Fprintln(out, "Signals:")
	for _, sig := range signals {
		_, _ = fmt.Fprintf(out, "  %-18s %d\n", signalKindLabel(sig.kind)+":", sig.count)
	}
}

func writeBotExclusion(out io.Writer, counts commitSummary) {
	if counts.botExcluded == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "Excluded %d bot-authored commits across %d bots (%s)\n",
		counts.botExcluded, len(counts.botDisplayNames), formatBotList(counts.botDisplayNames))
}

type signalCount struct {
	kind  string
	count int
}

// signalBreakdown returns the per-signal-kind row count from ai_signals,
// ordered by kind so the status output is stable across runs.
func signalBreakdown(ctx context.Context, db *sql.DB) ([]signalCount, error) {
	rows, err := db.QueryContext(ctx, `
        SELECT signal_kind, COUNT(*)
        FROM ai_signals
        GROUP BY signal_kind
        ORDER BY signal_kind
    `)
	if err != nil {
		return nil, fmt.Errorf("status: signal breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []signalCount
	for rows.Next() {
		var sc signalCount
		if err := rows.Scan(&sc.kind, &sc.count); err != nil {
			return nil, fmt.Errorf("status: scan signal row: %w", err)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// signalKindLabel renders an ai_signals.signal_kind value into the human
// label used in `loupe status`. Unknown kinds fall through to the raw
// value so future detectors don't break the output silently.
func signalKindLabel(kind string) string {
	switch kind {
	case analyze.KindCoAuthorTrailer:
		return "Co-author trailers"
	case analyze.KindBodyFooter:
		return "Body footers"
	case analyze.KindBotAuthor:
		return "Bot authors"
	case analyze.KindPRLabel:
		return "PR labels"
	case analyze.KindBranchName:
		return "Branch names"
	case analyze.KindSquashRecovery:
		return "Squash recovery"
	case analyze.KindSeatInference:
		return "Seat inference"
	default:
		return kind
	}
}

type hostStats struct {
	provider    string
	workspaces  int
	repos       int
	lastIndexed sql.NullInt64
}

type trackerStats struct {
	provider    string
	projects    int
	lastIndexed sql.NullInt64
}

func perGitHostStats(ctx context.Context, db *sql.DB) ([]hostStats, error) {
	rows, err := db.QueryContext(ctx, `
        SELECT w.provider,
               COUNT(DISTINCT w.slug)                   AS workspaces,
               COALESCE((SELECT COUNT(*) FROM repos r WHERE r.provider = w.provider), 0) AS repos,
               MAX(w.last_indexed_at)                   AS last_indexed
        FROM workspaces w
        GROUP BY w.provider
        ORDER BY w.provider
    `)
	if err != nil {
		return nil, fmt.Errorf("status: query workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []hostStats
	for rows.Next() {
		var h hostStats
		if err := rows.Scan(&h.provider, &h.workspaces, &h.repos, &h.lastIndexed); err != nil {
			return nil, fmt.Errorf("status: scan workspace row: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func perTrackerStats(ctx context.Context, db *sql.DB) ([]trackerStats, error) {
	rows, err := db.QueryContext(ctx, `
        SELECT provider, COUNT(*) AS projects, MAX(last_issue_indexed_at) AS last_indexed
        FROM tracker_projects
        GROUP BY provider
        ORDER BY provider
    `)
	if err != nil {
		return nil, fmt.Errorf("status: query tracker_projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []trackerStats
	for rows.Next() {
		var t trackerStats
		if err := rows.Scan(&t.provider, &t.projects, &t.lastIndexed); err != nil {
			return nil, fmt.Errorf("status: scan tracker row: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type commitSummary struct {
	total       int
	ai          int
	botExcluded int
	// botDisplayNames collapses excluded bots to canonical labels
	// (Dependabot, Renovate, …) so multiple raw email forms for the
	// same automation appear once in the footer.
	botDisplayNames []string
}

// commitCounts walks the commits table once, partitioning rows into the
// human-visible numerator/denominator and the bot rollup. Both totals
// exclude bots — those land in botExcluded / botDisplayNames so callers
// can surface what was dropped.
func commitCounts(ctx context.Context, db *sql.DB) (commitSummary, error) {
	rows, err := db.QueryContext(ctx, `
        SELECT c.author_email, c.author_name,
               CASE WHEN sig.commit_sha IS NOT NULL THEN 1 ELSE 0 END AS is_ai
        FROM commits c
        LEFT JOIN (SELECT DISTINCT commit_sha FROM ai_signals) sig
            ON sig.commit_sha = c.sha
    `)
	if err != nil {
		return commitSummary{}, fmt.Errorf("status: scan commits: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out commitSummary
	displayNames := make(map[string]struct{})
	for rows.Next() {
		var email, name string
		var ai int
		if err := rows.Scan(&email, &name, &ai); err != nil {
			return commitSummary{}, fmt.Errorf("status: scan commit row: %w", err)
		}
		if analyze.IsBot(email, name) {
			if _, isAIBot := analyze.IsAIBot(email, name); !isAIBot {
				out.botExcluded++
				displayNames[analyze.BotDisplayName(email, name)] = struct{}{}
				continue
			}
		}
		out.total++
		if ai == 1 {
			out.ai++
		}
	}
	if err := rows.Err(); err != nil {
		return commitSummary{}, fmt.Errorf("status: iterate commit rows: %w", err)
	}
	out.botDisplayNames = make([]string, 0, len(displayNames))
	for d := range displayNames {
		out.botDisplayNames = append(out.botDisplayNames, d)
	}
	sort.Strings(out.botDisplayNames)
	return out, nil
}

// formatBotList renders the bot-name list for the exclusion footer.
// Truncates to keep the line readable when many bots are present.
func formatBotList(names []string) string {
	const maxShown = 5
	if len(names) <= maxShown {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:maxShown], ", ") + ", ..."
}

func count(ctx context.Context, db *sql.DB, q string) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("status: %s: %w", q, err)
	}
	return n, nil
}

func displayName(provider string) string {
	switch provider {
	case "bitbucket-cloud":
		return "Bitbucket"
	case "jira-cloud":
		return "Jira"
	case "github":
		return "GitHub"
	default:
		return provider
	}
}

func pluralise(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

func formatLastIndexed(label string, ts sql.NullInt64) string {
	if !ts.Valid || ts.Int64 == 0 {
		return ""
	}
	t := time.Unix(ts.Int64, 0).UTC()
	return fmt.Sprintf(" — %s %s UTC", label, t.Format("2006-01-02 15:04"))
}
