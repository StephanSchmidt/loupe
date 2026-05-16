// Package jira is the v0 implementation of tracker.Tracker backed by Jira
// Cloud REST v3. Auth is basic (email + API token).
package jira

import (
	"context"
	"fmt"
	"iter"
	"net/url"
	"time"

	"github.com/StephanSchmidt/loupe/internal/apiclient"
	"github.com/StephanSchmidt/loupe/internal/tracker"
)

const Provider = "jira-cloud"

// Client implements tracker.Tracker against Jira Cloud.
type Client struct {
	api  *apiclient.Client
	site string
	// tz is the authed account's reporting timezone, fetched lazily from
	// /rest/api/3/myself the first time ListIssues needs to format a
	// `since` watermark into JQL. JQL date literals have no zone marker
	// and are evaluated in the account's timezone, so formatting in UTC
	// can drop or duplicate an entire offset's worth of issues.
	tz *time.Location
}

// Compile-time assertion.
var _ tracker.Tracker = (*Client)(nil)

// New returns a Client. site is the bare Jira host (e.g.
// "acme.atlassian.net"); the constructor composes the full base URL.
// Tests pass an httptest.Server URL via the baseURL form below.
func New(site, email, apiToken string) (tracker.Tracker, error) {
	if site == "" {
		return nil, fmt.Errorf("jira: site is required")
	}
	if email == "" {
		return nil, fmt.Errorf("jira: email is required")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("jira: api token is required")
	}
	return NewWithBaseURL("https://"+site, email, apiToken)
}

// NewWithBaseURL is the form used by tests against httptest.Server, where
// the base URL is already a full http://127.0.0.1:port. Production
// callers should go through New.
func NewWithBaseURL(baseURL, email, apiToken string) (tracker.Tracker, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("jira: baseURL is required")
	}
	return &Client{
		site: baseURL,
		api: apiclient.New(baseURL,
			apiclient.WithAuth(apiclient.BasicAuth(email, apiToken)),
			apiclient.WithProviderName(Provider),
			apiclient.WithHeader("User-Agent", "loupe/dev"),
		),
	}, nil
}

func (c *Client) Name() string { return Provider }

// --- wire structs ---

type projectListWire struct {
	StartAt    int           `json:"startAt"`
	MaxResults int           `json:"maxResults"`
	Total      int           `json:"total"`
	IsLast     bool          `json:"isLast"`
	Values     []projectWire `json:"values"`
}

type projectWire struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

type issueSearchWire struct {
	Issues        []issueWire `json:"issues"`
	NextPageToken string      `json:"nextPageToken,omitempty"`
}

type issueWire struct {
	ID        string         `json:"id"`
	Key       string         `json:"key"`
	Fields    issueFields    `json:"fields"`
	Changelog *changelogWire `json:"changelog,omitempty"`
}

type issueFields struct {
	Summary   string `json:"summary"`
	IssueType struct {
		Name string `json:"name"`
	} `json:"issuetype"`
	Status struct {
		Name string `json:"name"`
	} `json:"status"`
	Created        jiraTime  `json:"created"`
	ResolutionDate *jiraTime `json:"resolutiondate,omitempty"`
	Assignee       *struct {
		EmailAddress string `json:"emailAddress"`
	} `json:"assignee,omitempty"`
	Project struct {
		Key string `json:"key"`
	} `json:"project"`
	TimeEstimate float64 `json:"timeestimate"`
}

// changelogWire is Jira's standard expand=changelog payload. histories are
// returned newest-first by the API; the wire structure mirrors that and
// the converter reverses to oldest-first.
type changelogWire struct {
	Histories []changelogHistory `json:"histories"`
}

type changelogHistory struct {
	Created jiraTime               `json:"created"`
	Items   []changelogHistoryItem `json:"items"`
}

type changelogHistoryItem struct {
	Field      string `json:"field"`
	FromString string `json:"fromString"`
	ToString   string `json:"toString"`
}

// jiraTime parses Jira's "2026-01-01T10:00:00.000+0000" timestamps, which
// time.RFC3339 doesn't accept (TZ has no colon).
type jiraTime struct {
	time.Time
}

func (t *jiraTime) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" || s == `""` {
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05-0700",
		time.RFC3339,
	} {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed.UTC()
			return nil
		}
	}
	return fmt.Errorf("parse jira time %q", s)
}

// myselfWire is the slice of /rest/api/3/myself we care about. timeZone is
// an IANA name like "Europe/Berlin" or "Etc/UTC".
type myselfWire struct {
	TimeZone string `json:"timeZone"`
}

// resolveTimezone returns the Jira account's reporting timezone, caching
// the result on the client. Falls back to UTC if the API or the IANA name
// can't be loaded — better to misalign by some hours than to fail the run.
func (c *Client) resolveTimezone(ctx context.Context) (*time.Location, error) {
	if c.tz != nil {
		return c.tz, nil
	}
	resp, err := c.api.Do(ctx, "GET", "/rest/api/3/myself", "", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch /myself: %w", err)
	}
	var m myselfWire
	if err := apiclient.DecodeJSON(resp, &m); err != nil {
		return nil, fmt.Errorf("decode /myself: %w", err)
	}
	loc := time.UTC
	if m.TimeZone != "" {
		if l, err := time.LoadLocation(m.TimeZone); err == nil {
			loc = l
		}
	}
	c.tz = loc
	return loc, nil
}

// --- ListProjects ---

