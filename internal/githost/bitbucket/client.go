// Package bitbucket is the v0 implementation of githost.GitHost backed by
// Bitbucket Cloud REST 2.0. Auth is basic (username + app password). The
// wire-format JSON structs live in this file; the neutral types live in
// the parent githost package.
package bitbucket

import (
	"context"
	"fmt"
	"iter"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/StephanSchmidt/loupe/internal/apiclient"
	"github.com/StephanSchmidt/loupe/internal/githost"
)

// Provider is the value persisted in the `provider` column of every row
// produced by this client.
const Provider = "bitbucket-cloud"

// Client implements githost.GitHost against Bitbucket Cloud.
type Client struct {
	api     *apiclient.Client
	baseURL string
}

// Compile-time assertion that Client implements the interface.
var _ githost.GitHost = (*Client)(nil)

// New returns a Client. baseURL is typically https://api.bitbucket.org/2.0
// (tests pass an httptest.Server URL). username is the Bitbucket account
// or email; appPassword is the Bitbucket app password (or the new API
// token — same basic-auth wire format).
func New(baseURL, username, appPassword string) (githost.GitHost, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("bitbucket: baseURL is required")
	}
	if username == "" {
		return nil, fmt.Errorf("bitbucket: username is required")
	}
	if appPassword == "" {
		return nil, fmt.Errorf("bitbucket: app password is required")
	}
	return &Client{
		baseURL: baseURL,
		api: apiclient.New(baseURL,
			apiclient.WithAuth(apiclient.BasicAuth(username, appPassword)),
			apiclient.WithProviderName(Provider),
			apiclient.WithHeader("User-Agent", "loupe/dev"),
		),
	}, nil
}

func (c *Client) Name() string { return Provider }

// --- wire-format structs ---

type pagedList[T any] struct {
	Values []T    `json:"values"`
	Next   string `json:"next,omitempty"`
}

type wsWire struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type repoWire struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	FullName  string `json:"full_name"`
	Workspace struct {
		Slug string `json:"slug"`
	} `json:"workspace"`
}

type commitWire struct {
	Hash   string `json:"hash"`
	Author struct {
		Raw  string `json:"raw"`
		User *struct {
			DisplayName string `json:"display_name"`
		} `json:"user,omitempty"`
	} `json:"author"`
	Date    time.Time `json:"date"`
	Message string    `json:"message"`
	Parents []struct {
		Hash string `json:"hash"`
	} `json:"parents"`
}

type prWire struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Author struct {
		Raw         string `json:"raw,omitempty"`
		DisplayName string `json:"display_name"`
	} `json:"author"`
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"source"`
	Destination struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"destination"`
	CreatedOn   time.Time  `json:"created_on"`
	UpdatedOn   time.Time  `json:"updated_on"`
	ClosedOn    *time.Time `json:"closed_on,omitempty"`
	MergeCommit *struct {
		Hash string `json:"hash"`
	} `json:"merge_commit,omitempty"`
}

// --- ListWorkspaces ---

func (c *Client) ListWorkspaces(ctx context.Context) ([]githost.Workspace, error) {
	var out []githost.Workspace
	next := "/2.0/workspaces"
	rawQuery := "pagelen=100"
	prev := ""
	for pages := 0; next != ""; pages++ {
		cur := cursor(next, rawQuery)
		if err := guardPagination(prev, cur, pages); err != nil {
			return nil, fmt.Errorf("list workspaces: %w", err)
		}
		prev = cur
		var page pagedList[wsWire]
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, fmt.Errorf("list workspaces: %w", err)
		}
		for _, w := range page.Values {
			out = append(out, githost.Workspace{Slug: w.Slug, Name: w.Name})
		}
		next, rawQuery = nextPathQuery(nextURL)
	}
	return out, nil
}

// --- ListRepos ---

func (c *Client) ListRepos(ctx context.Context, workspaceSlug string) ([]githost.Repo, error) {
	var out []githost.Repo
	next := "/2.0/repositories/" + url.PathEscape(workspaceSlug)
	rawQuery := "pagelen=100"
	prev := ""
	for pages := 0; next != ""; pages++ {
		cur := cursor(next, rawQuery)
		if err := guardPagination(prev, cur, pages); err != nil {
			return nil, fmt.Errorf("list repos for %s: %w", workspaceSlug, err)
		}
		prev = cur
		var page pagedList[repoWire]
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, fmt.Errorf("list repos for %s: %w", workspaceSlug, err)
		}
		for _, r := range page.Values {
			ws := r.Workspace.Slug
			if ws == "" {
				ws = workspaceSlug
			}
			out = append(out, githost.Repo{
				RepoRef: githost.RepoRef{Workspace: ws, Slug: r.Slug},
				Name:    r.Name,
			})
		}
		next, rawQuery = nextPathQuery(nextURL)
	}
	return out, nil
}

