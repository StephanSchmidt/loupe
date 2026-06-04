package analyze

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/StephanSchmidt/loupe/internal/store"
)

// Landing is one bucket (repo or team) of the "where does AI land" breakdown:
// its commit volume and the share carrying an AI signal.
type Landing struct {
	Name      string
	Commits   int
	AICommits int
	Rate      float64 // AICommits / Commits * 100
}

// TeamSpec is a team's name and member emails, passed in by the caller so
// analyze stays free of an import on the config package.
type TeamSpec struct {
	Name    string
	Members []string
}

type commitAI struct {
	repo  string
	email string
	hasAI bool
}

// loadCommitAI returns one row per commit (deduped by SHA) with its repo,
// author email, and whether it carries any AI signal. sinceUnix > 0 restricts
// to commits at or after that time. Shared by the repo and team breakdowns.
func loadCommitAI(ctx context.Context, s *store.Store, sinceUnix int64) ([]commitAI, error) {
	where := ""
	var args []any
	if sinceUnix > 0 {
		where = " WHERE c.committed_at >= ?"
		args = append(args, sinceUnix)
	}
	rows, err := s.DB().QueryContext(ctx, `
        SELECT c.repo_name, c.author_email,
               MAX(CASE WHEN sig.commit_sha IS NOT NULL THEN 1 ELSE 0 END) AS has_ai
        FROM commits c
        LEFT JOIN ai_signals sig ON sig.commit_sha = c.sha`+where+`
        GROUP BY c.sha, c.repo_name, c.author_email
    `, args...)
	if err != nil {
		return nil, fmt.Errorf("query commit AI: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []commitAI
	for rows.Next() {
		var repo, email string
		var hasAI int
		if err := rows.Scan(&repo, &email, &hasAI); err != nil {
			return nil, fmt.Errorf("scan commit AI: %w", err)
		}
		out = append(out, commitAI{repo: repo, email: email, hasAI: hasAI == 1})
	}
	return out, rows.Err()
}

func rate(commits, ai int) float64 {
	if commits == 0 {
		return 0
	}
	return float64(ai) / float64(commits) * 100
}

// windowCutoff returns the Unix cutoff for the last `months` months, anchored
// on the most recent commit (matching the deck's windowByMonths so a slightly
// stale store still shows its own recent window). months <= 0 → 0 (all time).
func windowCutoff(ctx context.Context, s *store.Store, months int) (int64, error) {
	if months <= 0 {
		return 0, nil
	}
	var maxTS sql.NullInt64
	if err := s.DB().QueryRowContext(ctx, `SELECT MAX(committed_at) FROM commits`).Scan(&maxTS); err != nil {
		return 0, fmt.Errorf("max commit time: %w", err)
	}
	if !maxTS.Valid {
		return 0, nil
	}
	return time.Unix(maxTS.Int64, 0).UTC().AddDate(0, -months, 0).Unix(), nil
}

// landingMinCommits is the floor below which a repo is too small to
// characterise — its AI rate would be noise (a 2-commit repo at 100% is not
// "where AI lands"). 109 repos is too many to plot, so this also trims the
// long tail of near-dormant repos.
const landingMinCommits = 20

// ComputeRepoLanding returns the AI-commit rate per repository, ranked by AI
// rate descending (with a commit floor) and capped at topN — so the repos
// where AI actually concentrates lead. Ranking by volume instead buried
// high-AI/low-volume repos (e.g. an AI-heavy orchestration service) behind huge
// repos that barely use AI, making it look like AI lands nowhere.
func ComputeRepoLanding(ctx context.Context, s *store.Store, topN, months int) ([]Landing, error) {
	since, err := windowCutoff(ctx, s, months)
	if err != nil {
		return nil, err
	}
	commits, err := loadCommitAI(ctx, s, since)
	if err != nil {
		return nil, err
	}
	byRepo := map[string]*Landing{}
	for _, c := range commits {
		l := byRepo[c.repo]
		if l == nil {
			l = &Landing{Name: shortRepo(c.repo)}
			byRepo[c.repo] = l
		}
		l.Commits++
		if c.hasAI {
			l.AICommits++
		}
	}
	all := make([]Landing, 0, len(byRepo))
	for _, l := range byRepo {
		if l.Commits < landingMinCommits {
			continue
		}
		l.Rate = rate(l.Commits, l.AICommits)
		all = append(all, *l)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Rate != all[j].Rate {
			return all[i].Rate > all[j].Rate
		}
		return all[i].Commits > all[j].Commits // tie-break by volume
	})
	if topN > 0 && len(all) > topN {
		all = all[:topN]
	}
	return all, nil
}

// ComputeTeamLanding returns the AI-commit rate per configured team. Authors
// not in any team are skipped (only configured teams are shown). Returns nil
// when no teams are configured.
func ComputeTeamLanding(ctx context.Context, s *store.Store, teams []TeamSpec, months int) ([]Landing, error) {
	if len(teams) == 0 {
		return nil, nil
	}
	email2team := map[string]string{}
	for _, t := range teams {
		for _, m := range t.Members {
			email2team[strings.ToLower(strings.TrimSpace(m))] = t.Name
		}
	}
	since, err := windowCutoff(ctx, s, months)
	if err != nil {
		return nil, err
	}
	commits, err := loadCommitAI(ctx, s, since)
	if err != nil {
		return nil, err
	}
	byTeam := map[string]*Landing{}
	for _, t := range teams {
		byTeam[t.Name] = &Landing{Name: t.Name}
	}
	for _, c := range commits {
		team, ok := email2team[strings.ToLower(strings.TrimSpace(c.email))]
		if !ok {
			continue
		}
		l := byTeam[team]
		l.Commits++
		if c.hasAI {
			l.AICommits++
		}
	}
	out := make([]Landing, 0, len(byTeam))
	for _, t := range teams {
		l := byTeam[t.Name]
		if l.Commits == 0 {
			continue
		}
		l.Rate = rate(l.Commits, l.AICommits)
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rate > out[j].Rate })
	return out, nil
}

// shortRepo trims the "workspace/" prefix off a full repo name for display.
func shortRepo(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 && i < len(full)-1 {
		return full[i+1:]
	}
	return full
}
