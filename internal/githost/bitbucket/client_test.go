package bitbucket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/StephanSchmidt/loupe/internal/githost"
)

// fakeServer hosts a dispatcher map. Each entry is keyed by "METHOD PATH"
// (without query); the handler may inspect r.URL.RawQuery and route on it
// when paginating.
type fakeServer struct {
	t       *testing.T
	srv     *httptest.Server
	routes  map[string][]http.HandlerFunc // key = "GET /2.0/...". Slice consumed in order on repeated calls.
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

func newClientFor(t *testing.T, baseURL string) githost.GitHost {
	t.Helper()
	c, err := New(baseURL, "alice", "app-pw", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func mustJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// --- tests ---

func TestNew_Validates(t *testing.T) {
	cases := []struct{ base, user, pw, want string }{
		{"", "u", "p", "baseURL"},
		{"https://x", "", "p", "username"},
		{"https://x", "u", "", "app password"},
	}
	for _, c := range cases {
		_, err := New(c.base, c.user, c.pw, "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("New(%q,%q,%q) error = %v, want %q", c.base, c.user, c.pw, err, c.want)
		}
	}
}

func TestListWorkspaces_DirectWhenWorkspaceSet(t *testing.T) {
	// With a workspace configured, ListWorkspaces must NOT call the
	// account-level /2.0/workspaces endpoint — that endpoint is unsupported
	// by Atlassian API tokens. It returns the single configured workspace
	// directly, making zero HTTP requests. newFake fails the test on any
	// unexpected request, so an empty route table asserts "no calls".
	f := newFake(t)
	c, err := New(f.srv.URL, "alice@example.com", "api-token", "acme")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(got) != 1 || got[0].Slug != "acme" {
		t.Fatalf("got %+v, want single acme workspace", got)
	}
}

func TestListWorkspaces_Paginated(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/2.0/workspaces",
		func(w http.ResponseWriter, r *http.Request) {
			mustJSON(t, w, map[string]any{
				"values": []map[string]string{{"slug": "acme", "name": "Acme"}, {"slug": "beta", "name": "Beta"}},
				"next":   f.srv.URL + "/2.0/workspaces?page=2",
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "page=2" {
				t.Errorf("page 2 query = %q, want page=2", r.URL.RawQuery)
			}
			mustJSON(t, w, map[string]any{
				"values": []map[string]string{{"slug": "gamma", "name": "Gamma"}},
			})
		},
	)

	c := newClientFor(t, f.srv.URL)
	got, err := c.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
		return
	}
	if got[0].Slug != "acme" || got[2].Slug != "gamma" {
		t.Errorf("slugs = %+v", got)
	}
	if !strings.HasPrefix(f.gotAuth, "Basic ") {
		t.Errorf("no basic auth observed: %q", f.gotAuth)
	}
}

func TestListRepos(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/2.0/repositories/acme", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, map[string]any{
			"values": []map[string]any{
				{"slug": "backend", "name": "backend", "full_name": "acme/backend", "workspace": map[string]string{"slug": "acme"}},
				{"slug": "frontend", "name": "frontend", "full_name": "acme/frontend", "workspace": map[string]string{"slug": "acme"}},
			},
		})
	})
	c := newClientFor(t, f.srv.URL)
	got, err := c.ListRepos(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d repos, want 2", len(got))
		return
	}
	if got[0].Slug != "backend" || got[1].Slug != "frontend" {
		t.Errorf("repos = %+v", got)
	}
	if got[0].Workspace != "acme" {
		t.Errorf("workspace = %q", got[0].Workspace)
	}
}

func TestListCommits_StopsAtSince(t *testing.T) {
	f := newFake(t)
	mkCommit := func(sha, raw string, secs int64) map[string]any {
		return map[string]any{
			"hash":    sha,
			"author":  map[string]any{"raw": raw},
			"date":    time.Unix(secs, 0).UTC().Format(time.RFC3339),
			"message": "msg",
			"parents": []any{},
		}
	}
	f.route("GET", "/2.0/repositories/acme/backend/commits", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, map[string]any{
			"values": []map[string]any{
				mkCommit("c3", "Alice <alice@a>", 1700000300), // newest
				mkCommit("c2", "Bob <bob@b>", 1700000200),
				mkCommit("c1", "Carol <carol@c>", 1700000100), // oldest — strictly before since
			},
		})
	})

	c := newClientFor(t, f.srv.URL)
	since := time.Unix(1700000150, 0).UTC()
	repo := githost.RepoRef{Workspace: "acme", Slug: "backend"}

	var got []githost.Commit
	for commit, err := range c.ListCommits(context.Background(), repo, since) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		got = append(got, commit)
	}
	if len(got) != 2 {
		t.Fatalf("got %d commits, want 2 (c3,c2); c1 should be filtered by since", len(got))
		return
	}
	if got[0].AuthorEmail != "alice@a" || got[0].AuthorName != "Alice" {
		t.Errorf("author parsing wrong: %+v", got[0])
	}
}