func (c *Client) ListProjects(ctx context.Context) ([]tracker.Project, error) {
	var out []tracker.Project
	startAt := 0
	for {
		q := url.Values{}
		q.Set("startAt", fmt.Sprintf("%d", startAt))
		q.Set("maxResults", "50")
		var page projectListWire
		resp, err := c.api.Do(ctx, "GET", "/rest/api/3/project/search", q.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		if err := apiclient.DecodeJSON(resp, &page); err != nil {
			return nil, fmt.Errorf("decode projects: %w", err)
		}
		for _, p := range page.Values {
			out = append(out, tracker.Project{Key: p.Key, Name: p.Name})
		}
		if page.IsLast || len(page.Values) == 0 {
			break
		}
		startAt += len(page.Values)
		if startAt >= page.Total && page.Total > 0 {
			break
		}
	}
	return out, nil
}

// --- ListIssues ---

// ListIssues streams issues for projectKey updated after since (zero means
// full history). Uses Jira Cloud's `/rest/api/3/search/jql` endpoint with
// cursor pagination via nextPageToken.
func (c *Client) ListIssues(ctx context.Context, projectKey string, since time.Time) iter.Seq2[tracker.Issue, error] {
	return func(yield func(tracker.Issue, error) bool) {
		jql := fmt.Sprintf("project = %s ORDER BY updated DESC", projectKey)
		if !since.IsZero() {
			tz, err := c.resolveTimezone(ctx)
			if err != nil {
				yield(tracker.Issue{}, fmt.Errorf("resolve jira timezone: %w", err))
				return
			}
			jql = fmt.Sprintf(`project = %s AND updated >= "%s" ORDER BY updated DESC`,
				projectKey, since.In(tz).Format("2006-01-02 15:04"))
		}

		const maxPages = 10000 // pagelen 100 × 10k = 1M issues, far above any real project
		nextToken := ""
		prevToken := ""
		for pages := 0; ; pages++ {
			if pages >= maxPages {
				yield(tracker.Issue{}, fmt.Errorf("list issues %s: pagination exceeded %d pages — aborting", projectKey, maxPages))
				return
			}
			q := url.Values{}
			q.Set("jql", jql)
			q.Set("maxResults", "100")
			q.Set("fields", "summary,issuetype,status,created,resolutiondate,assignee,project,timeestimate")
			// expand=changelog inlines status-transition history on each issue
			// so cycle-time computation doesn't need a per-issue follow-up
			// request. Histories come back newest-first; issueFromWire flips
			// them to oldest-first before populating tracker.Transition.
			q.Set("expand", "changelog")
			if nextToken != "" {
				q.Set("nextPageToken", nextToken)
			}
			resp, err := c.api.Do(ctx, "GET", "/rest/api/3/search/jql", q.Encode(), nil)
			if err != nil {
				yield(tracker.Issue{}, fmt.Errorf("list issues %s: %w", projectKey, err))
				return
			}
			var page issueSearchWire
			if err := apiclient.DecodeJSON(resp, &page); err != nil {
				yield(tracker.Issue{}, fmt.Errorf("decode issues %s: %w", projectKey, err))
				return
			}
			for _, raw := range page.Issues {
				if !yield(issueFromWire(raw), nil) {
					return
				}
			}
			if page.NextPageToken == "" {
				return
			}
			if page.NextPageToken == prevToken && prevToken != "" {
				yield(tracker.Issue{}, fmt.Errorf("list issues %s: pagination cursor did not advance: %q", projectKey, page.NextPageToken))
				return
			}
			prevToken = nextToken
			nextToken = page.NextPageToken
		}
	}
}

func issueFromWire(raw issueWire) tracker.Issue {
	iss := tracker.Issue{
		ID:         raw.ID,
		Key:        raw.Key,
		ProjectKey: raw.Fields.Project.Key,
		Title:      raw.Fields.Summary,
		Type:       raw.Fields.IssueType.Name,
		Status:     raw.Fields.Status.Name,
		CreatedAt:  raw.Fields.Created.Time,
		Estimate:   raw.Fields.TimeEstimate,
	}
	if raw.Fields.Assignee != nil {
		iss.AssigneeEmail = raw.Fields.Assignee.EmailAddress
	}
	// `resolutiondate` is sometimes returned as "" rather than null on
	// unresolved issues, in which case jiraTime.UnmarshalJSON leaves the
	// embedded time zero. Treat zero as "unset" so downstream `*time.Time`
	// consumers don't confuse year 0001 with a real resolution.
	if raw.Fields.ResolutionDate != nil && !raw.Fields.ResolutionDate.IsZero() {
		t := raw.Fields.ResolutionDate.Time
		iss.ResolvedAt = &t
		iss.ClosedAt = &t
	}
	iss.Transitions = transitionsFromChangelog(raw.Changelog)
	return iss
}

// transitionsFromChangelog flattens Jira's nested history payload into a
// list of status-only transitions, oldest-first. Non-status items (field,
// assignee, …) are dropped.
func transitionsFromChangelog(cl *changelogWire) []tracker.Transition {
	if cl == nil || len(cl.Histories) == 0 {
		return nil
	}
	var out []tracker.Transition
	// Jira returns histories newest-first; iterate in reverse so the
	// emitted slice is oldest-first.
	for i := len(cl.Histories) - 1; i >= 0; i-- {
		h := cl.Histories[i]
		for _, item := range h.Items {
			if item.Field != "status" {
				continue
			}
			out = append(out, tracker.Transition{
				At:         h.Created.Time,
				FromStatus: item.FromString,
				ToStatus:   item.ToString,
			})
		}
	}
	return out
}
