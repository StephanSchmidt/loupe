package analyze

import (
	"regexp"
	"strings"
)

// IsBot reports whether a commit author is an automated bot rather than
// a human contributor. Used to exclude bot-authored commits from every
// analytics read path — the rows stay in sqlite, they just don't count.
//
// We err on the side of false negatives: an "Alice Bottega" being counted
// as human is far better than her being filtered out. So the detection
// requires either the exact GitHub `[bot]` suffix convention or a match
// against a small list of known automation identities.
func IsBot(email, name string) bool {
	if botDisplayLookup(email, name) != "" || hasBotSuffix(name) {
		return true
	}
	// Catch GitHub App bots not (yet) in the curated list. Without this an
	// `acme-autofix[bot]@users.noreply.github.com` commit with an empty
	// author name would slip through the curated + suffix checks.
	return loginFromGitHubBotEmail(strings.ToLower(strings.TrimSpace(email))) != ""
}

// BotDisplayName returns the canonical display label for an automated
// author — "Dependabot" instead of `49699333+dependabot[bot]@...`. Falls
// back to the author name with the trailing `[bot]` stripped (Title-Cased)
// for unrecognised bots, and to the raw email if there's no name at all.
//
// Callers should only invoke this when IsBot reports true; passing a
// human author returns the unmodified name.
func BotDisplayName(email, name string) string {
	if d := botDisplayLookup(email, name); d != "" {
		return d
	}
	// `Some-Service[bot]` → `Some-Service`.
	if i := strings.LastIndex(strings.ToLower(name), "[bot]"); i >= 0 {
		stripped := strings.TrimSpace(name[:i])
		if stripped != "" {
			return stripped
		}
	}
	// PR rows and commits with empty author names: derive the login from
	// the noreply email when it matches the GitHub App shape.
	if login := loginFromGitHubBotEmail(strings.ToLower(strings.TrimSpace(email))); login != "" {
		return login
	}
	if name != "" {
		return name
	}
	return email
}

func hasBotSuffix(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), "[bot]")
}

// botDisplayLookup returns the display name for the first matching rule,
// or "" when nothing matches. Order in botRules matters only when two
// substrings could collide — currently they don't, but keep the list
// stable when adding entries.
//
// Intentionally curated-only: signals_bot.looksLikeGitHubBot relies on a
// "" return for unknown-but-bot-shaped identities to flag them as
// SourceUnknownAIBot. The generic email-regex fallback lives in IsBot /
// BotDisplayName instead.
func botDisplayLookup(email, name string) string {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "noreply@github.com" {
		return "GitHub"
	}
	for _, r := range botRules {
		if strings.Contains(e, r.emailSubstring) {
			return r.displayName
		}
	}
	return ""
}

// ghBotEmail matches GitHub's generic App-bot email shape, e.g.
// `49699333+dependabot[bot]@users.noreply.github.com`. The capture group
// is the login (the part before `[bot]`).
var ghBotEmail = regexp.MustCompile(`^\d+\+([^@]+)\[bot\]@users\.noreply\.github\.com$`)

func loginFromGitHubBotEmail(lowerEmail string) string {
	m := ghBotEmail.FindStringSubmatch(lowerEmail)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// botRules is the curated mapping of automation identities to their
// display labels. Each entry is a commitment to filter every commit whose
// email contains the substring — keep narrow, add only when you've
// confirmed the identity is genuinely automated.
type botRule struct {
	emailSubstring string
	displayName    string
}

var botRules = []botRule{
	// Dependency-update bots.
	{"dependabot", "Dependabot"},
	{"renovate", "Renovate"},
	{"release-please", "release-please"},
	{"semantic-release-bot", "semantic-release"},
	// CI / forge automation.
	{"github-actions", "GitHub Actions"},
	{"mergify", "Mergify"},
	{"pre-commit-ci", "pre-commit.ci"},
	// Security / SCA bots that auto-open fix PRs.
	{"aikido-autofix", "Aikido"},
	{"snyk-bot", "Snyk"},
	{"mend-for-github", "Mend"},
	{"whitesource", "WhiteSource"},
	{"gitguardian", "GitGuardian"},
	{"socket-security", "Socket"},
	{"sonatype-lift", "Sonatype Lift"},
	{"step-security", "StepSecurity"},
	{"stepsecurity-app", "StepSecurity"},
	// Code-quality / review bots.
	{"codecov", "Codecov"},
	{"sonarcloud", "SonarCloud"},
	{"sonarqubecloud", "SonarCloud"},
	{"codacy-production", "Codacy"},
	{"codacy-bot", "Codacy"},
	{"deepsource", "DeepSource"},
	{"lgtm-com", "LGTM"},
	{"semgrep-app", "Semgrep"},
	{"semgrep-bot", "Semgrep"},
	{"coderabbitai", "CodeRabbit"},
	{"sourcery-ai", "Sourcery"},
	{"copilot-pull-request-reviewer", "Copilot PR Reviewer"},
	// Misc.
	{"sweep-ai", "Sweep"},
	{"restyled-io", "Restyled"},
	{"imgbot", "ImgBot"},
	{"allcontributors", "AllContributors"},
}
