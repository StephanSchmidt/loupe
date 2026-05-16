package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultMinWeeklyAICommitsForCutover = 0.05
	defaultBaselineWeeks                = 12
	defaultComparisonWeeks              = 12
	defaultOutputPath                   = "./reports"
	defaultBitbucketBaseURL             = "https://api.bitbucket.org/2.0"
	defaultGitHubBaseURL                = "https://api.github.com"

	ProviderBitbucketCloud = "bitbucket-cloud"
	ProviderJiraCloud      = "jira-cloud"
	ProviderGitHub         = "github"
)

// supportedGitHostProviders lists every provider string accepted in
// git_host.provider. Adding GitLab here + a case in the registry
// (cmd/cmdbaseline.buildGitHost) is the full plug-in surface.
var supportedGitHostProviders = []string{ProviderBitbucketCloud, ProviderGitHub}

// supportedTrackerProviders lists every provider string accepted in
// tracker.provider. GitHub can play either role: as a tracker it ingests
// issues from the same repos that the git host enumerates.
var supportedTrackerProviders = []string{ProviderJiraCloud, ProviderGitHub}

type Config struct {
	Org        string           `yaml:"org"`
	GitHost    GitHostConfig    `yaml:"git_host"`
	Tracker    TrackerConfig    `yaml:"tracker"`
	Teams      []TeamConfig     `yaml:"teams"`
	AIAdoption AIAdoptionConfig `yaml:"ai_adoption"`
	CycleTime  CycleTimeConfig  `yaml:"cycle_time"`
	Windows    WindowsConfig    `yaml:"windows"`
	Output     OutputConfig     `yaml:"output"`
}

// CycleTimeConfig drives the idea→dev→release cycle-time charts. The
// status names are matched case-insensitively against tracker
// transitions; falling back to the first commit on a ticket when no
// matching transition is found.
type CycleTimeConfig struct {
	DevStartedStatuses []string `yaml:"dev_started_statuses"`
}

// GitHostConfig holds non-secret coordinates for the git host. The token is
// prompted (or passed via the hidden --bitbucket-token flag), never stored.
type GitHostConfig struct {
	Provider string `yaml:"provider"`
	BaseURL  string `yaml:"base_url"`
	Username string `yaml:"username"`
}

// TrackerConfig holds non-secret coordinates for the issue tracker.
// Site/Email are Jira-specific; BaseURL is used by GitHub (and lets Jira
// callers override the composed URL for tests).
type TrackerConfig struct {
	Provider string `yaml:"provider"`
	Site     string `yaml:"site,omitempty"`
	Email    string `yaml:"email,omitempty"`
	BaseURL  string `yaml:"base_url,omitempty"`
}

type TeamConfig struct {
	Name    string   `yaml:"name"`
	Members []string `yaml:"members"`
}

type AIAdoptionConfig struct {
	CutoverDate string          `yaml:"cutover_date,omitempty"`
	Detection   DetectionConfig `yaml:"detection"`
	// Pointer so an explicit `0` in YAML (meaning "always meet the
	// threshold") is preserved instead of being replaced by the default.
	MinWeeklyCommitsForCutover *float64 `yaml:"min_weekly_commits_for_cutover,omitempty"`
}

type DetectionConfig struct {
	// CoAuthorTrailers / BodyFooters / BotAuthors are always on at the
	// detector level; the bools exist so future detectors can be opt-in
	// without breaking config compatibility. (Today these flags are
	// informational only.)
	CoAuthorTrailers bool `yaml:"co_author_trailers"`
	BodyFooters      bool `yaml:"body_footers"`
	BotAuthors       bool `yaml:"bot_authors"`

	// PRLabels lists the case-insensitive label strings that count as an
	// AI signal. A label whose name contains a known tool ("claude",
	// "copilot", …) gets attributed to that tool; everything else is
	// attributed to a generic "ai" source.
	PRLabels []string `yaml:"pr_labels"`

	// BranchPrefixes lists the case-insensitive branch-name prefixes
	// (typically "tool/") that count as an AI signal.
	BranchPrefixes []string `yaml:"branch_prefixes"`

	// SquashMergeRecovery enables fetching pre-squash PR commits and
	// running the trailer/footer/bot detectors on them. Default true —
	// the API cost is one extra request per PR and the recall lift on
	// squash-merged PRs is large.
	SquashMergeRecovery *bool `yaml:"squash_merge_recovery,omitempty"`

	// SeatInference enables propagating direct AI evidence to the same
	// author's other commits in the same week and repo. Off by default
	// because it's the only detector that infers rather than observes —
	// users should turn it on knowingly.
	SeatInference bool `yaml:"seat_inference"`
}

