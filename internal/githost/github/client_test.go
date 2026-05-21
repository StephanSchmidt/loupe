package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/StephanSchmidt/loupe/internal/apiclient"
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
				"user": map[string]string{"login": "carol"}},
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

func TestListCommits_EmptyRepoReturns409Gracefully(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/repos/acme/empty/commits", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Git Repository is empty.","status":"409"}`))
	})

	c := newClient(t, f.srv.URL)
	var commits []githost.Commit
	var streamErr error
	for cmt, err := range c.ListCommits(context.Background(), githost.RepoRef{Workspace: "acme", Slug: "empty"}, time.Time{}) {
		if err != nil {
			streamErr = err
			break
		}
		commits = append(commits, cmt)
	}
	if streamErr != nil {
		t.Errorf("expected 409 to be swallowed, got error: %v", streamErr)
	}
	if len(commits) != 0 {
		t.Errorf("expected zero commits, got %d", len(commits))
	}
}

// writeRateLimit emits GitHub's primary-rate-limit response shape: 403
// with X-RateLimit-Remaining: 0 and a unix-epoch X-RateLimit-Reset.
func writeRateLimit(w http.ResponseWriter, reset time.Time) {
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
}

// TestDoRequest_PrimaryRateLimitRetries verifies that a 403 carrying
// X-RateLimit-Remaining: 0 plus a near-immediate X-RateLimit-Reset is
// treated as the primary REST limit: the wrapper sleeps until reset and
// retries instead of bubbling the error to the caller.
func TestDoRequest_PrimaryRateLimitRetries(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/user",
		// First call: rate limit.
		func(w http.ResponseWriter, _ *http.Request) {
			writeRateLimit(w, time.Now().Add(-time.Second))
		},
		// Second call: success.
		func(w http.ResponseWriter, _ *http.Request) {
			mustJSON(t, w, map[string]any{"login": "alice"})
		},
	)
	f.route("GET", "/user/orgs", func(w http.ResponseWriter, _ *http.Request) {
		mustJSON(t, w, []map[string]any{})
	})

	prev := rateLimitLog
	rateLimitLog = io.Discard
	defer func() { rateLimitLog = prev }()

	prevSleep := sleepCh
	sleepCh = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { sleepCh = prevSleep }()

	c := newClient(t, f.srv.URL)
	ws, err := c.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if f.cursor["GET /user"] != 2 {
		t.Errorf("expected 2 hits on /user (1 + 1 retry), got %d", f.cursor["GET /user"])
	}
	if len(ws) == 0 || ws[0].Slug != "alice" {
		t.Errorf("ws[0] = %+v, want alice", ws)
	}
}

// TestDoRequest_PrimaryRateLimitGivesUpWhenResetTooFar covers the cap:
// a primary-limit response whose reset is further out than maxRateLimitWait
// must not sleep; the original *StatusError is returned to the caller.
func TestDoRequest_PrimaryRateLimitGivesUpWhenResetTooFar(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/user", func(w http.ResponseWriter, _ *http.Request) {
		writeRateLimit(w, time.Now().Add(2*time.Hour))
	})

	c := newClient(t, f.srv.URL)
	_, err := c.ListWorkspaces(context.Background())
	if err == nil {
		t.Fatal("expected error when reset is beyond the cap")
	}
	if f.cursor["GET /user"] != 1 {
		t.Errorf("expected 1 hit on /user (no retry past cap), got %d", f.cursor["GET /user"])
	}
}

// TestDoRequest_NonRateLimit403Surfaces makes sure non-rate-limit 403s
// (insufficient scope, SAML SSO required, etc.) are not retried — those
// don't carry X-RateLimit-Remaining: 0.
func TestDoRequest_NonRateLimit403Surfaces(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/user", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible"}`))
	})

	c := newClient(t, f.srv.URL)
	_, err := c.ListWorkspaces(context.Background())
	if err == nil {
		t.Fatal("expected non-rate-limit 403 to surface")
	}
	if f.cursor["GET /user"] != 1 {
		t.Errorf("expected 1 hit on /user (no retry on plain 403), got %d", f.cursor["GET /user"])
	}
}

