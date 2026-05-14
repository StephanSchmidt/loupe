package github

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

// fakeServer is a tiny dispatcher keyed by "METHOD PATH". Handlers may
// inspect r.URL.RawQuery and route on it. The first request to a key uses
// handler[0], the second uses [1], and so on; calls past the last handler
// re-use the final one.
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

func newClient(t *testing.T, baseURL string) githost.GitHost {
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
		t.Fatalf("encode response: %v", err)
	}
}

func TestNew_Validates(t *testing.T) {
	cases := []struct{ base, tok, want string }{
		{"", "t", "baseURL"},
		{"https://x", "", "token"},
	}
	for _, c := range cases {
		_, err := New(c.base, c.tok)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("New(%q,%q) = %v, want substring %q", c.base, c.tok, err, c.want)
		}
	}
}

func TestListWorkspaces_UserPlusOrgs(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/user", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, map[string]any{"login": "alice"})
	})
	f.route("GET", "/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{
			{"login": "acme"},
			{"login": "beta"},
		})
	})

	c := newClient(t, f.srv.URL)
	ws, err := c.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	got := make([]string, len(ws))
	for i, w := range ws {
		got[i] = w.Slug
	}
	want := []string{"alice", "acme", "beta"}
	if len(got) != len(want) {
		t.Fatalf("workspaces = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("workspaces[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if !strings.HasPrefix(f.gotAuth, "Bearer ") {
		t.Errorf("Authorization = %q, want Bearer prefix", f.gotAuth)
	}
}

func TestListRepos_OrgPath(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/user", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, map[string]any{"login": "alice"})
	})
	f.route("GET", "/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{{"login": "acme"}})
	})
	f.route("GET", "/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{
			{"name": "service", "full_name": "acme/service", "owner": map[string]string{"login": "acme", "type": "Organization"}},
			{"name": "lib", "full_name": "acme/lib", "owner": map[string]string{"login": "acme", "type": "Organization"}},
		})
	})

	c := newClient(t, f.srv.URL)
	if _, err := c.ListWorkspaces(context.Background()); err != nil {
		t.Fatalf("seed ListWorkspaces: %v", err)
	}
	repos, err := c.ListRepos(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 || repos[0].Slug != "service" || repos[0].Workspace != "acme" {
		t.Errorf("repos = %+v", repos)
	}
}

func TestListRepos_UserPathFiltersByOwner(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/user", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, map[string]any{"login": "alice"})
	})
	f.route("GET", "/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{})
	})
	f.route("GET", "/user/repos", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{
			// owner == alice: include
			{"name": "loupe", "full_name": "alice/loupe", "owner": map[string]string{"login": "alice", "type": "User"}},
			// owner != alice (e.g. she's a collaborator): exclude
			{"name": "shared", "full_name": "carol/shared", "owner": map[string]string{"login": "carol", "type": "User"}},
		})
	})

	c := newClient(t, f.srv.URL)
	if _, err := c.ListWorkspaces(context.Background()); err != nil {
		t.Fatalf("seed ListWorkspaces: %v", err)
	}
	repos, err := c.ListRepos(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].Slug != "loupe" {
		t.Errorf("expected only alice/loupe, got %+v", repos)
	}
}

func TestListCommits_PaginatedViaLinkHeader(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/repos/acme/svc/commits",
		// page 1: advertise next page
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", `<`+f.srv.URL+`/repos/acme/svc/commits?page=2>; rel="next"`)
			mustJSON(t, w, []map[string]any{
				{
					"sha": "aaa",
					"commit": map[string]any{
						"author":  map[string]any{"name": "Alice", "email": "alice@acme.com", "date": "2026-05-13T10:00:00Z"},
						"message": "subject 1",
					},
					"parents": []map[string]string{{"sha": "p1"}},
				},
			})
		},
		// page 2: no Link header
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") != "2" {
				t.Errorf("expected page=2, got %q", r.URL.RawQuery)
			}
			mustJSON(t, w, []map[string]any{
				{
					"sha": "bbb",
					"commit": map[string]any{
						"author":  map[string]any{"name": "Bob", "email": "bob@acme.com", "date": "2026-05-10T09:00:00Z"},
						"message": "subject 2",
					},
					"parents": []map[string]string{{"sha": "p2"}, {"sha": "p3"}},
				},
			})
		})

	c := newClient(t, f.srv.URL)
	var shas []string
	for cmt, err := range c.ListCommits(context.Background(), githost.RepoRef{Workspace: "acme", Slug: "svc"}, time.Time{}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		shas = append(shas, cmt.SHA)
	}
	if len(shas) != 2 || shas[0] != "aaa" || shas[1] != "bbb" {
		t.Errorf("shas = %v, want [aaa bbb]", shas)
	}
}

