// Package github is the v0 implementation of githost.GitHost backed by
// GitHub's REST API. Auth is bearer-token (PAT or fine-grained PAT).
// GitHub Enterprise Server (with its /api/v3 prefix) is not supported in
// v0 because apiclient currently replaces the base URL path; only
// github.com works.
package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"os"
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

	// maxRateLimitRetries bounds how many consecutive rate-limit waits a
	// single request will tolerate before giving up. Five gives plenty of
	// headroom for the primary 5000/h limit plus a couple of secondary
	// bursts without looping forever on a misbehaving upstream.
	maxRateLimitRetries = 5

	// maxRateLimitWait caps any single sleep. The primary limit resets at
	// most one hour out, so anything longer is almost certainly a clock
	// skew or a header bug and not worth waiting through.
	maxRateLimitWait = time.Hour

	// rateLimitBuffer is added to every rate-limit sleep so we don't retry
	// exactly at the documented reset instant. GitHub and our clock can
	// disagree by a few seconds, and an early retry just earns another 403
	// against the not-yet-reset window. 15s is overkill for plain clock
	// skew but cheap insurance.
	rateLimitBuffer = 15 * time.Second

	// maxTransientRetries bounds retries for transport-level failures (HTTP/2
	// stream cancels, connection resets, unexpected EOFs mid-body). Three
	// attempts after the first means worst-case 7s of backoff before we
	// surface the error to the caller — long enough to ride out a brief
	// upstream blip, short enough not to mask a real outage.
	maxTransientRetries = 3

	// transientRetryBase is the first transient-retry sleep. Doubles each
	// attempt (1s, 2s, 4s).
	transientRetryBase = time.Second

	// maxRedirects bounds the number of 301/302 hops a single request will
	// follow before giving up. GitHub returns 301 for renamed or
	// transferred repos, pointing at the /repositories/{id}/... canonical
	// path; a small cap is enough for the documented one-hop case and
	// prevents misconfigured upstreams from looping us indefinitely.
	maxRedirects = 3
)

// rateLimitBackoffFloor is the per-attempt minimum sleep applied to retries
// after the first. The initial retry honors X-RateLimit-Reset as-is (usually
// accurate). Subsequent retries enforce this floor to defend against the
// boundary case where GitHub keeps returning Remaining: 0 with a reset
// header at-or-before "now" — typically clock skew at the reset boundary,
// or a secondary (abuse) limit signalled with stale primary-limit headers.
// Indexed by retry attempt minus one; attempts past the end reuse the last.
var rateLimitBackoffFloor = [...]time.Duration{
	1 * time.Minute,
	2 * time.Minute,
	4 * time.Minute,
	8 * time.Minute,
	16 * time.Minute,
}

// rateLimitLog is where the wrapper writes "sleeping until reset" lines.
// Stored as a package-level variable so tests can redirect it.
var rateLimitLog io.Writer = os.Stderr

// sleepCh indirects time.After so tests can replace the sleeper with an
// instant-fire channel and avoid wall-clock waits in the rate-limit retry
// loop. Mirrors the rateLimitLog pattern.
var sleepCh = func(d time.Duration) <-chan time.Time { return time.After(d) }

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
	Archived bool `json:"archived"`
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
		// Type is "User" or "Bot" (App identities). Authoritative bot signal.
		Type string `json:"type"`
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
	resp, err := c.doRequest(ctx, "GET", "/user", "", nil)
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
		Name:     r.Name,
		Archived: r.Archived,
	}
}

// --- ListCommits ---

// ListCommits streams commits in reverse-chronological order. With a
// non-zero since, GitHub's `since` query parameter bounds the result
// server-side; pagination continues until the API runs out of pages.
//
// Empty repositories (initialised but with no default branch) return 409
// Conflict from /commits — that's not an error condition for loupe, so
// we treat it as "no commits" and let the rest of the pipeline proceed.
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
				if isEmptyRepoError(err) {
					return
				}
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

