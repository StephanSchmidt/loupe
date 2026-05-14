package cmdstatus

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/StephanSchmidt/loupe/internal/store"
)

const stateDBPath = ".loupe/state.db"

func BuildStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Summarise what's indexed locally — provider counts, last-import timestamps",
		SilenceUsage: true,
		RunE:         runStatus,
	}
	return cmd
}

func runStatus(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()

	if _, err := os.Stat(stateDBPath); err != nil {
		_, _ = fmt.Fprintf(out, "No state yet at %s — run `loupe baseline` first.\n", stateDBPath)
		return nil
	}

	s, err := store.Open(stateDBPath)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	return WriteStatus(context.Background(), s, out)
}

// WriteStatus is the format-and-print path, separated from runStatus so
// tests can drive it against a seeded in-memory store.
func WriteStatus(ctx context.Context, s *store.Store, out io.Writer) error {
	hosts, err := perGitHostStats(ctx, s.DB())
	if err != nil {
		return err
	}
	trackers, err := perTrackerStats(ctx, s.DB())
	if err != nil {
		return err
	}
	commits, aiCommits, err := commitCounts(ctx, s.DB())
	if err != nil {
		return err
	}
	tickets, err := count(ctx, s.DB(), "SELECT COUNT(*) FROM tickets")
	if err != nil {
		return err
	}

	if len(hosts) == 0 && len(trackers) == 0 && commits == 0 && tickets == 0 {
		_, _ = fmt.Fprintln(out, "Empty state — run `loupe baseline` to index providers.")
		return nil
	}

	for _, h := range hosts {
		_, _ = fmt.Fprintf(out, "%-10s %d %s (%d %s)%s\n",
			displayName(h.provider)+":",
			h.workspaces, pluralise("workspace", h.workspaces),
			h.repos, pluralise("repo", h.repos),
			formatLastIndexed("last commit indexed", h.lastIndexed),
		)
	}
	for _, t := range trackers {
		_, _ = fmt.Fprintf(out, "%-10s %d %s%s\n",
			displayName(t.provider)+":",
			t.projects, pluralise("project", t.projects),
			formatLastIndexed("last issue indexed", t.lastIndexed),
		)
	}

	aiPct := 0.0
	if commits > 0 {
		aiPct = float64(aiCommits) / float64(commits) * 100
	}
	_, _ = fmt.Fprintf(out, "%-10s %d  (%d AI-tagged, %.1f%%)\n", "Commits:", commits, aiCommits, aiPct)
	_, _ = fmt.Fprintf(out, "%-10s %d\n", "Tickets:", tickets)

	return nil
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

func commitCounts(ctx context.Context, db *sql.DB) (total, aiTagged int, _ error) {
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commits`).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("status: count commits: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT commit_sha) FROM ai_signals`).Scan(&aiTagged); err != nil {
		return 0, 0, fmt.Errorf("status: count ai_signals: %w", err)
	}
	return total, aiTagged, nil
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