func TestListPullRequests_StateMapping(t *testing.T) {
	merged := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	closed := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	f := newFake(t)
	f.route("GET", "/repos/acme/svc/pulls", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{
			{"number": 1, "title": "open one", "state": "open",
				"created_at": "2026-05-13T10:00:00Z", "updated_at": "2026-05-13T10:00:00Z",
				"head": map[string]string{"ref": "feature"}, "base": map[string]string{"ref": "main"},
				"user": map[string]string{"login": "alice"}},
			{"number": 2, "title": "merged one", "state": "closed",
				"created_at": "2026-05-08T10:00:00Z", "updated_at": "2026-05-10T10:00:00Z",
				"merged_at":        merged.Format(time.RFC3339),
				"closed_at":        merged.Format(time.RFC3339),
				"merge_commit_sha": "mmm",
				"head":             map[string]string{"ref": "fix-x"}, "base": map[string]string{"ref": "main"},
				"user": map[string]string{"login": "bob"}},
			{"number": 3, "title": "declined one", "state": "closed",
				"created_at": "2026-05-07T10:00:00Z", "updated_at": "2026-05-09T10:00:00Z",
				"closed_at": closed.Format(time.RFC3339),
				"head":      map[string]string{"ref": "nope"}, "base": map[string]string{"ref": "main"},
				"user":      map[string]string{"login": "carol"}},
		})
	})

	c := newClient(t, f.srv.URL)
	var got []githost.PullRequest
	for pr, err := range c.ListPullRequests(context.Background(), githost.RepoRef{Workspace: "acme", Slug: "svc"}, time.Time{}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		got = append(got, pr)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].State != "OPEN" {
		t.Errorf("pr[0].State = %q, want OPEN", got[0].State)
	}
	if got[1].State != "MERGED" || got[1].MergedAt == nil {
		t.Errorf("pr[1] = %+v, want MERGED with MergedAt", got[1])
	}
	if got[2].State != "DECLINED" || got[2].MergedAt != nil {
		t.Errorf("pr[2] = %+v, want DECLINED with no MergedAt", got[2])
	}
}

func TestListPullRequests_StopsBeforeSince(t *testing.T) {
	since := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	f := newFake(t)
	f.route("GET", "/repos/acme/svc/pulls", func(w http.ResponseWriter, r *http.Request) {
		mustJSON(t, w, []map[string]any{
			{"number": 10, "title": "newer", "state": "open",
				"created_at": "2026-05-13T10:00:00Z", "updated_at": "2026-05-13T10:00:00Z",
				"head": map[string]string{"ref": "x"}, "base": map[string]string{"ref": "main"},
				"user": map[string]string{"login": "alice"}},
			{"number": 9, "title": "older", "state": "open",
				"created_at": "2026-05-11T10:00:00Z", "updated_at": "2026-05-11T10:00:00Z",
				"head": map[string]string{"ref": "x"}, "base": map[string]string{"ref": "main"},
				"user": map[string]string{"login": "alice"}},
		})
	})

	c := newClient(t, f.srv.URL)
	var got []string
	for pr, err := range c.ListPullRequests(context.Background(), githost.RepoRef{Workspace: "acme", Slug: "svc"}, since) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		got = append(got, pr.ID)
	}
	if len(got) != 1 || got[0] != "10" {
		t.Errorf("got %v, want [10] (older PR should be skipped)", got)
	}
}

func TestParseLinkHeader(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{`<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=10>; rel="last"`, "https://api.github.com/x?page=2"},
		{`<https://api.github.com/x?page=10>; rel="last"`, ""},
		{`<https://api.github.com/x?page=1>; rel="prev"`, ""},
	}
	for _, c := range cases {
		if got := parseLinkHeader(c.in); got != c.want {
			t.Errorf("parseLinkHeader(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