// isEmptyRepoError reports whether err comes from GitHub's documented
// 409 response on /commits for a repository with no default branch.
// The 409 only fires on the commits endpoint — /pulls and /issues
// return 200 [] for empty repos, so we don't generalise this check.
func isEmptyRepoError(err error) bool {
	var se *apiclient.StatusError
	if !errors.As(err, &se) || se == nil {
		// errors.As guarantees a non-nil target on true; the second check
		// is for nilaway's flow analysis, which can't model that contract.
		return false
	}
	return se.StatusCode == http.StatusConflict
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
		// We also populate AuthorEmail with the login so legacy queries
		// (and curated email-substring bot rules) keep working unchanged.
		pr.AuthorLogin = raw.User.Login
		pr.AuthorEmail = raw.User.Login
		pr.AuthorIsBot = strings.EqualFold(raw.User.Type, "Bot")
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

// doRequest wraps c.api.Do with a GitHub-specific rate-limit retry loop.
//
// GitHub signals rate limiting two ways:
//   - Primary REST limit: HTTP 403 with X-RateLimit-Remaining: 0 and a
//     X-RateLimit-Reset unix-epoch header. The shared apiclient surfaces
//     this as a fatal *StatusError because 403 isn't covered by its
//     generic Retry-After path.
//   - Secondary (abuse) limit: HTTP 429, sometimes with Retry-After.
//     apiclient already does a one-shot retry for Retry-After ≤ 60s; we
//     handle the longer or header-less cases here.
//
// On a retryable response we sleep until the documented reset (capped at
// maxRateLimitWait), log one line to rateLimitLog so the user sees why
// the run paused, then retry up to maxRateLimitRetries times. Anything
// else is returned unchanged.
func (c *Client) doRequest(ctx context.Context, method, path, rawQuery string, body io.Reader) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRateLimitRetries; attempt++ {
		resp, err := c.doOnceFollowingRedirects(ctx, method, path, rawQuery, body)
		if err == nil {
			return resp, nil
		}
		wait, ok := rateLimitWait(err, time.Now())
		if !ok || wait > maxRateLimitWait {
			return nil, err
		}
		lastErr = err
		wait = applyBackoffFloor(wait, attempt) + rateLimitBuffer
		fmt.Fprintf(rateLimitLog, "github: rate limit hit, sleeping %s until %s (attempt %d/%d)\n",
			wait.Round(time.Second), time.Now().Add(wait).Format(time.RFC3339), attempt+1, maxRateLimitRetries)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("github %s %s (waiting for rate limit reset): %w", method, path, ctx.Err())
		case <-sleepCh(wait):
		}
	}
	return nil, lastErr
}

// doOnceFollowingRedirects issues the request and, when the upstream
// answers with a 301/302 pointing at the same host, re-issues against the
// new path. GitHub returns 301 when a repo has been renamed or
// transferred and points at the stable /repositories/{id}/... path; the
// shared apiclient surfaces redirects as *StatusError because it
// deliberately doesn't follow them (auth replay risk on cross-origin).
//
// Same-host enforcement keeps the Authorization header from leaking to a
// foreign host. A bounded hop count guards against pathological loops.
func (c *Client) doOnceFollowingRedirects(ctx context.Context, method, path, rawQuery string, body io.Reader) (*http.Response, error) {
	for hop := 0; hop <= maxRedirects; hop++ {
		resp, err := c.api.Do(ctx, method, path, rawQuery, body)
		if err == nil {
			return resp, nil
		}
		newPath, newQuery, ok := redirectTarget(err, c.baseURL)
		if !ok {
			return nil, err
		}
		if hop == maxRedirects {
			return nil, fmt.Errorf("github %s %s: too many redirects (last → %s)", method, path, newPath)
		}
		fmt.Fprintf(rateLimitLog, "github: %s %s redirected → %s\n", method, path, newPath)
		path, rawQuery = newPath, newQuery
	}
	return nil, fmt.Errorf("github %s %s: redirect loop", method, path)
}

// redirectTarget extracts a same-host redirect target from a *StatusError.
// Returns (path, rawQuery, true) when err is a 301/302 with a usable
// Location header pointing at the same host as baseURL; otherwise the
// bool is false and the caller surfaces err unchanged.
//
// Non-idempotent methods would normally need 307/308 to be redirected
// safely. Our caller only sends GETs (auth, listing endpoints), so we
// accept 301/302 too — GitHub's renamed-repo response is 301.
func redirectTarget(err error, baseURL string) (string, string, bool) {
	var se *apiclient.StatusError
	if !errors.As(err, &se) || se == nil {
		return "", "", false
	}
	switch se.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return "", "", false
	}
	loc := se.Headers.Get("Location")
	if loc == "" {
		return "", "", false
	}
	base, berr := url.Parse(baseURL)
	if berr != nil {
		return "", "", false
	}
	target, terr := url.Parse(loc)
	if terr != nil {
		return "", "", false
	}
	// Resolve so a path-only Location ("/repositories/123/commits") gets
	// the scheme+host of the base URL applied, then enforce same-host.
	target = base.ResolveReference(target)
	if target.Host != base.Host || target.Scheme != base.Scheme {
		return "", "", false
	}
	return target.Path, target.RawQuery, true
}

