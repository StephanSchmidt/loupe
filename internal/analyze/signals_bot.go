package analyze

import "strings"

// aiBotRule matches one author identity to an AI source. Both fields are
// optional but at least one must be non-empty; matching is case-insensitive
// with substring semantics on the email and prefix semantics on the name.
type aiBotRule struct {
	source string
	// emailContains matches a substring of the lower-cased email.
	// GitHub bot emails follow `NUMBER+name[bot]@users.noreply.github.com`,
	// so substrings like "+copilot[bot]@" or "+devin-ai-integration[bot]@"
	// pin the rule to that specific bot without colliding on names that
	// happen to contain the substring.
	emailContains string
	// nameContains matches a substring of the lower-cased author name.
	// Useful when the email is generic (some integrations use a shared
	// noreply address but distinguish via the name).
	nameContains string
}

// aiBotRules lists the bot-author identities that should be counted as AI.
//
// The ordering matters when two rules could match: keep the most specific
// (e.g. "copilot-swe-agent" before the bare "copilot") earlier in the list
// so it wins the lookup. None of the current rules overlap, but new
// entries should preserve that property.
var aiBotRules = []aiBotRule{
	{source: SourceCopilot, emailContains: "+copilot-swe-agent[bot]@", nameContains: "copilot-swe-agent"},
	{source: SourceCopilot, emailContains: "+copilot[bot]@", nameContains: "copilot[bot]"},
	{source: SourceDevin, emailContains: "+devin-ai-integration[bot]@", nameContains: "devin-ai-integration"},
	{source: SourceDevin, emailContains: "+devin-ai[bot]@", nameContains: "devin-ai["},
	{source: SourceGemini, emailContains: "+gemini-code-assist[bot]@", nameContains: "gemini-code-assist"},
	{source: SourceJules, emailContains: "+google-labs-jules[bot]@", nameContains: "google-labs-jules"},
	{source: SourceJules, emailContains: "+jules[bot]@", nameContains: "jules[bot]"},
}

// IsAIBot reports whether the author identity belongs to a known AI bot
// account (Copilot Coding Agent, Devin, Gemini Code Assist, Jules, …) and
// returns the AI source for it. Returns ("", false) for human authors and
// for non-AI bots like dependabot.
//
// Callers that filter bots out of analytics totals (see weekly.go,
// cmdstatus) should use this to keep AI-bot commits in the count, while
// IsBot continues to exclude general automation.
func IsAIBot(email, name string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(email))
	n := strings.ToLower(strings.TrimSpace(name))
	for _, r := range aiBotRules {
		if r.emailContains != "" && strings.Contains(e, r.emailContains) {
			return r.source, true
		}
		if r.nameContains != "" && strings.Contains(n, r.nameContains) {
			return r.source, true
		}
	}
	return "", false
}

// detectFromAuthorIdentity emits a bot_author signal for commits whose
// author matches a known AI-bot account. The returned signal carries
// high confidence — the email and name patterns are specific enough to
// be unambiguous (substrings are pinned to GitHub's `[bot]@` suffix).
//
// Unknown bot accounts (those that look like GitHub bots but don't match
// a known AI tool) are reported as SourceUnknownAIBot with medium
// confidence so the deck can surface "we saw N bot-authored commits we
// can't classify".
func detectFromAuthorIdentity(email, name string) (Signal, bool) {
	if source, ok := IsAIBot(email, name); ok {
		return Signal{
			Kind:       KindBotAuthor,
			Source:     source,
			Confidence: ConfidenceHigh,
			Detail:     name + " <" + email + ">",
		}, true
	}
	if looksLikeGitHubBot(email, name) {
		return Signal{
			Kind:       KindBotAuthor,
			Source:     SourceUnknownAIBot,
			Confidence: ConfidenceMedium,
			Detail:     name + " <" + email + ">",
		}, true
	}
	return Signal{}, false
}

// looksLikeGitHubBot matches the generic GitHub App identity pattern
// without claiming a specific tool. Used to surface unknown bot
// authors without misattributing them to a known AI source.
//
// Excludes well-known non-AI bots (dependabot, renovate, …) via the
// existing IsBot/botRules curated list, so this only fires for unknown
// `*[bot]` identities.
func looksLikeGitHubBot(email, name string) bool {
	if !IsBot(email, name) {
		return false
	}
	// Known non-AI bots fall through botDisplayLookup; if it returns a
	// display name, the bot is already curated as non-AI and we don't
	// want to flag it as an unknown AI bot.
	if botDisplayLookup(email, name) != "" {
		return false
	}
	// Bot-shaped without a curated rule: either the name carries the
	// `[bot]` suffix or the email matches GitHub's App-bot regex (used
	// for PR rows where author name is empty / commit rows where the
	// author identity is set by a GitHub App not yet on our list).
	return hasBotSuffix(name) || loginFromGitHubBotEmail(strings.ToLower(strings.TrimSpace(email))) != ""
}
