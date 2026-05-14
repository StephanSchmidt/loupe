package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fullYAML = `
org: acme-eng

git_host:
  provider: bitbucket-cloud
  base_url: https://api.bitbucket.org/2.0
  username: stephan@inkmi.com

tracker:
  provider: jira-cloud
  site: acme.atlassian.net
  email: stephan@inkmi.com

teams:
  - name: platform
    members: [alice@acme.com, bob@acme.com]

ai_adoption:
  cutover_date: 2026-03-15
  detection:
    co_author_trailers: true
    pr_labels: [ai-assisted]
  min_weekly_commits_for_cutover: 0.05

windows:
  baseline_weeks: 8
  comparison_weeks: 8

output:
  path: ./reports
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "loupe.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoad_GitHubProviderDefaultsBaseURLs(t *testing.T) {
	body := `
org: acme-eng

git_host:
  provider: github

tracker:
  provider: github
`
	path := writeConfig(t, body)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GitHost.BaseURL != "https://api.github.com" {
		t.Errorf("git_host.base_url = %q, want default https://api.github.com", c.GitHost.BaseURL)
	}
	if c.Tracker.BaseURL != "https://api.github.com" {
		t.Errorf("tracker.base_url = %q, want default https://api.github.com", c.Tracker.BaseURL)
	}
}

func TestLoad_FullConfigRoundtrip(t *testing.T) {
	path := writeConfig(t, fullYAML)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Org != "acme-eng" {
		t.Errorf("Org = %q, want acme-eng", c.Org)
	}
	if c.GitHost.Provider != ProviderBitbucketCloud {
		t.Errorf("GitHost.Provider = %q, want %q", c.GitHost.Provider, ProviderBitbucketCloud)
	}
	if c.GitHost.Username != "stephan@inkmi.com" {
		t.Errorf("GitHost.Username = %q", c.GitHost.Username)
	}
	if c.Tracker.Provider != ProviderJiraCloud {
		t.Errorf("Tracker.Provider = %q", c.Tracker.Provider)
	}
	if c.Tracker.Site != "acme.atlassian.net" {
		t.Errorf("Tracker.Site = %q", c.Tracker.Site)
	}
	if !c.AIAdoption.Detection.CoAuthorTrailers {
		t.Errorf("Detection.CoAuthorTrailers = false, want true")
	}
}

func TestLoad_AppliesDefaults(t *testing.T) {
	path := writeConfig(t, `
org: minimal
git_host:
  provider: bitbucket-cloud
  username: u
tracker:
  provider: jira-cloud
  site: x.atlassian.net
  email: u@x
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GitHost.BaseURL != defaultBitbucketBaseURL {
		t.Errorf("GitHost.BaseURL default = %q, want %q", c.GitHost.BaseURL, defaultBitbucketBaseURL)
	}
	if c.Windows.BaselineWeeks != 12 {
		t.Errorf("Windows.BaselineWeeks default = %d, want 12", c.Windows.BaselineWeeks)
	}
	if c.AIAdoption.MinWeeklyCommitsForCutover != 0.05 {
		t.Errorf("MinWeeklyCommitsForCutover default = %v, want 0.05", c.AIAdoption.MinWeeklyCommitsForCutover)
	}
	if c.Output.Path != "./reports" {
		t.Errorf("Output.Path default = %q, want ./reports", c.Output.Path)
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing org",
			yaml: "git_host: {provider: bitbucket-cloud, username: u}\ntracker: {provider: jira-cloud, site: s, email: e}\n",
			want: "org is required",
		},
		{
			name: "missing git_host provider",
			yaml: "org: a\ntracker: {provider: jira-cloud, site: s, email: e}\n",
			want: "git_host.provider is required",
		},
		{
			name: "unsupported git_host provider",
			yaml: "org: a\ngit_host: {provider: gitlab-cloud, username: u}\ntracker: {provider: jira-cloud, site: s, email: e}\n",
			want: `git_host.provider "gitlab-cloud" is not supported`,
		},
		{
			name: "missing bitbucket username",
			yaml: "org: a\ngit_host: {provider: bitbucket-cloud}\ntracker: {provider: jira-cloud, site: s, email: e}\n",
			want: "git_host.username is required",
		},
		{
			name: "missing tracker site",
			yaml: "org: a\ngit_host: {provider: bitbucket-cloud, username: u}\ntracker: {provider: jira-cloud, email: e}\n",
			want: "tracker.site is required",
		},
		{
			name: "missing tracker provider",
			yaml: "org: a\ngit_host: {provider: bitbucket-cloud, username: u}\n",
			want: "tracker.provider is required",
		},
		{
			name: "unsupported tracker provider",
			yaml: "org: a\ngit_host: {provider: bitbucket-cloud, username: u}\ntracker: {provider: linear, site: s, email: e}\n",
			want: `tracker.provider "linear" is not supported`,
		},
		{
			name: "bad cutover date",
			yaml: "org: a\ngit_host: {provider: bitbucket-cloud, username: u}\ntracker: {provider: jira-cloud, site: s, email: e}\nai_adoption: {cutover_date: not-a-date}\n",
			want: "ai_adoption.cutover_date",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load: expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