type WindowsConfig struct {
	BaselineWeeks   int `yaml:"baseline_weeks"`
	ComparisonWeeks int `yaml:"comparison_weeks"`
}

type OutputConfig struct {
	Path string `yaml:"path"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- config path is user-supplied by design
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.AIAdoption.MinWeeklyCommitsForCutover == nil {
		v := defaultMinWeeklyAICommitsForCutover
		c.AIAdoption.MinWeeklyCommitsForCutover = &v
	}
	if c.AIAdoption.Detection.SquashMergeRecovery == nil {
		t := true
		c.AIAdoption.Detection.SquashMergeRecovery = &t
	}
	if len(c.CycleTime.DevStartedStatuses) == 0 {
		c.CycleTime.DevStartedStatuses = []string{
			"In Progress", "In Development", "In Review", "Code Review",
		}
	}
	if c.Windows.BaselineWeeks == 0 {
		c.Windows.BaselineWeeks = defaultBaselineWeeks
	}
	if c.Windows.ComparisonWeeks == 0 {
		c.Windows.ComparisonWeeks = defaultComparisonWeeks
	}
	if c.Output.Path == "" {
		c.Output.Path = defaultOutputPath
	}
	if c.GitHost.Provider == ProviderBitbucketCloud && c.GitHost.BaseURL == "" {
		c.GitHost.BaseURL = defaultBitbucketBaseURL
	}
	if c.GitHost.Provider == ProviderGitHub && c.GitHost.BaseURL == "" {
		c.GitHost.BaseURL = defaultGitHubBaseURL
	}
	if c.Tracker.Provider == ProviderGitHub && c.Tracker.BaseURL == "" {
		c.Tracker.BaseURL = defaultGitHubBaseURL
	}
}

func (c *Config) Validate() error {
	var errs []string
	if c.Org == "" {
		errs = append(errs, "org is required")
	}

	switch c.GitHost.Provider {
	case "":
		errs = append(errs, "git_host.provider is required (e.g. \"bitbucket-cloud\")")
	case ProviderBitbucketCloud:
		if c.GitHost.Username == "" {
			errs = append(errs, "git_host.username is required for bitbucket-cloud")
		}
	case ProviderGitHub:
		// No required fields beyond Provider — the authed user's login is
		// fetched at runtime via /user.
	default:
		errs = append(errs, fmt.Sprintf("git_host.provider %q is not supported (allowed: %s)",
			c.GitHost.Provider, strings.Join(supportedGitHostProviders, ", ")))
	}

	switch c.Tracker.Provider {
	case "":
		errs = append(errs, "tracker.provider is required (e.g. \"jira-cloud\")")
	case ProviderJiraCloud:
		if c.Tracker.Site == "" {
			errs = append(errs, "tracker.site is required for jira-cloud")
		}
		if c.Tracker.Email == "" {
			errs = append(errs, "tracker.email is required for jira-cloud")
		}
	case ProviderGitHub:
		// No required fields beyond Provider.
	default:
		errs = append(errs, fmt.Sprintf("tracker.provider %q is not supported (allowed: %s)",
			c.Tracker.Provider, strings.Join(supportedTrackerProviders, ", ")))
	}

	if c.AIAdoption.CutoverDate != "" {
		if _, err := ParseCutoverDate(c.AIAdoption.CutoverDate); err != nil {
			errs = append(errs, fmt.Sprintf("ai_adoption.cutover_date %q: %v", c.AIAdoption.CutoverDate, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ParseCutoverDate parses a YYYY-MM-DD config value as a UTC midnight.
// Note for users east/west of UTC: the cutover date is interpreted in UTC
// before being bucketed to its ISO-week (Mon 00:00 UTC). The edge case to
// know about: if your local calendar date is a Monday, set the same date
// here; if it's a Sunday or any other day, ISO-week bucketing puts the
// cutover at the start of the same ISO week regardless of timezone.
func ParseCutoverDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}
