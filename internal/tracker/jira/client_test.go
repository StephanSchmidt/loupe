package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/StephanSchmidt/loupe/internal/tracker"
)

func mustJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func newClient(t *testing.T, srv *httptest.Server) tracker.Tracker {
	t.Helper()
	c, err := NewWithBaseURL(srv.URL, "alice@acme.com", "api-token")
	if err != nil {
		t.Fatalf("NewWithBaseURL: %v", err)
	}
	return c
}

func TestNew_Validates(t *testing.T) {
	cases := []struct{ site, email, token, want string }{
		{"", "a", "t", "site"},
		{"acme.atlassian.net", "", "t", "email"},
		{"acme.atlassian.net", "a", "", "api token"},
	}
	for _, c := range cases {
		_, err := New(c.site, c.email, c.token)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("New(%q,%q,%q) = %v, want %q", c.site, c.email, c.token, err, c.want)
		}
	}
}

func TestListProjects_PaginatedByStartAt(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/rest/api/3/project/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.URL.Query().Get("startAt") {
		case "0":
			mustJSON(t, w, map[string]any{
				"startAt":    0,
				"maxResults": 50,
				"total":      3,
				"isLast":     false,
				"values": []map[string]string{
					{"id": "1", "key": "ENG", "name": "Engineering"},
					{"id": "2", "key": "OPS", "name": "Ops"},
				},
			})
		case "2":
			mustJSON(t, w, map[string]any{
				"startAt":    2,
				"maxResults": 50,
				"total":      3,
				"isLast":     true,
				"values": []map[string]string{
					{"id": "3", "key": "DOC", "name": "Docs"},
				},
			})
		default:
			t.Errorf("unexpected startAt=%q", r.URL.Query().Get("startAt"))
		}
	}))
	defer srv.Close()

	c := newClient(t, srv)
	got, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d projects, want 3 (%+v)", len(got), got)
		return
	}
	if got[0].Key != "ENG" || got[2].Key != "DOC" {
		t.Errorf("keys = %+v", got)
	}
	if calls != 2 {
		t.Errorf("expected 2 paginated calls, got %d", calls)
	}
}

func TestListIssues_PaginatedAndDecoded(t *testing.T) {
	mkIssue := func(key string) map[string]any {
		return map[string]any{
			"id":  key + "-id",
			"key": key,
			"fields": map[string]any{
				"summary":        "Fix " + key,
				"issuetype":      map[string]any{"name": "Bug"},
				"status":         map[string]any{"name": "Done"},
				"created":        "2026-05-01T10:00:00.000+0000",
				"resolutiondate": "2026-05-05T11:00:00.000+0000",
				"assignee":       map[string]any{"emailAddress": "alice@acme.com"},
				"project":        map[string]any{"key": "ENG"},
				"timeestimate":   3600,
			},
		}
	}

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if !strings.Contains(r.URL.Query().Get("jql"), "project = ENG") {
			t.Errorf("jql = %q", r.URL.Query().Get("jql"))
		}
		switch r.URL.Query().Get("nextPageToken") {
		case "":
			mustJSON(t, w, map[string]any{
				"issues":        []map[string]any{mkIssue("ENG-1"), mkIssue("ENG-2")},
				"nextPageToken": "page-2",
			})
		case "page-2":
			mustJSON(t, w, map[string]any{
				"issues": []map[string]any{mkIssue("ENG-3")},
			})
		default:
			t.Errorf("unexpected token = %q", r.URL.Query().Get("nextPageToken"))
		}
	}))
	defer srv.Close()

	c := newClient(t, srv)
	var got []tracker.Issue
	for iss, err := range c.ListIssues(context.Background(), "ENG", time.Time{}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		got = append(got, iss)
	}
	if len(got) != 3 {
		t.Fatalf("got %d issues, want 3 (%+v)", len(got), got)
		return
	}
	if calls != 2 {
		t.Errorf("expected 2 paginated calls, got %d", calls)
	}
	assertIssueRow(t, got)
}

