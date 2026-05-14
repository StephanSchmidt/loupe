package github

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

type fakeServer struct {
	t       *testing.T
	srv     *httptest.Server
	routes  map[string][]http.HandlerFunc
	cursor  map[string]int
	gotAuth string
}

func newFake(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{
		t:      t,
		routes: make(map[string][]http.HandlerFunc),
		cursor: make(map[string]int),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		key := r.Method + " " + r.URL.Path
		hs, ok := f.routes[key]
		if !ok {
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
			return
		}
		i := f.cursor[key]
		if i >= len(hs) {
			i = len(hs) - 1
		}
		f.cursor[key] = i + 1
		hs[i](w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) route(method, path string, hs ...http.HandlerFunc) {
	f.routes[method+" "+path] = hs
}

func newClient(t *testing.T, baseURL string) tracker.Tracker {
	t.Helper()
	c, err := New(baseURL, "pat-abc")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func mustJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

func TestNew_Validates(t *testing.T) {
	if _, err := New("", "t"); err == nil || !strings.Contains(err.Error(), "baseURL") {
		t.Errorf("expected baseURL error, got %v", err)
	}
	if _, err := New("https://x", ""); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("expected token error, got %v", err)
	}
}

func TestListProjects_SkipsRepoWithoutIssuesOrArchived(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/user", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, map[string]any{"login": "alice"})
	})
	f.route("GET", "/user/repos", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{
			{"name": "loupe", "full_name": "alice/loupe", "has_issues": true},
			{"name": "noissues", "full_name": "alice/noissues", "has_issues": false},
			{"name": "archived", "full_name": "alice/archived", "has_issues": true, "archived": true},
			// Owner-mismatch: surfaced by /user/repos when alice is an org admin.
			// Should be excluded from "alice"'s personal-repo enumeration.
			{"name": "service", "full_name": "acme/service", "has_issues": true},
		})
	})
	f.route("GET", "/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{{"login": "acme"}})
	})
	f.route("GET", "/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{
			{"name": "service", "full_name": "acme/service", "has_issues": true},
			{"name": "wiki", "full_name": "acme/wiki", "has_issues": false},
		})
	})

	c := newClient(t, f.srv.URL)
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	keys := make([]string, len(projects))
	for i, p := range projects {
		keys[i] = p.Key
	}
	want := []string{"alice/loupe", "acme/service"}
	if len(keys) != len(want) {
		t.Fatalf("projects = %v, want %v", keys, want)
	}
	for i := range keys {
		if keys[i] != want[i] {
			t.Errorf("projects[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
	if !strings.HasPrefix(f.gotAuth, "Bearer ") {
		t.Errorf("Authorization = %q, want Bearer prefix", f.gotAuth)
	}
}

func TestListIssues_FiltersPullRequests(t *testing.T) {
	f := newFake(t)
	closedAt := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	f.route("GET", "/repos/acme/svc/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "all" {
			t.Errorf("state = %q, want all", r.URL.Query().Get("state"))
		}
		mustJSON(t, w, []map[string]any{
			{"number": 1, "title": "real bug", "state": "open",
				"created_at": "2026-05-13T10:00:00Z", "updated_at": "2026-05-13T10:00:00Z",
				"labels":   []map[string]string{{"name": "bug"}},
				"assignee": map[string]string{"login": "alice", "email": "alice@acme.com"}},
			{"number": 2, "title": "this is a PR", "state": "open",
				"created_at":   "2026-05-12T10:00:00Z",
				"updated_at":   "2026-05-12T10:00:00Z",
				"pull_request": map[string]any{}}, // makes pull_request non-nil → skipped
			{"number": 3, "title": "old one", "state": "closed",
				"created_at": "2026-04-01T10:00:00Z", "updated_at": "2026-05-10T10:00:00Z",
				"closed_at": closedAt.Format(time.RFC3339)},
		})
	})

	c := newClient(t, f.srv.URL)
	var got []tracker.Issue
	for iss, err := range c.ListIssues(context.Background(), "acme/svc", time.Time{}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		got = append(got, iss)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (PR row excluded); got: %+v", len(got), got)
	}
	if got[0].Key != "acme/svc#1" || got[0].Type != "bug" || got[0].AssigneeEmail != "alice@acme.com" {
		t.Errorf("issue[0] = %+v", got[0])
	}
	if got[1].Key != "acme/svc#3" || got[1].ClosedAt == nil {
		t.Errorf("issue[1] = %+v", got[1])
	}
}

func TestListIssues_PassesSinceParam(t *testing.T) {
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	f := newFake(t)
	f.route("GET", "/repos/acme/svc/issues", func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("since")
		want := since.Format(time.RFC3339)
		if got != want {
			t.Errorf("since = %q, want %q", got, want)
		}
		mustJSON(t, w, []map[string]any{})
	})

	c := newClient(t, f.srv.URL)
	for range c.ListIssues(context.Background(), "acme/svc", since) {
	}
}

func TestListIssues_BadProjectKey(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f.srv.URL)
	var gotErr error
	for _, err := range c.ListIssues(context.Background(), "no-slash", time.Time{}) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "invalid project key") {
		t.Errorf("expected invalid-project-key error, got %v", gotErr)
	}
}