// rateLimitWait inspects a *apiclient.StatusError for GitHub's rate-limit
// signals and returns the duration to sleep before retrying. The bool is
// false when the error doesn't look like a rate-limit response, so the
// caller surfaces it unchanged.
func rateLimitWait(err error, now time.Time) (time.Duration, bool) {
	var se *apiclient.StatusError
	if !errors.As(err, &se) || se == nil {
		return 0, false
	}
	switch se.StatusCode {
	case http.StatusForbidden:
		// Only a 403 with Remaining: 0 is the primary rate limit; other
		// 403s (bad scope, SAML SSO required, etc.) must not retry.
		if se.Headers.Get("X-RateLimit-Remaining") != "0" {
			return 0, false
		}
		return waitFromReset(se.Headers.Get("X-RateLimit-Reset"), now)
	case http.StatusTooManyRequests:
		if d, ok := apiclient.ParseRetryAfter(se.Headers.Get("Retry-After"), now); ok {
			return d, true
		}
		return waitFromReset(se.Headers.Get("X-RateLimit-Reset"), now)
	}
	return 0, false
}

// waitFromReset parses an X-RateLimit-Reset header (unix epoch seconds)
// and returns how long until that instant. A reset in the past yields
// (0, true) so the caller retries immediately.
func waitFromReset(reset string, now time.Time) (time.Duration, bool) {
	if reset == "" {
		return 0, false
	}
	secs, err := strconv.ParseInt(reset, 10, 64)
	if err != nil {
		return 0, false
	}
	d := time.Unix(secs, 0).Sub(now)
	if d < 0 {
		return 0, true
	}
	return d, true
}

// applyBackoffFloor enforces a minimum sleep on retries after the first.
// attempt is 0-based: attempt=0 (the first sleep) is left untouched so the
// common case "header is fresh, sleep exactly that long" is unchanged.
// Values past the table length reuse the last entry.
func applyBackoffFloor(wait time.Duration, attempt int) time.Duration {
	if attempt <= 0 {
		return wait
	}
	idx := attempt - 1
	if idx >= len(rateLimitBackoffFloor) {
		idx = len(rateLimitBackoffFloor) - 1
	}
	floor := rateLimitBackoffFloor[idx]
	if wait < floor {
		return floor
	}
	return wait
}

// getPage executes a GET against path?rawQuery, decodes JSON into dest,
// and returns the URL from the Link: rel="next" header (or "" when there
// are no more pages).
//
// Transient transport failures (HTTP/2 stream cancels, connection resets,
// unexpected EOFs mid-body — observed after long-running ingest sessions)
// are retried with exponential backoff. GET is idempotent, so retries are
// safe. *StatusError responses are not retried here; the rate-limit cases
// are already handled inside doRequest.
func (c *Client) getPage(ctx context.Context, path, rawQuery string, dest any) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= maxTransientRetries; attempt++ {
		nextURL, err := c.getPageOnce(ctx, path, rawQuery, dest)
		if err == nil {
			return nextURL, nil
		}
		if !apiclient.IsTransientErr(err) {
			return "", err
		}
		lastErr = err
		if attempt == maxTransientRetries {
			return "", err
		}
		delay := transientRetryBase * (1 << attempt)
		fmt.Fprintf(rateLimitLog, "github: transient error, sleeping %s before retry (attempt %d/%d): %v\n",
			delay, attempt+1, maxTransientRetries, err)
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("github GET %s (waiting for transient retry): %w", path, ctx.Err())
		case <-sleepCh(delay):
		}
	}
	return "", lastErr
}

func (c *Client) getPageOnce(ctx context.Context, path, rawQuery string, dest any) (string, error) {
	resp, err := c.doRequest(ctx, "GET", path, rawQuery, nil)
	if err != nil {
		return "", err
	}
	// Capture the Link header before DecodeJSON consumes the response.
	nextURL := parseLinkHeader(resp.Header.Get("Link"))
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