// TestDoRequest_ContextCancelDuringWait verifies the sleep is interruptible.
func TestDoRequest_ContextCancelDuringWait(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/user", func(w http.ResponseWriter, _ *http.Request) {
		// Reset 10s out — well within the cap but long enough for us to cancel.
		writeRateLimit(w, time.Now().Add(10*time.Second))
	})

	prev := rateLimitLog
	rateLimitLog = io.Discard
	defer func() { rateLimitLog = prev }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	c := newClient(t, f.srv.URL)
	_, err := c.ListWorkspaces(ctx)
	if err == nil {
		t.Fatal("expected ctx-cancel error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want wrapping context.Canceled", err)
	}
	if f.cursor["GET /user"] != 1 {
		t.Errorf("expected exactly 1 server hit, got %d", f.cursor["GET /user"])
	}
}

// TestDoRequest_PrimaryRateLimitStaleResetAppliesBackoffFloor covers the
// boundary bug: GitHub returns Remaining: 0 with an X-RateLimit-Reset at or
// before "now" on every retry. Without the floor the loop spins through all
// attempts in one tick; with it the loop sleeps 1m, 2m, 4m, 8m before the
// final attempt succeeds. (The first retry honors the header as-is.)
func TestDoRequest_PrimaryRateLimitStaleResetAppliesBackoffFloor(t *testing.T) {
	f := newFake(t)
	handlers := make([]http.HandlerFunc, 0, 6)
	for i := 0; i < 5; i++ {
		handlers = append(handlers, func(w http.ResponseWriter, _ *http.Request) {
			writeRateLimit(w, time.Now().Add(-time.Second))
		})
	}
	handlers = append(handlers, func(w http.ResponseWriter, _ *http.Request) {
		mustJSON(t, w, map[string]any{"login": "alice"})
	})
	f.route("GET", "/user", handlers...)
	f.route("GET", "/user/orgs", func(w http.ResponseWriter, _ *http.Request) {
		mustJSON(t, w, []map[string]any{})
	})

	prevLog := rateLimitLog
	rateLimitLog = io.Discard
	defer func() { rateLimitLog = prevLog }()

	var waits []time.Duration
	prevSleep := sleepCh
	sleepCh = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { sleepCh = prevSleep }()

	c := newClient(t, f.srv.URL)
	ws, err := c.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if got := f.cursor["GET /user"]; got != 6 {
		t.Errorf("expected 6 hits on /user (1 + 5 retries), got %d", got)
	}
	if len(ws) == 0 || ws[0].Slug != "alice" {
		t.Errorf("ws[0] = %+v, want alice", ws)
	}
	// Each retry adds rateLimitBuffer (15s). The first retry (attempt=0)
	// honors the (past) header → 0 + 15s buffer. Subsequent retries enforce
	// the per-attempt floor + 15s buffer.
	wantWaits := []time.Duration{
		15 * time.Second,
		1*time.Minute + 15*time.Second,
		2*time.Minute + 15*time.Second,
		4*time.Minute + 15*time.Second,
		8*time.Minute + 15*time.Second,
	}
	if len(waits) != len(wantWaits) {
		t.Fatalf("sleeper invoked %d times, want %d (waits=%v)", len(waits), len(wantWaits), waits)
	}
	for i, w := range waits {
		if w != wantWaits[i] {
			t.Errorf("waits[%d] = %s, want %s", i, w, wantWaits[i])
		}
	}
}

func TestApplyBackoffFloor(t *testing.T) {
	tests := []struct {
		attempt int
		wait    time.Duration
		want    time.Duration
	}{
		{0, 0, 0},
		{0, 10 * time.Minute, 10 * time.Minute},
		{1, 0, 1 * time.Minute},
		{1, 30 * time.Second, 1 * time.Minute},
		{1, 5 * time.Minute, 5 * time.Minute},
		{2, 0, 2 * time.Minute},
		{3, 0, 4 * time.Minute},
		{4, 0, 8 * time.Minute},
		{5, 0, 16 * time.Minute},
		{99, 0, 16 * time.Minute},
	}
	for _, tc := range tests {
		got := applyBackoffFloor(tc.wait, tc.attempt)
		if got != tc.want {
			t.Errorf("applyBackoffFloor(%s, %d) = %s, want %s", tc.wait, tc.attempt, got, tc.want)
		}
	}
}

// writeTransient hijacks the connection and writes response headers with a
// Content-Length larger than the body it sends, then closes the socket.
// On the client side io.ReadAll surfaces io.ErrUnexpectedEOF — the same
// kind of mid-body transport failure produced by an http2 RST_STREAM.
func writeTransient(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("ResponseWriter does not support Hijacker")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	defer func() { _ = conn.Close() }()
	headers := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n"
	if _, err := conn.Write([]byte(headers + `[{`)); err != nil {
		t.Fatalf("write partial: %v", err)
	}
}