// --- ListCommits ---

// ListCommits streams commits in reverse-chronological order (Bitbucket's
// default). When since is non-zero, the stream stops as soon as a commit
// older than since is seen.
func (c *Client) ListCommits(ctx context.Context, repo githost.RepoRef, since time.Time) iter.Seq2[githost.Commit, error] {
	return func(yield func(githost.Commit, error) bool) {
		next := "/2.0/repositories/" + url.PathEscape(repo.Workspace) + "/" + url.PathEscape(repo.Slug) + "/commits"
		rawQuery := "pagelen=100"
		prev := ""
		for pages := 0; next != ""; pages++ {
			cur := cursor(next, rawQuery)
			if err := guardPagination(prev, cur, pages); err != nil {
				yield(githost.Commit{}, fmt.Errorf("list commits %s/%s: %w", repo.Workspace, repo.Slug, err))
				return
			}
			prev = cur
			var page pagedList[commitWire]
			nextURL, err := c.getPage(ctx, next, rawQuery, &page)
			if err != nil {
				yield(githost.Commit{}, fmt.Errorf("list commits %s/%s: %w", repo.Workspace, repo.Slug, err))
				return
			}
			for _, raw := range page.Values {
				if !since.IsZero() && raw.Date.Before(since) {
					return // commits arrive newest-first; we're past the watermark
				}
				c := commitFromWire(raw)
				if !yield(c, nil) {
					return
				}
			}
			next, rawQuery = nextPathQuery(nextURL)
		}
	}
}

// --- ListPullRequests ---

func (c *Client) ListPullRequests(ctx context.Context, repo githost.RepoRef, since time.Time) iter.Seq2[githost.PullRequest, error] {
	return func(yield func(githost.PullRequest, error) bool) {
		next := "/2.0/repositories/" + url.PathEscape(repo.Workspace) + "/" + url.PathEscape(repo.Slug) + "/pullrequests"
		// Without state= the API only returns OPEN. We want everything.
		rawQuery := "pagelen=50&state=OPEN&state=MERGED&state=DECLINED&state=SUPERSEDED&sort=-updated_on"
		prev := ""
		for pages := 0; next != ""; pages++ {
			cur := cursor(next, rawQuery)
			if err := guardPagination(prev, cur, pages); err != nil {
				yield(githost.PullRequest{}, fmt.Errorf("list PRs %s/%s: %w", repo.Workspace, repo.Slug, err))
				return
			}
			prev = cur
			var page pagedList[prWire]
			nextURL, err := c.getPage(ctx, next, rawQuery, &page)
			if err != nil {
				yield(githost.PullRequest{}, fmt.Errorf("list PRs %s/%s: %w", repo.Workspace, repo.Slug, err))
				return
			}
			for _, raw := range page.Values {
				if !since.IsZero() && raw.UpdatedOn.Before(since) {
					return
				}
				if !yield(prFromWire(raw), nil) {
					return
				}
			}
			next, rawQuery = nextPathQuery(nextURL)
		}
	}
}

// --- ListPRCommits ---

func (c *Client) ListPRCommits(ctx context.Context, repo githost.RepoRef, prID string) ([]githost.Commit, error) {
	var out []githost.Commit
	next := fmt.Sprintf("/2.0/repositories/%s/%s/pullrequests/%s/commits",
		url.PathEscape(repo.Workspace), url.PathEscape(repo.Slug), url.PathEscape(prID))
	rawQuery := "pagelen=100"
	prev := ""
	for pages := 0; next != ""; pages++ {
		cur := cursor(next, rawQuery)
		if err := guardPagination(prev, cur, pages); err != nil {
			return nil, fmt.Errorf("list PR commits %s/%s#%s: %w", repo.Workspace, repo.Slug, prID, err)
		}
		prev = cur
		var page pagedList[commitWire]
		nextURL, err := c.getPage(ctx, next, rawQuery, &page)
		if err != nil {
			return nil, fmt.Errorf("list PR commits %s/%s#%s: %w", repo.Workspace, repo.Slug, prID, err)
		}
		for _, raw := range page.Values {
			out = append(out, commitFromWire(raw))
		}
		next, rawQuery = nextPathQuery(nextURL)
	}
	return out, nil
}

// --- helpers ---

