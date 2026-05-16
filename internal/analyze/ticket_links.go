package analyze

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/StephanSchmidt/loupe/internal/store"
)

const JoinMethodCommitMsg = "commit_msg"

// jiraKeyRe matches Jira-style ticket keys (`ENG-123`, `MY_TEAM-7`). The
// project prefix is uppercase ASCII (Jira's own constraint) so lowercase
// fragments like `v1-23` don't false-positive. Word boundaries on each end
// stop `ABC-123` from matching inside `XABC-1234`.
var jiraKeyRe = regexp.MustCompile(`\b([A-Z][A-Z0-9_]+)-(\d+)\b`)

// ghIssueRe matches GitHub-style issue references in commit messages. The
// optional `owner/repo` prefix lets a commit in one repo link to an issue
// in another. The trailing `\b` keeps `#100` from matching `#1000`.
var ghIssueRe = regexp.MustCompile(`(?:([\w.\-]+/[\w.\-]+))?#(\d+)\b`)

// LinkCommitsToTickets scans every commit message for ticket-key
// references, intersects them with the local tickets table, and upserts
// rows into ticket_commits. Idempotent: the table's (ticket_id,
// commit_sha) primary key dedupes re-runs.
//
// Returns the number of (ticket, commit) pairs written or refreshed.
func LinkCommitsToTickets(ctx context.Context, s *store.Store) (int, error) {
	keys, err := loadTicketIDs(ctx, s.DB())
	if err != nil {
		return 0, err
	}
	if len(keys) == 0 {
		return 0, nil
	}

	commits, err := loadCommitsForLinking(ctx, s.DB())
	if err != nil {
		return 0, err
	}

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO ticket_commits (ticket_id, commit_sha, join_method)
        VALUES (?, ?, ?)
        ON CONFLICT(ticket_id, commit_sha) DO UPDATE SET
            join_method = excluded.join_method
    `)
	if err != nil {
		return 0, fmt.Errorf("prepare upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	count := 0
	seen := make(map[string]struct{})
	for _, c := range commits {
		for _, id := range extractTicketIDs(c.message, c.repoName, keys) {
			pair := id + "|" + c.sha
			if _, dup := seen[pair]; dup {
				continue
			}
			seen[pair] = struct{}{}
			if _, err := stmt.ExecContext(ctx, id, c.sha, JoinMethodCommitMsg); err != nil {
				return count, fmt.Errorf("upsert ticket_commit %s/%s: %w", id, c.sha, err)
			}
			count++
		}
	}
	if err := tx.Commit(); err != nil {
		return count, fmt.Errorf("commit tx: %w", err)
	}
	return count, nil
}

// extractTicketIDs returns the set of ticket IDs referenced in msg that
// also exist in the known set. repoName is the commit's repo (used to
// resolve bare `#N` references against the GitHub `owner/repo#N` form).
func extractTicketIDs(msg, repoName string, known map[string]struct{}) []string {
	var out []string
	seen := make(map[string]struct{})

	for _, m := range jiraKeyRe.FindAllString(msg, -1) {
		if _, ok := known[m]; !ok {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}

	for _, m := range ghIssueRe.FindAllStringSubmatch(msg, -1) {
		owner := m[1]
		number := m[2]
		if owner == "" {
			owner = repoName
		}
		if owner == "" {
			continue
		}
		id := owner + "#" + number
		if _, ok := known[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// loadTicketIDs returns the set of ticket IDs (as stored in the tickets
// table) so commit-message regex matches can be intersected against
// actual tickets rather than blindly emitting links.
func loadTicketIDs(ctx context.Context, db *sql.DB) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM tickets`)
	if err != nil {
		return nil, fmt.Errorf("load ticket ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan ticket id: %w", err)
		}
		if strings.TrimSpace(id) == "" {
			continue
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

type commitForLinking struct {
	sha      string
	repoName string
	message  string
}

func loadCommitsForLinking(ctx context.Context, db *sql.DB) ([]commitForLinking, error) {
	rows, err := db.QueryContext(ctx, `SELECT sha, repo_name, message FROM commits`)
	if err != nil {
		return nil, fmt.Errorf("query commits: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []commitForLinking
	for rows.Next() {
		var c commitForLinking
		if err := rows.Scan(&c.sha, &c.repoName, &c.message); err != nil {
			return nil, fmt.Errorf("scan commit: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