func assertIssueRow(t *testing.T, got []tracker.Issue) {
	t.Helper()
	if got[0].Key != "ENG-1" || got[2].Key != "ENG-3" {
		t.Errorf("keys = %+v", got)
	}
	if got[0].Title != "Fix ENG-1" || got[0].Status != "Done" || got[0].ProjectKey != "ENG" {
		t.Errorf("row[0] = %+v", got[0])
	}
	if got[0].AssigneeEmail != "alice@acme.com" {
		t.Errorf("assignee email = %q", got[0].AssigneeEmail)
	}
	if got[0].ResolvedAt == nil || got[0].ClosedAt == nil {
		t.Errorf("resolved/closed should be set: %+v", got[0])
	}
}

func TestListIssues_SinceFilterRendersJQL(t *testing.T) {
	var seenJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenJQL = r.URL.Query().Get("jql")
		mustJSON(t, w, map[string]any{"issues": []any{}})
	}))
	defer srv.Close()

	c := newClient(t, srv)
	since := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, err := range c.ListIssues(context.Background(), "ENG", since) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	}
	if !strings.Contains(seenJQL, `updated >= "2026-05-01 12:00"`) {
		t.Errorf("JQL did not include since filter: %q", seenJQL)
	}
}

func TestListIssues_SinceFilterUsesAccountTimezone(t *testing.T) {
	// Jira account in UTC+10 (Australia/Sydney is +10 in May, AEST).
	var seenJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/myself":
			mustJSON(t, w, map[string]any{"timeZone": "Australia/Sydney"})
		case "/rest/api/3/search/jql":
			seenJQL = r.URL.Query().Get("jql")
			mustJSON(t, w, map[string]any{"issues": []any{}})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newClient(t, srv)
	// 2026-05-01 12:00 UTC == 2026-05-01 22:00 in Australia/Sydney (AEST, +10)
	since := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, err := range c.ListIssues(context.Background(), "ENG", since) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	}
	if !strings.Contains(seenJQL, `updated >= "2026-05-01 22:00"`) {
		t.Errorf("JQL did not render in account timezone: %q", seenJQL)
	}
}

func TestListIssues_ExpandsChangelogAndFlattensTransitions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("expand"); got != "changelog" {
			t.Errorf("expected expand=changelog, got %q", got)
		}
		// Jira returns histories newest-first; the parser flips them to
		// oldest-first.
		mustJSON(t, w, map[string]any{
			"issues": []map[string]any{
				{
					"id":  "ENG-1-id",
					"key": "ENG-1",
					"fields": map[string]any{
						"summary":   "Fix login",
						"issuetype": map[string]any{"name": "Bug"},
						"status":    map[string]any{"name": "Done"},
						"created":   "2026-05-01T10:00:00.000+0000",
						"project":   map[string]any{"key": "ENG"},
					},
					"changelog": map[string]any{
						"histories": []map[string]any{
							// newest
							{
								"created": "2026-05-05T11:00:00.000+0000",
								"items": []map[string]any{
									{"field": "status", "fromString": "In Progress", "toString": "Done"},
								},
							},
							{
								"created": "2026-05-03T09:00:00.000+0000",
								"items": []map[string]any{
									{"field": "assignee", "fromString": "", "toString": "alice"},
									{"field": "status", "fromString": "To Do", "toString": "In Progress"},
								},
							},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := newClient(t, srv)
	var got []tracker.Issue
	for iss, err := range c.ListIssues(context.Background(), "ENG", time.Time{}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		got = append(got, iss)
	}
	if len(got) != 1 {
		t.Fatalf("got %d issues, want 1", len(got))
	}
	tx := got[0].Transitions
	if len(tx) != 2 {
		t.Fatalf("got %d status transitions, want 2 (status-only, oldest-first): %+v", len(tx), tx)
	}
	if tx[0].ToStatus != "In Progress" || tx[0].FromStatus != "To Do" {
		t.Errorf("first transition = %+v, want To Do → In Progress", tx[0])
	}
	if tx[1].ToStatus != "Done" || tx[1].FromStatus != "In Progress" {
		t.Errorf("second transition = %+v, want In Progress → Done", tx[1])
	}
	if !tx[0].At.Before(tx[1].At) {
		t.Errorf("transitions not oldest-first: %v then %v", tx[0].At, tx[1].At)
	}
}

func TestName(t *testing.T) {
	c, err := NewWithBaseURL("https://x", "a@a", "t")
	if err != nil {
		t.Fatalf("NewWithBaseURL: %v", err)
	}
	if c.Name() != Provider {
		t.Errorf("Name = %q, want %q", c.Name(), Provider)
	}
}