// TestGetPage_RetriesOnTransientError covers the user-observed failure
// after 12 hours of streaming PRs: an HTTP/2 stream cancel (or any other
// mid-body transport error) aborted the entire run. With the fix, getPage
// retries up to maxTransientRetries times on transient errors.
func TestGetPage_RetriesOnTransientError(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/repos/acme/svc/pulls",
		func(w http.ResponseWriter, _ *http.Request) {
			writeTransient(t, w)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			writeTransient(t, w)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			mustJSON(t, w, []map[string]any{
				{"number": 1, "state": "open", "title": "PR1", "user": map[string]any{"login": "alice"}},
			})
		},
	)

	prevLog := rateLimitLog
	rateLimitLog = io.Discard
	defer func() { rateLimitLog = prevLog }()

	var waits []time.Duration
	prevSleep := sleepCh
	sleepCh = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { sleepCh = prevSleep }()

	c := newClient(t, f.srv.URL)
	var prs []githost.PullRequest
	for pr, err := range c.ListPullRequests(context.Background(), githost.RepoRef{Workspace: "acme", Slug: "svc"}, time.Time{}) {
		if err != nil {
			t.Fatalf("ListPullRequests: %v", err)
		}
		prs = append(prs, pr)
	}
	if got := f.cursor["GET /repos/acme/svc/pulls"]; got != 3 {
		t.Errorf("expected 3 hits (2 transient + 1 success), got %d", got)
	}
	if len(prs) != 1 || prs[0].ID != "1" {
		t.Errorf("PRs = %+v, want [{ID:1 ...}]", prs)
	}
	wantWaits := []time.Duration{1 * time.Second, 2 * time.Second}
	if len(waits) != len(wantWaits) {
		t.Fatalf("sleeper invoked %d times, want %d (waits=%v)", len(waits), len(wantWaits), waits)
	}
	for i, w := range waits {
		if w != wantWaits[i] {
			t.Errorf("waits[%d] = %s, want %s", i, w, wantWaits[i])
		}
	}
}

// TestGetPage_TransientGivesUpAfterMaxRetries verifies the bound: when
// every retry fails with a transient error, the loop surfaces the last
// error rather than spinning forever.
func TestGetPage_TransientGivesUpAfterMaxRetries(t *testing.T) {
	f := newFake(t)
	handlers := make([]http.HandlerFunc, 0, maxTransientRetries+1)
	for i := 0; i <= maxTransientRetries; i++ {
		handlers = append(handlers, func(w http.ResponseWriter, _ *http.Request) {
			writeTransient(t, w)
		})
	}
	f.route("GET", "/repos/acme/svc/pulls", handlers...)

	prevLog := rateLimitLog
	rateLimitLog = io.Discard
	defer func() { rateLimitLog = prevLog }()

	prevSleep := sleepCh
	sleepCh = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { sleepCh = prevSleep }()

	c := newClient(t, f.srv.URL)
	var streamErr error
	for _, err := range c.ListPullRequests(context.Background(), githost.RepoRef{Workspace: "acme", Slug: "svc"}, time.Time{}) {
		if err != nil {
			streamErr = err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if got := f.cursor["GET /repos/acme/svc/pulls"]; got != maxTransientRetries+1 {
		t.Errorf("expected %d hits, got %d", maxTransientRetries+1, got)
	}
}

// TestDoRequest_Follows301Redirect reproduces the user-observed failure:
// after a repo is renamed/transferred, GitHub returns 301 with a
// Location header pointing at the stable /repositories/{id}/... path.
// The client must follow the redirect transparently so a 24h baseline
// doesn't abort on a single moved repo.
func TestDoRequest_Follows301Redirect(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/repos/acme/old/commits", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/repositories/12345/commits")
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = w.Write([]byte(`{"message":"Moved Permanently"}`))
	})
	f.route("GET", "/repositories/12345/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustJSON(t, w, []map[string]any{
			{
				"sha": "aaa",
				"commit": map[string]any{
					"author":  map[string]any{"name": "Alice", "email": "a@b", "date": "2026-05-13T10:00:00Z"},
					"message": "after-rename",
				},
				"parents": []map[string]string{{"sha": "p"}},
			},
		})
	})

	prevLog := rateLimitLog
	rateLimitLog = io.Discard
	defer func() { rateLimitLog = prevLog }()

	c := newClient(t, f.srv.URL)
	var shas []string
	for cmt, err := range c.ListCommits(context.Background(), githost.RepoRef{Workspace: "acme", Slug: "old"}, time.Time{}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		shas = append(shas, cmt.SHA)
	}
	if len(shas) != 1 || shas[0] != "aaa" {
		t.Errorf("shas = %v, want [aaa]", shas)
	}
	if f.cursor["GET /repos/acme/old/commits"] != 1 {
		t.Errorf("expected 1 hit on original path, got %d", f.cursor["GET /repos/acme/old/commits"])
	}
	if f.cursor["GET /repositories/12345/commits"] != 1 {
		t.Errorf("expected 1 hit on redirected path, got %d", f.cursor["GET /repositories/12345/commits"])
	}
}

