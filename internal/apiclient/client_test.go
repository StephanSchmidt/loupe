package apiclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func atomicInc(p *int32) int32 { return atomic.AddInt32(p, 1) }

func mustParseHTTPTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := http.ParseTime(s)
	if err != nil {
		t.Fatalf("ParseTime(%q): %v", s, err)
	}
	return v
}

type errDoer struct{ err error }

func (d *errDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }

type nilDoer struct{}

func (*nilDoer) Do(*http.Request) (*http.Response, error) { return nil, nil }

func TestDo_NetworkError(t *testing.T) {
	c := New("https://example.com",
		WithProviderName("test"),
		WithHTTPDoer(&errDoer{err: fmt.Errorf("connection refused")}),
	)
	_, err := c.Do(context.Background(), "GET", "/x", "", nil)
	if err == nil || !strings.Contains(err.Error(), "test GET /x") {
		t.Errorf("expected error wrapping provider+method+path, got %v", err)
	}
}

func TestDo_NilResponse(t *testing.T) {
	c := New("https://example.com",
		WithProviderName("test"),
		WithHTTPDoer(&nilDoer{}),
	)
	_, err := c.Do(context.Background(), "GET", "/x", "", nil)
	if err == nil || !strings.Contains(err.Error(), "nil response") {
		t.Errorf("expected nil-response error, got %v", err)
	}
}

func TestDo_InvalidBaseURL(t *testing.T) {
	c := New("ftp://example.com")
	_, err := c.Do(context.Background(), "GET", "/x", "", nil)
	if err == nil || !strings.Contains(err.Error(), "http or https") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

func TestDo_Success_AuthAndHeaders(t *testing.T) {
	var seen struct {
		auth   string
		ua     string
		accept string
		path   string
		query  string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.auth = r.Header.Get("Authorization")
		seen.ua = r.Header.Get("User-Agent")
		seen.accept = r.Header.Get("Accept")
		seen.path = r.URL.Path
		seen.query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL,
		WithAuth(BasicAuth("user", "pw")),
		WithHeader("User-Agent", "loupe/test"),
	)
	resp, err := c.Do(context.Background(), "GET", "/api/v1/things", "page=2", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	if seen.path != "/api/v1/things" {
		t.Errorf("path = %q, want /api/v1/things", seen.path)
	}
	if seen.query != "page=2" {
		t.Errorf("query = %q, want page=2", seen.query)
	}
	if !strings.HasPrefix(seen.auth, "Basic ") {
		t.Errorf("auth = %q, want Basic prefix", seen.auth)
	}
	if seen.ua != "loupe/test" {
		t.Errorf("UA = %q, want loupe/test", seen.ua)
	}
	if seen.accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", seen.accept)
	}
}

func TestDo_RetriesOn429WithRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomicInc(&calls)
		if n == 1 {
			w.Header().Set("Retry-After", "0") // immediate
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"rate":"limited"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithProviderName("gh"))
	resp, err := c.Do(context.Background(), "GET", "/x", "", nil)
	if err != nil {
		t.Fatalf("Do: %v (want retry to succeed)", err)
	}
	_ = resp.Body.Close()
	if calls != 2 {
		t.Errorf("expected 2 calls (1 + 1 retry), got %d", calls)
	}
}

func TestDo_RetriesOn429WithoutRetryAfter(t *testing.T) {
	// Bitbucket's 429s often lack a Retry-After, so we fall back to the
	// backoff schedule rather than giving up. With a 2-step (zero-delay)
	// schedule an always-429 server is hit 1 + 2 times, then surfaces.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomicInc(&calls)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"rate":"limited"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithProviderName("gh"), WithRetryBackoff([]time.Duration{0, 0}))
	_, err := c.Do(context.Background(), "GET", "/x", "", nil)
	if err == nil {
		t.Fatal("expected 429 to surface as error after retries are exhausted")
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (1 + 2 retries), got %d", calls)
	}
}

func TestDo_RetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomicInc(&calls) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithProviderName("gh"), WithRetryBackoff([]time.Duration{0}))
	resp, err := c.Do(context.Background(), "GET", "/x", "", nil)
	if err != nil {
		t.Fatalf("Do: %v (want 503 then success)", err)
	}
	_ = resp.Body.Close()
	if calls != 2 {
		t.Errorf("expected 2 calls (1 + 1 retry), got %d", calls)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := mustParseHTTPTime(t, "Wed, 21 Oct 2026 07:28:00 GMT")
	cases := []struct {
		in      string
		wantDur time.Duration
		wantOK  bool
	}{
		{"", 0, false},
		{"5", 5 * time.Second, true},
		{"0", 0, true},
		{"-3", 0, false},
		{"not-a-number", 0, false},
		{"Wed, 21 Oct 2026 07:28:30 GMT", 30 * time.Second, true},
		{"Wed, 21 Oct 2026 07:27:00 GMT", 0, true}, // past → retry immediately
	}
	for _, c := range cases {
		gotDur, gotOK := ParseRetryAfter(c.in, now)
		if gotDur != c.wantDur || gotOK != c.wantOK {
			t.Errorf("ParseRetryAfter(%q) = (%v, %v), want (%v, %v)", c.in, gotDur, gotOK, c.wantDur, c.wantOK)
		}
	}
}

func TestDo_Non2xx_SurfacesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithProviderName("jira"))
	_, err := c.Do(context.Background(), "GET", "/x", "", nil)
	if err == nil {
		t.Fatal("expected non-2xx to error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("error didn't surface status+body: %v", err)
	}
}

func TestDecodeJSON_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"alice","age":30}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	resp, err := c.Do(context.Background(), "GET", "/x", "", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	var dst struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	if err := DecodeJSON(resp, &dst); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if dst.Name != "alice" || dst.Age != 30 {
		t.Errorf("decoded = %+v", dst)
	}
}

func TestDecodeJSON_MalformedSurfacesSnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"oops":}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	resp, err := c.Do(context.Background(), "GET", "/x", "", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	var dst struct{ Oops string }
	err = DecodeJSON(resp, &dst)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), `body: {"oops":}`) {
		t.Errorf("error didn't include body snippet: %v", err)
	}
}

func TestIsTransientErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"http2 stream cancel (real format)", errors.New("stream error: stream ID 7825; CANCEL; received from peer"), true},
		{"http2 stream refused", errors.New("stream error: stream ID 12; REFUSED_STREAM"), true},
		{"http2 stream internal", errors.New("stream error: stream ID 5; INTERNAL_ERROR"), true},
		{"connection reset", errors.New("read tcp 1.2.3.4:80: connection reset by peer"), true},
		{"broken pipe", errors.New("write tcp: broken pipe"), true},
		{"http2 goaway", errors.New("http2: server sent GOAWAY and closed the connection"), true},
		{"unexpected EOF wrapped", fmt.Errorf("read response body: %w", io.ErrUnexpectedEOF), true},
		{"plain EOF (not retriable on its own)", io.EOF, false},
		{"status error not transient", &StatusError{StatusCode: 500, Provider: "x", Method: "GET", Path: "/y", Body: "boom"}, false},
		{"wrapped status error not transient", fmt.Errorf("list things: %w", &StatusError{StatusCode: 502}), false},
		{"random error", errors.New("something else"), false},
		{
			"full nested user-observed chain",
			fmt.Errorf("stream PRs acme/feature: %w",
				fmt.Errorf("list PRs acme/feature: %w",
					fmt.Errorf("read response body: %w",
						errors.New("stream error: stream ID 7825; CANCEL; received from peer")))),
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientErr(tc.err); got != tc.want {
				t.Errorf("IsTransientErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
