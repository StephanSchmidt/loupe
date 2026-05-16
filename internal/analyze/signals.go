package analyze

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/StephanSchmidt/loupe/internal/store"
)

const (
	SourceClaude   = "claude"
	SourceAider    = "aider"
	SourceCopilot  = "copilot"
	SourceCursor   = "cursor"
	SourceDevin    = "devin"
	SourceGemini   = "gemini-code-assist"
	SourceJules    = "jules"
	SourceOpenCode = "opencode"
	// SourceUnknownAIBot is the fallback Source for an author identity that
	// matches the generic `*[bot]@users.noreply.github.com` pattern without
	// matching one of the known AI-bot names.
	SourceUnknownAIBot = "unknown-ai-bot"
	// SourceAIGeneric labels a PR-level signal (label or branch) that
	// indicates AI involvement without naming a specific tool — e.g. a
	// label named "ai-generated" or "ai-assist".
	SourceAIGeneric = "ai"

	KindCoAuthorTrailer = "co_author_trailer"
	KindBodyFooter      = "body_footer"
	KindBotAuthor       = "bot_author"
	KindPRLabel         = "pr_label"
	KindBranchName      = "branch_name"
	KindSquashRecovery  = "squash_recovery"
	KindSeatInference   = "seat_inference"

	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// trailerRe matches a single Co-Authored-By trailer line.
//
// The name capture excludes `<` so a malformed "Name <broken-email" line
// can't slip through into the name and accidentally classify on a tool
// substring. The email group is optional — some tools (e.g. Aider via the
// `--no-trailer-email` mode in third-party configs) write the trailer
// without an address.
//
// Anchored to the line (multiline mode) so trailer-shaped prose inside a
// body paragraph doesn't trigger a false positive.
var trailerRe = regexp.MustCompile(`(?im)^\s*Co-Authored-By:\s*([^<\n]+?)(?:\s*<\s*([^>]+)\s*>)?\s*$`)

type Signal struct {
	CommitSHA  string
	Kind       string
	Source     string
	Confidence string
	Detail     string
}

// DetectFromMessage extracts AI signals from a single commit message via
// the Co-Authored-By trailer. The same (commit, source) pair is reported
// at most once, even if multiple matching trailers exist — duplicates
// would just collide in the upsert.
func DetectFromMessage(message string) []Signal {
	matches := trailerRe.FindAllStringSubmatch(message, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]Signal, 0, len(matches))
	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		email := strings.ToLower(strings.TrimSpace(m[2]))
		source := classifyTrailer(name, email)
		if source == "" {
			continue
		}
		if _, dup := seen[source]; dup {
			continue
		}
		seen[source] = struct{}{}
		detail := name
		if email != "" {
			detail = fmt.Sprintf("%s <%s>", name, email)
		}
		out = append(out, Signal{
			Kind:       KindCoAuthorTrailer,
			Source:     source,
			Confidence: ConfidenceHigh,
			Detail:     detail,
		})
	}
	return out
}

// trailerRule maps a single name/email substring pair to an AI source.
// Order in trailerRules matters when two tokens could collide (e.g.
// "gemini-code-assist" must win over a future plain "gemini" entry);
// keep the most specific tokens earlier.
type trailerRule struct {
	source        string
	nameTokens    []string
	emailTokens   []string
	emailPrefixes []string
}

var trailerRules = []trailerRule{
	{source: SourceClaude, nameTokens: []string{"claude"}, emailTokens: []string{"anthropic.com"}},
	{source: SourceAider, nameTokens: []string{"aider"}, emailPrefixes: []string{"aider"}},
	{source: SourceCopilot, nameTokens: []string{"copilot"}, emailTokens: []string{"copilot"}},
	{source: SourceCursor, nameTokens: []string{"cursor"}, emailTokens: []string{"cursor"}},
	{source: SourceDevin,
		nameTokens:  []string{"devin-ai-integration", "devin-ai"},
		emailTokens: []string{"devin-ai"}},
	{source: SourceGemini,
		nameTokens:  []string{"gemini-code-assist"},
		emailTokens: []string{"gemini-code-assist"}},
	{source: SourceJules,
		nameTokens:  []string{"google-labs-jules", "jules[bot]"},
		emailTokens: []string{"google-labs-jules"}},
	{source: SourceOpenCode, nameTokens: []string{"opencode"}, emailTokens: []string{"opencode"}},
}

func classifyTrailer(name, email string) string {
	lname := strings.ToLower(name)
	for _, r := range trailerRules {
		if matchAny(lname, r.nameTokens, strings.Contains) ||
			matchAny(email, r.emailTokens, strings.Contains) ||
			matchAny(email, r.emailPrefixes, strings.HasPrefix) {
			return r.source
		}
	}
	return ""
}

func matchAny(s string, needles []string, fn func(string, string) bool) bool {
	for _, n := range needles {
		if fn(s, n) {
			return true
		}
	}
	return false
}

const upsertSignalSQL = `
INSERT INTO ai_signals (commit_sha, signal_kind, signal_source, confidence, detail)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(commit_sha, signal_kind, signal_source) DO UPDATE SET
    confidence = excluded.confidence,
    detail     = excluded.detail
`

// DetectAndStore scans every commit in the store, extracts AI signals via
// every active commit-level detector (trailer, body-footer, bot-author),
// and upserts them into ai_signals. Idempotent. Returns the total number
// of signal rows written across all detectors.
func DetectAndStore(ctx context.Context, s *store.Store) (int, error) {
	// SetMaxOpenConns(1) means we can't iterate rows AND open a tx on the
	// same connection. Read all commits into memory first, then write.
	rows, err := s.DB().QueryContext(ctx, `SELECT sha, author_email, author_name, message FROM commits`)
	if err != nil {
		return 0, fmt.Errorf("query commits: %w", err)
	}
	type row struct {
		sha, email, name, msg string
	}
	var commits []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.sha, &r.email, &r.name, &r.msg); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan commit row: %w", err)
		}
		commits = append(commits, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate commits: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close rows: %w", err)
	}

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, upsertSignalSQL)
	if err != nil {
		return 0, fmt.Errorf("prepare upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	count := 0
	for _, c := range commits {
		sigs := DetectFromMessage(c.msg)
		sigs = append(sigs, detectFromBodyFooters(c.msg)...)
		if sig, ok := detectFromAuthorIdentity(c.email, c.name); ok {
			sigs = append(sigs, sig)
		}
		for _, sig := range sigs {
			if _, err := stmt.ExecContext(ctx, c.sha, sig.Kind, sig.Source, sig.Confidence, sig.Detail); err != nil {
				return count, fmt.Errorf("upsert signal for %s: %w", c.sha, err)
			}
			count++
		}
	}
	if err := tx.Commit(); err != nil {
		return count, fmt.Errorf("commit tx: %w", err)
	}
	return count, nil
}
