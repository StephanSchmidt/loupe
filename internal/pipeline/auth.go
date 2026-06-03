package pipeline

import (
	"fmt"

	"github.com/StephanSchmidt/loupe/internal/auth"
	"github.com/StephanSchmidt/loupe/internal/config"
	"github.com/StephanSchmidt/loupe/internal/githost"
	adoHost "github.com/StephanSchmidt/loupe/internal/githost/azuredevops"
	"github.com/StephanSchmidt/loupe/internal/githost/bitbucket"
	ghHost "github.com/StephanSchmidt/loupe/internal/githost/github"
	glHost "github.com/StephanSchmidt/loupe/internal/githost/gitlab"
	"github.com/StephanSchmidt/loupe/internal/tracker"
	adoTracker "github.com/StephanSchmidt/loupe/internal/tracker/azuredevops"
	ghTracker "github.com/StephanSchmidt/loupe/internal/tracker/github"
	glTracker "github.com/StephanSchmidt/loupe/internal/tracker/gitlab"
	"github.com/StephanSchmidt/loupe/internal/tracker/jira"
	"github.com/StephanSchmidt/loupe/internal/tracker/linear"
)

// GitHostTokenLabel / TrackerTokenLabel return the interactive-prompt label for
// the configured provider. Centralising this keeps the prompt wording
// consistent and makes adding a provider a one-line change.
func GitHostTokenLabel(provider string) string {
	switch provider {
	case config.ProviderBitbucketCloud:
		return "Bitbucket app password"
	case config.ProviderGitHub:
		return "GitHub token (git host)"
	case config.ProviderGitLab:
		return "GitLab token (git host)"
	case config.ProviderAzureDevOps:
		return "Azure DevOps PAT (git host)"
	default:
		return "git host token"
	}
}

func TrackerTokenLabel(provider string) string {
	switch provider {
	case config.ProviderJiraCloud:
		return "Jira API token"
	case config.ProviderGitHub:
		return "GitHub token (tracker)"
	case config.ProviderGitLab:
		return "GitLab token (tracker)"
	case config.ProviderLinear:
		return "Linear API key"
	case config.ProviderAzureDevOps:
		return "Azure DevOps PAT (tracker)"
	default:
		return "tracker token"
	}
}

// SameSinglePATProvider reports whether the configured git host and tracker use
// the same single-PAT provider (github, gitlab, or azuredevops). Same-provider
// configs share a credential and treat --repo == --project.
func SameSinglePATProvider(cfg *config.Config) bool {
	if cfg.GitHost.Provider != cfg.Tracker.Provider {
		return false
	}
	switch cfg.GitHost.Provider {
	case config.ProviderGitHub, config.ProviderGitLab, config.ProviderAzureDevOps:
		return true
	default:
		return false
	}
}

// ResolveTokens fills in the git-host and tracker tokens, prompting
// interactively for any not already supplied (e.g. via the hidden CI flags).
// When the git host and tracker share a single-PAT provider one credential
// covers both, so only one prompt fires and the value is copied across. Pass
// dryRun=true to skip prompting entirely and return the inputs untouched.
func ResolveTokens(cfg *config.Config, gitHostToken, trackerToken string, dryRun bool) (gh, trk string, err error) {
	if dryRun {
		return gitHostToken, trackerToken, nil
	}
	if SameSinglePATProvider(cfg) {
		seed := gitHostToken
		if seed == "" {
			seed = trackerToken
		}
		tok, err := EnsureToken(seed, GitHostTokenLabel(cfg.GitHost.Provider))
		if err != nil {
			return "", "", err
		}
		return tok, tok, nil
	}
	gitHostToken, err = EnsureToken(gitHostToken, GitHostTokenLabel(cfg.GitHost.Provider))
	if err != nil {
		return "", "", err
	}
	trackerToken, err = EnsureToken(trackerToken, TrackerTokenLabel(cfg.Tracker.Provider))
	if err != nil {
		return "", "", err
	}
	return gitHostToken, trackerToken, nil
}

// EnsureToken returns existing if non-empty, otherwise interactively prompts.
// Smoke tests pass tokens via hidden flags so no prompt fires.
func EnsureToken(existing, label string) (string, error) {
	if existing != "" {
		return existing, nil
	}
	tok, err := auth.PromptToken(label)
	if err != nil {
		return "", err
	}
	return tok, nil
}

// BuildGitHost is the explicit registry for git-host providers. Adding a case
// (e.g. ProviderGitLabCloud) is the full plug-in surface.
func BuildGitHost(cfg *config.Config, token, baseURLOverride string) (githost.GitHost, error) {
	base := cfg.GitHost.BaseURL
	if baseURLOverride != "" {
		base = baseURLOverride
	}
	switch cfg.GitHost.Provider {
	case config.ProviderBitbucketCloud:
		// org is the Bitbucket workspace: scoping the client to it lets
		// ListWorkspaces skip /2.0/workspaces, so a single Atlassian API
		// token works (that endpoint rejects API tokens).
		return bitbucket.New(base, cfg.GitHost.Username, token, cfg.Org)
	case config.ProviderGitHub:
		return ghHost.New(base, token)
	case config.ProviderGitLab:
		return glHost.New(base, token)
	case config.ProviderAzureDevOps:
		// git_host.username carries the Azure organization name.
		return adoHost.New(base, cfg.GitHost.Username, token)
	default:
		return nil, fmt.Errorf("unsupported git_host.provider %q", cfg.GitHost.Provider)
	}
}

func BuildTracker(cfg *config.Config, token, baseURLOverride string) (tracker.Tracker, error) {
	switch cfg.Tracker.Provider {
	case config.ProviderJiraCloud:
		if baseURLOverride != "" {
			return jira.NewWithBaseURL(baseURLOverride, cfg.Tracker.Email, token)
		}
		if cfg.Tracker.BaseURL != "" {
			return jira.NewWithBaseURL(cfg.Tracker.BaseURL, cfg.Tracker.Email, token)
		}
		return jira.New(cfg.Tracker.Site, cfg.Tracker.Email, token)
	case config.ProviderGitHub:
		base := cfg.Tracker.BaseURL
		if baseURLOverride != "" {
			base = baseURLOverride
		}
		return ghTracker.New(base, token)
	case config.ProviderGitLab:
		base := cfg.Tracker.BaseURL
		if baseURLOverride != "" {
			base = baseURLOverride
		}
		return glTracker.New(base, token)
	case config.ProviderLinear:
		base := cfg.Tracker.BaseURL
		if baseURLOverride != "" {
			base = baseURLOverride
		}
		return linear.New(base, token)
	case config.ProviderAzureDevOps:
		base := cfg.Tracker.BaseURL
		if baseURLOverride != "" {
			base = baseURLOverride
		}
		// tracker.site carries the Azure organization; fall back to
		// git_host.username when the git host is also Azure DevOps.
		org := cfg.Tracker.Site
		if org == "" {
			org = cfg.GitHost.Username
		}
		return adoTracker.New(base, org, token)
	default:
		return nil, fmt.Errorf("unsupported tracker.provider %q", cfg.Tracker.Provider)
	}
}
