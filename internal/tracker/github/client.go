// Package github is the v0 implementation of tracker.Tracker backed by
// GitHub Issues. Each GitHub repo becomes a tracker.Project, keyed by
// "owner/repo". Pull requests are filtered out of the issues list (the
// GitHub Issues API returns both, distinguished by a non-nil pull_request
// field).
//
// Auth is bearer-token (PAT or fine-grained PAT). GitHub Enterprise Server
// is not supported in v0 — see internal/githost/github.
package github

import (
	"context"
	"fmt"
	"iter"
	"net/url"
	"strings"
	"time"

	"github.com/StephanSchmidt/loupe/internal/apiclient"
	"github.com/StephanSchmidt/loupe/internal/tracker"
)

const (
	Provider       = "github"
	DefaultBaseURL = "https://api.github.com"
)

// Client implements tracker.Tracker against GitHub Issues.
type Client struct {
	api *apiclient.Client
}

var _ tracker.Tracker = (*Client)(nil)

// New returns a Client. baseURL is typically https://api.github.com.
func New(baseURL, token string) (tracker.Tracker, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("github tracker: baseURL is required")
	}
	if token == "" {
		return nil, fmt.Errorf("github tracker: token is required")
	}
	return &Client{
		api: apiclient.New(baseURL,
			apiclient.WithAuth(apiclient.BearerToken(token)),
			apiclient.WithHeader("Accept", "application/vnd.github+json"),
			apiclient.WithHeader("User-Agent", "loupe/dev"),
			apiclient.WithHeader("X-GitHub-Api-Version", "2022-11-28"),
			apiclient.WithProviderName(Provider),
		),
	}, nil
}

func (c *Client) Name() string { return Provider }

// --- wire structs ---

type wireOrg struct {
	Login string `json:"login"`
}

type wireRepo struct {
	Name      string `json:"name"`
	FullName  string `json:"full_name"`
	HasIssues bool   `json:"has_issues"`
	Archived  bool   `json:"archived"`
	Disabled  bool   `json:"disabled"`
}

