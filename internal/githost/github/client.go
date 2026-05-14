// Package github is the v0 implementation of githost.GitHost backed by
// GitHub's REST API. Auth is bearer-token (PAT or fine-grained PAT).
// GitHub Enterprise Server (with its /api/v3 prefix) is not supported in
// v0 because apiclient currently replaces the base URL path; only
// github.com works.
package github

import (
	"context"
	"fmt"
	"iter"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/StephanSchmidt/loupe/internal/apiclient"
	"github.com/StephanSchmidt/loupe/internal/githost"
)

const (
	// Provider is the value persisted in the `provider` column of every row
	// produced by this client.
	Provider = "github"

	// DefaultBaseURL is the github.com REST endpoint. GHE deployments would
	// use https://<host>/api/v3 — not supported by v0 (see package doc).
	DefaultBaseURL = "https://api.github.com"
)

// Client implements githost.GitHost against GitHub.
//
// The client caches workspace types (User vs Organization) discovered by
// ListWorkspaces so ListRepos can route to /user/repos vs /orgs/{org}/repos
// without an extra round-trip per workspace. Not goroutine-safe — loupe's
// ingest pipeline calls these methods sequentially.
type Client struct {
	api      *apiclient.Client
	baseURL  string
	wsType   map[string]string // slug -> "User" | "Organization"
	userSlug string            // authed user login, cached from ListWorkspaces
}

var _ githost.GitHost = (*Client)(nil)

// New returns a Client. baseURL is typically https://api.github.com (tests
// pass an httptest.Server URL).
func New(baseURL, token string) (githost.GitHost, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("github: baseURL is required")
	}
	if token == "" {
		return nil, fmt.Errorf("github: token is required")
	}
	return &Client{
		baseURL: baseURL,
		wsType:  make(map[string]string),
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

// --- wire-format structs ---

type wireUser struct {
	Login string `json:"login"`
}

type wireOrg struct {
	Login string `json:"login"`
}

type wireRepo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}

type wireCommitAuthor struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `json:"date"`
}

type wireCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author  wireCommitAuthor `json:"author"`
		Message string           `json:"message"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

type wirePR struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Head      struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	User           *struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// --- ListWorkspaces ---

// ListWorkspaces returns the authed user as a workspace (so their personal
// repos get indexed) plus every org the user belongs to. The user/org
// distinction is stashed on the client for ListRepos routing.
func (c *Client) ListWorkspaces(ctx context.Context) ([]githost.Workspace, error) {
	user, err := c.getAuthedUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("get authed user: %w", err)
	}
	c.userSlug = user.Login
	c.wsType[user.Login] = "User"
	out := []githost.Workspace{{Slug: user.Login, Name: user.Login}}

	orgs, err := c.listUserOrgs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list user orgs: %w", err)
	}
	for _, o := range orgs {
		c.wsType[o.Login] = "Organization"
		out = append(out, githost.Workspace{Slug: o.Login, Name: o.Login})
	}
	return out, nil
}

func (c *Client) getAuthedUser(ctx context.Context) (wireUser, error) {
	var u wireUser
	resp, err := c.api.Do(ctx, "GET", "/user", "", nil)
	if err != nil {
		return u, err
	}
	if err := apiclient.DecodeJSON(resp, &u); err != nil {
		return u, err
	}
	return u, nil
}

func (c *Client) listUserOrgs(ctx context.Context) ([]wireOrg, error) {
	var out []wireOrg
	next := "/user/orgs"
	rawQuery := "per_page=100"
	for next != "" {
		var page []wireOrg
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		next, rawQuery = splitNextURL(nextURL)
	}
	return out, nil
}

// --- ListRepos ---

func (c *Client) ListRepos(ctx context.Context, workspaceSlug string) ([]githost.Repo, error) {
	switch c.wsType[workspaceSlug] {
	case "User":
		return c.listAuthedUserRepos(ctx, workspaceSlug)
	case "Organization":
		return c.listOrgRepos(ctx, workspaceSlug)
	default:
		// Caller didn't run ListWorkspaces first. Best-effort: assume org.
		return c.listOrgRepos(ctx, workspaceSlug)
	}
}

func (c *Client) listAuthedUserRepos(ctx context.Context, ownerLogin string) ([]githost.Repo, error) {
	var out []githost.Repo
	// /user/repos returns repos the authed user has access to, including
	// private — /users/{login}/repos returns public only. Filter by owner
	// to keep this workspace's repos.
	next := "/user/repos"
	rawQuery := "affiliation=owner&per_page=100"
	for next != "" {
		var page []wireRepo
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, fmt.Errorf("list user repos: %w", err)
		}
		for _, r := range page {
			if r.Owner.Login != ownerLogin {
				continue
			}
			out = append(out, repoFromWire(r))
		}
		next, rawQuery = splitNextURL(nextURL)
	}
	return out, nil
}

func (c *Client) listOrgRepos(ctx context.Context, org string) ([]githost.Repo, error) {
	var out []githost.Repo
	next := "/orgs/" + url.PathEscape(org) + "/repos"
	rawQuery := "type=all&per_page=100"
	for next != "" {
		var page []wireRepo
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, fmt.Errorf("list org %s repos: %w", org, err)
		}
		for _, r := range page {
			out = append(out, repoFromWire(r))
		}
		next, rawQuery = splitNextURL(nextURL)
	}
	return out, nil
}

func repoFromWire(r wireRepo) githost.Repo {
	return githost.Repo{
		RepoRef: githost.RepoRef{
			Workspace: r.Owner.Login,
			Slug:      r.Name,
		},
		Name: r.Name,
	}
}

// --- ListCommits ---

// ListCommits streams commits in reverse-chronological order. With a
// non-zero since, GitHub's `since` query parameter bounds the result
// server-side; pagination continues until the API runs out of pages.
func (c *Client) ListCommits(ctx context.Context, repo githost.RepoRef, since time.Time) iter.Seq2[githost.Commit, error] {
	return func(yield func(githost.Commit, error) bool) {
		next := fmt.Sprintf("/repos/%s/%s/commits",
			url.PathEscape(repo.Workspace), url.PathEscape(repo.Slug))
		q := url.Values{"per_page": {"100"}}
		if !since.IsZero() {
			q.Set("since", since.UTC().Format(time.RFC3339))
		}
		rawQuery := q.Encode()
		for next != "" {
			var page []wireCommit
			nextURL, err := c.getPage(ctx, next, rawQuery, &page)
			if err != nil {
				yield(githost.Commit{}, fmt.Errorf("list commits %s/%s: %w", repo.Workspace, repo.Slug, err))
				return
			}
			for _, raw := range page {
				if !yield(commitFromWire(raw), nil) {
					return
				}
			}
			next, rawQuery = splitNextURL(nextURL)
		}
	}
}

func commitFromWire(raw wireCommit) githost.Commit {
	return githost.Commit{
		SHA:         raw.SHA,
		AuthorEmail: raw.Commit.Author.Email,
		AuthorName:  raw.Commit.Author.Name,
		CommittedAt: raw.Commit.Author.Date.UTC(),
		Message:     raw.Commit.Message,
		ParentCount: len(raw.Parents),
	}
}

// --- ListPullRequests ---

// ListPullRequests streams PRs newest-updated-first and stops when an
// updated_at older than since is seen.
func (c *Client) ListPullRequests(ctx context.Context, repo githost.RepoRef, since time.Time) iter.Seq2[githost.PullRequest, error] {
	return func(yield func(githost.PullRequest, error) bool) {
		next := fmt.Sprintf("/repos/%s/%s/pulls",
			url.PathEscape(repo.Workspace), url.PathEscape(repo.Slug))
		rawQuery := "state=all&sort=updated&direction=desc&per_page=100"
		for next != "" {
			var page []wirePR
			nextURL, err := c.getPage(ctx, next, rawQuery, &page)
			if err != nil {
				yield(githost.PullRequest{}, fmt.Errorf("list PRs %s/%s: %w", repo.Workspace, repo.Slug, err))
				return
			}
			for _, raw := range page {
				if !since.IsZero() && raw.UpdatedAt.Before(since) {
					return
				}
				if !yield(prFromWire(raw), nil) {
					return
				}
			}
			next, rawQuery = splitNextURL(nextURL)
		}
	}
}

func prFromWire(raw wirePR) githost.PullRequest {
	pr := githost.PullRequest{
		ID:                strconv.Itoa(raw.Number),
		Title:             raw.Title,
		SourceBranch:      raw.Head.Ref,
		DestinationBranch: raw.Base.Ref,
		CreatedAt:         raw.CreatedAt.UTC(),
		MergeCommitSHA:    raw.MergeCommitSHA,
	}
	// State: GitHub has "open" and "closed"; merged is closed + merged_at.
	// Map to loupe's neutral set (OPEN | MERGED | DECLINED).
	switch {
	case raw.State == "open":
		pr.State = "OPEN"
	case raw.MergedAt != nil:
		pr.State = "MERGED"
	default:
		pr.State = "DECLINED"
	}
	if raw.MergedAt != nil {
		t := raw.MergedAt.UTC()
		pr.MergedAt = &t
	}
	if raw.ClosedAt != nil {
		t := raw.ClosedAt.UTC()
		pr.ClosedAt = &t
	}
	if raw.User != nil {
		// GitHub doesn't return author email on PR list; the login is the
		// best identifier we have without an extra /users/{login} fetch.
		pr.AuthorEmail = raw.User.Login
	}
	for _, l := range raw.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr
}

// --- ListPRCommits ---

// ListPRCommits returns the pre-squash commits for a PR — needed later for
// trailer recovery on squash-merged PRs where the merge commit drops the
// Co-Authored-By line.
func (c *Client) ListPRCommits(ctx context.Context, repo githost.RepoRef, prID string) ([]githost.Commit, error) {
	var out []githost.Commit
	next := fmt.Sprintf("/repos/%s/%s/pulls/%s/commits",
		url.PathEscape(repo.Workspace), url.PathEscape(repo.Slug), url.PathEscape(prID))
	rawQuery := "per_page=100"
	for next != "" {
		var page []wireCommit
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, fmt.Errorf("list PR commits %s/%s#%s: %w", repo.Workspace, repo.Slug, prID, err)
		}
		for _, raw := range page {
			out = append(out, commitFromWire(raw))
		}
		next, rawQuery = splitNextURL(nextURL)
	}
	return out, nil
}

// --- helpers ---

// getPage executes a GET against path?rawQuery, decodes JSON into dest,
// and returns the URL from the Link: rel="next" header (or "" when there
// are no more pages).
func (c *Client) getPage(ctx context.Context, path, rawQuery string, dest any) (nextURL string, _ error) {
	resp, err := c.api.Do(ctx, "GET", path, rawQuery, nil)
	if err != nil {
		return "", err
	}
	// Capture the Link header before DecodeJSON consumes the response.
	nextURL = parseLinkHeader(resp.Header.Get("Link"))
	if err := apiclient.DecodeJSON(resp, dest); err != nil {
		return "", err
	}
	return nextURL, nil
}

// parseLinkHeader extracts the URL marked rel="next" from a GitHub
// Link header, or "" if no next page is advertised. The header looks like:
//
//	Link: <https://api.github.com/...&page=2>; rel="next", <https://api.github.com/...&page=10>; rel="last"
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

// splitNextURL takes a full URL from the Link header and returns
// (path, rawQuery) for re-use against the same apiclient base. The host
// is discarded — GitHub returns absolute URLs on the same host we asked.
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