// getPage executes a GET against path?rawQuery and decodes JSON into dest.
// Returns the raw `next` URL from the response so the caller can paginate.
func (c *Client) getPage(ctx context.Context, path, rawQuery string, dest any) (nextURL string, _ error) {
	resp, err := c.api.Do(ctx, "GET", path, rawQuery, nil)
	if err != nil {
		return "", err
	}
	if err := apiclient.DecodeJSON(resp, dest); err != nil {
		return "", err
	}
	// dest is *pagedList[T]; pull Next out via a tiny interface so we
	// don't pay reflection costs.
	if n, ok := dest.(interface{ nextURL() string }); ok {
		return n.nextURL(), nil
	}
	// Inline path: dest is *pagedList[T] where Next is the field.
	// We can grab it via the wire structs directly since they all share
	// the same shape — use a type switch at the call sites instead.
	switch v := dest.(type) {
	case *pagedList[wsWire]:
		return v.Next, nil
	case *pagedList[repoWire]:
		return v.Next, nil
	case *pagedList[commitWire]:
		return v.Next, nil
	case *pagedList[prWire]:
		return v.Next, nil
	}
	return "", nil
}

// maxPaginationPages caps any single paginated enumeration to a
// generous-but-finite ceiling. At pagelen=100 this covers up to 1M items,
// well above any realistic org, but guarantees a misbehaving cursor that
// never terminates eventually fails rather than spins forever.
const maxPaginationPages = 10000

// guardPagination returns a non-nil error when the paginator should
// abort: either the page count exceeded maxPaginationPages, or the next
// cursor matches the previous one (a stuck cursor we've actually seen
// from Bitbucket Cloud during stale-cache incidents). The cursor passed
// in must include both path and query so that same-path/different-query
// pagination doesn't false-positive.
func guardPagination(prev, next string, pages int) error {
	if pages >= maxPaginationPages {
		return fmt.Errorf("pagination exceeded %d pages — aborting", maxPaginationPages)
	}
	if next != "" && next == prev {
		return fmt.Errorf("pagination cursor did not advance: %q", next)
	}
	return nil
}

// cursor returns a single comparable token for guardPagination. Bitbucket
// pagination uses (path, rawQuery) pairs; concatenating with a sentinel
// keeps them distinguishable.
func cursor(path, rawQuery string) string { return path + "?" + rawQuery }

// nextPathQuery splits a Bitbucket `next` URL into (path, raw query) for
// re-use against the same apiclient base. Bitbucket's `next` is a full
// URL pointing at the same host — we drop scheme+host and keep the rest.
func nextPathQuery(rawNext string) (string, string) {
	if rawNext == "" {
		return "", ""
	}
	u, err := url.Parse(rawNext)
	if err != nil {
		return "", ""
	}
	return u.Path, u.RawQuery
}

// emailFromRaw extracts an email from Bitbucket's "Name <email>" raw
// author string. Returns "" if no angle-bracketed email is present.
var emailRe = regexp.MustCompile(`<([^>]+)>`)

func emailFromRaw(raw string) (name, email string) {
	idx := strings.Index(raw, "<")
	if idx < 0 {
		return strings.TrimSpace(raw), ""
	}
	m := emailRe.FindStringSubmatch(raw[idx:])
	if m == nil {
		return strings.TrimSpace(raw), ""
	}
	return strings.TrimSpace(raw[:idx]), strings.TrimSpace(m[1])
}

func commitFromWire(raw commitWire) githost.Commit {
	name, email := emailFromRaw(raw.Author.Raw)
	if name == "" && raw.Author.User != nil {
		name = raw.Author.User.DisplayName
	}
	return githost.Commit{
		SHA:         raw.Hash,
		AuthorEmail: email,
		AuthorName:  name,
		CommittedAt: raw.Date.UTC(),
		Message:     raw.Message,
		ParentCount: len(raw.Parents),
	}
}

func prFromWire(raw prWire) githost.PullRequest {
	pr := githost.PullRequest{
		ID:                fmt.Sprintf("%d", raw.ID),
		Title:             raw.Title,
		State:             raw.State,
		SourceBranch:      raw.Source.Branch.Name,
		DestinationBranch: raw.Destination.Branch.Name,
		CreatedAt:         raw.CreatedOn.UTC(),
	}
	if raw.MergeCommit != nil {
		pr.MergeCommitSHA = raw.MergeCommit.Hash
	}
	if raw.ClosedOn != nil {
		t := raw.ClosedOn.UTC()
		pr.ClosedAt = &t
		if raw.State == "MERGED" {
			pr.MergedAt = &t
		}
	}
	// Bitbucket only exposes the raw "Name <email>" string. If no email
	// can be parsed, leave AuthorEmail empty rather than writing the
	// display name — downstream joins treat this column as an email.
	_, email := emailFromRaw(raw.Author.Raw)
	pr.AuthorEmail = email
	// Bitbucket Cloud has no authoritative bot-type flag on PR authors and
	// the user object on PRs doesn't expose a stable "login"; leave
	// AuthorLogin empty and AuthorIsBot false. Bot detection on Bitbucket
	// PRs falls through to email-substring rules (snyk-bot@snyk.io,
	// bot@renovateapp.com, etc.) in internal/analyze/bots.go.
	return pr
}