// TestDoRequest_RejectsCrossHostRedirect verifies the same-host guard:
// the Authorization header must not leak to a foreign host even if the
// upstream sends a 301 pointing there. The original 301 error surfaces
// instead of being followed.
func TestDoRequest_RejectsCrossHostRedirect(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/repos/acme/old/commits", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://attacker.example.com/repositories/12345/commits")
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = w.Write([]byte(`{"message":"Moved Permanently"}`))
	})

	prevLog := rateLimitLog
	rateLimitLog = io.Discard
	defer func() { rateLimitLog = prevLog }()

	c := newClient(t, f.srv.URL)
	var gotErr error
	for _, err := range c.ListCommits(context.Background(), githost.RepoRef{Workspace: "acme", Slug: "old"}, time.Time{}) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("expected error when redirect points off-host")
	}
	if !strings.Contains(gotErr.Error(), "301") {
		t.Errorf("error = %v, want one mentioning 301", gotErr)
	}
}

// TestDoRequest_BoundsRedirectChain stops a server that perpetually 301s
// at maxRedirects + 1 hops, so we don't loop forever on a misconfigured
// upstream.
func TestDoRequest_BoundsRedirectChain(t *testing.T) {
	f := newFake(t)
	f.route("GET", "/repos/acme/old/commits", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/repositories/1/commits")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	loopHandler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/repositories/1/commits")
		w.WriteHeader(http.StatusMovedPermanently)
	}
	f.route("GET", "/repositories/1/commits",
		loopHandler, loopHandler, loopHandler, loopHandler, loopHandler, loopHandler,
	)

	prevLog := rateLimitLog
	rateLimitLog = io.Discard
	defer func() { rateLimitLog = prevLog }()

	c := newClient(t, f.srv.URL)
	var gotErr error
	for _, err := range c.ListCommits(context.Background(), githost.RepoRef{Workspace: "acme", Slug: "old"}, time.Time{}) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("expected error after exhausting redirect budget")
	}
	if !strings.Contains(gotErr.Error(), "too many redirects") {
		t.Errorf("error = %v, want 'too many redirects'", gotErr)
	}
}

func TestRedirectTarget(t *testing.T) {
	base := "https://api.github.com"
	mk := func(status int, loc string) error {
		h := http.Header{}
		if loc != "" {
			h.Set("Location", loc)
		}
		return &apiclient.StatusError{StatusCode: status, Headers: h}
	}
	cases := []struct {
		name      string
		err       error
		wantPath  string
		wantQuery string
		wantOK    bool
	}{
		{"nil error", nil, "", "", false},
		{"plain error", errors.New("nope"), "", "", false},
		{"500 not redirect", mk(500, "/anywhere"), "", "", false},
		{"301 missing Location", mk(301, ""), "", "", false},
		{"301 same host absolute", mk(301, "https://api.github.com/repositories/1/commits?per_page=100"), "/repositories/1/commits", "per_page=100", true},
		{"301 same host relative", mk(301, "/repositories/1/commits?per_page=100"), "/repositories/1/commits", "per_page=100", true},
		{"302 same host", mk(302, "/repositories/2/issues"), "/repositories/2/issues", "", true},
		{"307 same host", mk(307, "/repositories/3"), "/repositories/3", "", true},
		{"308 same host", mk(308, "/repositories/4"), "/repositories/4", "", true},
		{"cross host blocked", mk(301, "https://evil.example.com/x"), "", "", false},
		{"cross scheme blocked", mk(301, "http://api.github.com/x"), "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, q, ok := redirectTarget(tc.err, base)
			if p != tc.wantPath || q != tc.wantQuery || ok != tc.wantOK {
				t.Errorf("redirectTarget = (%q, %q, %v), want (%q, %q, %v)",
					p, q, ok, tc.wantPath, tc.wantQuery, tc.wantOK)
			}
		})
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