type wireIssue struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	PullRequest *struct{}  `json:"pull_request"` // non-nil → this row is a PR, not an issue
	Assignee    *struct {
		Login string `json:"login"`
		Email string `json:"email"`
	} `json:"assignee"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// --- ListProjects ---

// ListProjects returns every repo the authed user can see whose Issues
// feature is enabled. Archived / disabled repos are skipped. ProjectKey is
// the repo's "owner/repo" full name.
func (c *Client) ListProjects(ctx context.Context) ([]tracker.Project, error) {
	authedLogin, err := c.getAuthedLogin(ctx)
	if err != nil {
		return nil, fmt.Errorf("get authed user: %w", err)
	}

	var projects []tracker.Project

	userRepos, err := c.listAuthedUserRepos(ctx, authedLogin)
	if err != nil {
		return nil, err
	}
	for _, r := range userRepos {
		if repoIssuesActive(r) {
			projects = append(projects, repoToProject(r))
		}
	}

	orgs, err := c.listUserOrgs(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range orgs {
		orgRepos, err := c.listOrgRepos(ctx, o.Login)
		if err != nil {
			return nil, err
		}
		for _, r := range orgRepos {
			if repoIssuesActive(r) {
				projects = append(projects, repoToProject(r))
			}
		}
	}
	return projects, nil
}

func repoIssuesActive(r wireRepo) bool {
	return r.HasIssues && !r.Archived && !r.Disabled
}

func repoToProject(r wireRepo) tracker.Project {
	return tracker.Project{Key: r.FullName, Name: r.FullName}
}

func (c *Client) getAuthedLogin(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	resp, err := c.api.Do(ctx, "GET", "/user", "", nil)
	if err != nil {
		return "", err
	}
	if err := apiclient.DecodeJSON(resp, &u); err != nil {
		return "", err
	}
	return u.Login, nil
}

func (c *Client) listAuthedUserRepos(ctx context.Context, ownerLogin string) ([]wireRepo, error) {
	var out []wireRepo
	next := "/user/repos"
	rawQuery := "affiliation=owner&per_page=100"
	for next != "" {
		var page []wireRepo
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, fmt.Errorf("list user repos: %w", err)
		}
		for _, r := range page {
			// /user/repos with affiliation=owner can still surface repos
			// where the user is owner via an org. Filter to repos whose
			// full_name starts with "<login>/".
			if !strings.HasPrefix(r.FullName, ownerLogin+"/") {
				continue
			}
			out = append(out, r)
		}
		next, rawQuery = splitNextURL(nextURL)
	}
	return out, nil
}

func (c *Client) listOrgRepos(ctx context.Context, org string) ([]wireRepo, error) {
	var out []wireRepo
	next := "/orgs/" + url.PathEscape(org) + "/repos"
	rawQuery := "type=all&per_page=100"
	for next != "" {
		var page []wireRepo
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, fmt.Errorf("list org %s repos: %w", org, err)
		}
		out = append(out, page...)
		next, rawQuery = splitNextURL(nextURL)
	}
	return out, nil
}

func (c *Client) listUserOrgs(ctx context.Context) ([]wireOrg, error) {
	var out []wireOrg
	next := "/user/orgs"
	rawQuery := "per_page=100"
	for next != "" {
		var page []wireOrg
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, fmt.Errorf("list user orgs: %w", err)
		}
		out = append(out, page...)
		next, rawQuery = splitNextURL(nextURL)
	}
	return out, nil
}

// --- ListIssues ---

// ListIssues streams issues for projectKey ("owner/repo") updated after
// since (zero means everything). GitHub's /issues endpoint returns both
// issues and pull requests; the pull_request field discriminates the two
// and PR rows are dropped.
func (c *Client) ListIssues(ctx context.Context, projectKey string, since time.Time) iter.Seq2[tracker.Issue, error] {
	return func(yield func(tracker.Issue, error) bool) {
		owner, repo, err := splitProjectKey(projectKey)
		if err != nil {
			yield(tracker.Issue{}, err)
			return
		}

		next := fmt.Sprintf("/repos/%s/%s/issues",
			url.PathEscape(owner), url.PathEscape(repo))
		q := url.Values{
			"state":     {"all"},
			"per_page":  {"100"},
			"sort":      {"updated"},
			"direction": {"desc"},
		}
		if !since.IsZero() {
			q.Set("since", since.UTC().Format(time.RFC3339))
		}
		rawQuery := q.Encode()

		for next != "" {
			var page []wireIssue
			nextURL, err := c.getPage(ctx, next, rawQuery, &page)
			if err != nil {
				yield(tracker.Issue{}, fmt.Errorf("list issues %s: %w", projectKey, err))
				return
			}
			for _, raw := range page {
				if raw.PullRequest != nil {
					continue
				}
				if !yield(issueFromWire(owner, repo, raw), nil) {
					return
				}
			}
			next, rawQuery = splitNextURL(nextURL)
		}
	}
}

func issueFromWire(owner, repo string, raw wireIssue) tracker.Issue {
	iss := tracker.Issue{
		ID:         fmt.Sprintf("%s/%s#%d", owner, repo, raw.Number),
		Key:        fmt.Sprintf("%s/%s#%d", owner, repo, raw.Number),
		ProjectKey: fmt.Sprintf("%s/%s", owner, repo),
		Title:      raw.Title,
		Type:       "Issue",
		Status:     raw.State,
		CreatedAt:  raw.CreatedAt.UTC(),
	}
	if raw.Assignee != nil {
		// GitHub usually hides email; fall back to login so per-dev joins
		// can still work when emails happen to match.
		iss.AssigneeEmail = raw.Assignee.Email
		if iss.AssigneeEmail == "" {
			iss.AssigneeEmail = raw.Assignee.Login
		}
	}
	if raw.ClosedAt != nil {
		t := raw.ClosedAt.UTC()
		iss.ClosedAt = &t
		iss.ResolvedAt = &t
	}
	if len(raw.Labels) > 0 {
		// First label as type (e.g. "bug", "enhancement"). Loupe doesn't
		// yet do anything semantic with type beyond surfacing it.
		iss.Type = raw.Labels[0].Name
	}
	return iss
}

// --- helpers ---

func (c *Client) getPage(ctx context.Context, path, rawQuery string, dest any) (nextURL string, _ error) {
	resp, err := c.api.Do(ctx, "GET", path, rawQuery, nil)
	if err != nil {
		return "", err
	}
	nextURL = parseLinkHeader(resp.Header.Get("Link"))
	if err := apiclient.DecodeJSON(resp, dest); err != nil {
		return "", err
	}
	return nextURL, nil
}

func parseLinkHeader(h string) string {
	if h == "" {
		return ""
	}
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end <= start+1 {
			return ""
		}
		return part[start+1 : end]
	}
	return ""
}

func splitNextURL(rawNext string) (string, string) {
	if rawNext == "" {
		return "", ""
	}
	u, err := url.Parse(rawNext)
	if err != nil {
		return "", ""
	}
	return u.Path, u.RawQuery
}

func splitProjectKey(projectKey string) (owner, repo string, _ error) {
	parts := strings.SplitN(projectKey, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("github tracker: invalid project key %q (want owner/repo)", projectKey)
	}
	return parts[0], parts[1], nil
}