func TestListPullRequests_StreamsAndStopsAtSince(t *testing.T) {
	f := newFake(t)
	mkPR := func(id int, state string, updated int64) map[string]any {
		return map[string]any{
			"id":          id,
			"title":       "PR",
			"state":       state,
			"author":      map[string]any{"raw": "Alice <a@a>"},
			"source":      map[string]any{"branch": map[string]any{"name": "feature"}},
			"destination": map[string]any{"branch": map[string]any{"name": "main"}},
			"created_on":  time.Unix(updated-100, 0).UTC().Format(time.RFC3339),
			"updated_on":  time.Unix(updated, 0).UTC().Format(time.RFC3339),
		}
	}
	f.route("GET", "/2.0/repositories/acme/backend/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, map[string]any{
			"values": []map[string]any{
				mkPR(3, "MERGED", 1700000300),
				mkPR(2, "OPEN", 1700000200),
				mkPR(1, "DECLINED", 1700000100), // before since
			},
		})
	})

	c := newClientFor(t, f.srv.URL)
	since := time.Unix(1700000150, 0).UTC()
	repo := githost.RepoRef{Workspace: "acme", Slug: "backend"}

	var got []githost.PullRequest
	for pr, err := range c.ListPullRequests(context.Background(), repo, since) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		got = append(got, pr)
	}
	if len(got) != 2 {
		t.Fatalf("got %d PRs, want 2", len(got))
	}
	if len(got) == 0 {
		t.Fatal("expected at least one PR")
		return
	}
	if got[0].ID != "3" || got[0].State != "MERGED" {
		t.Errorf("PR[0] = %+v", got[0])
	}
	if got[0].AuthorEmail != "a@a" {
		t.Errorf("author email = %q", got[0].AuthorEmail)
	}
}

func TestListPRCommits(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/2.0/repositories/acme/backend/pullrequests/42/commits", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, map[string]any{
			"values": []map[string]any{
				{"hash": "a", "author": map[string]any{"raw": "Alice <a@a>"}, "date": time.Now().UTC().Format(time.RFC3339), "message": "first", "parents": []any{}},
				{"hash": "b", "author": map[string]any{"raw": "Bob <b@b>"}, "date": time.Now().UTC().Format(time.RFC3339),
					"message": "second\n\nCo-Authored-By: Claude <noreply@anthropic.com>", "parents": []any{}},
			},
		})
	})
	c := newClientFor(t, f.srv.URL)
	repo := githost.RepoRef{Workspace: "acme", Slug: "backend"}
	got, err := c.ListPRCommits(context.Background(), repo, "42")
	if err != nil {
		t.Fatalf("ListPRCommits: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d commits, want 2", len(got))
		return
	}
	if !strings.Contains(got[1].Message, "Co-Authored-By: Claude") {
		t.Errorf("trailer not preserved in returned message: %q", got[1].Message)
	}
}

func TestName(t *testing.T) {
	c := newClientFor(t, "https://x")
	if c.Name() != Provider {
		t.Errorf("Name = %q, want %q", c.Name(), Provider)
	}
}

func TestEmailFromRaw(t *testing.T) {
	cases := []struct{ in, name, email string }{
		{"Alice Smith <alice@acme.com>", "Alice Smith", "alice@acme.com"},
		{"  Bob <b@b>  ", "Bob", "b@b"},
		{"No Email Here", "No Email Here", ""},
		{"<just-email@x>", "", "just-email@x"},
	}
	for _, c := range cases {
		gotName, gotEmail := emailFromRaw(c.in)
		if gotName != c.name || gotEmail != c.email {
			t.Errorf("emailFromRaw(%q) = (%q,%q), want (%q,%q)", c.in, gotName, gotEmail, c.name, c.email)
		}
	}
}
