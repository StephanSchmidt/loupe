// Package apiclient is a small shared HTTP client used by Loupe's provider
// implementations (Bitbucket Cloud, Jira Cloud, …). It handles base-URL
// composition, basic auth, configurable headers, JSON decoding, and
// bounded response bodies. Trimmed-down port of
// `human/cli/internal/apiclient/`.
package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// DefaultTimeout is applied to the built-in *http.Client when no
	// custom doer is provided.
	DefaultTimeout = 30 * time.Second

	// MaxResponseBodyBytes caps DecodeJSON reads so an oversized or
	// misbehaving upstream cannot exhaust memory.
	MaxResponseBodyBytes = 50 * 1024 * 1024

	// maxRetryAfter caps how long we'll sleep in response to a 429
	// Retry-After hint. A misbehaving provider could otherwise stall the
	// run for hours.
	maxRetryAfter = 60 * time.Second

	// idleConnsPerHost keeps enough keep-alive connections open to cover
	// the concurrent ingest fan-out, so requests beyond Go's default of 2
	// don't pay a fresh TCP+TLS handshake each time.
	idleConnsPerHost = 16
)

// defaultBackoff is the retry schedule for transient upstream failures
// (HTTP 429 and 5xx) on idempotent GETs. Bitbucket's rate limit is
// cost-based and can take tens of seconds to clear, so the tail is
// deliberately long but bounded.
var defaultBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	30 * time.Second,
}

// HTTPDoer abstracts request execution for testability.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// AuthFunc applies authentication to an outgoing request.
type AuthFunc func(req *http.Request)

// BasicAuth sets HTTP Basic Authentication on every request.
func BasicAuth(user, password string) AuthFunc {
	return func(req *http.Request) { req.SetBasicAuth(user, password) }
}

// BearerToken sets Authorization: Bearer <token> on every request. Used by
// GitHub-style APIs where a PAT or fine-grained token authenticates without
// a username pair.
func BearerToken(token string) AuthFunc {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// HeaderAuth sets an arbitrary header on every request. GitLab uses
// PRIVATE-TOKEN; some self-hosted instances expect X-Auth-Token.
func HeaderAuth(name, value string) AuthFunc {
	return func(req *http.Request) {
		req.Header.Set(name, value)
	}
}

// NoAuth applies nothing — useful for tests that hit local httptest servers.
func NoAuth() AuthFunc { return func(*http.Request) {} }

// RetryNotifyFunc is called just before the client sleeps to retry a
// transient (429/5xx) failure. attempt is the upcoming attempt number
// (1 = first retry); delay is how long it will wait.
type RetryNotifyFunc func(attempt int, delay time.Duration)

type retryNotifyKey struct{}

// WithRetryNotify attaches a callback that the client invokes before each
// transient-failure retry on requests made with the returned context. Used
// to surface "on backoff" status in progress UIs.
func WithRetryNotify(ctx context.Context, fn RetryNotifyFunc) context.Context {
	return context.WithValue(ctx, retryNotifyKey{}, fn)
}

func retryNotifyFrom(ctx context.Context) RetryNotifyFunc {
	fn, _ := ctx.Value(retryNotifyKey{}).(RetryNotifyFunc)
	return fn
}

// Client is the shared HTTP API client. Not safe for concurrent
// modification — configure once, then call Do.
type Client struct {
	baseURL      string
	auth         AuthFunc
	headers      map[string]string
	providerName string
	http         HTTPDoer
	timeout      time.Duration
	// backoff is the per-attempt sleep schedule for retrying idempotent
	// GETs on 429/5xx. len(backoff) is the maximum number of retries.
	backoff []time.Duration
}

// Option configures a Client.
type Option func(*Client)

// New builds a Client with the given base URL.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: baseURL,
		auth:    NoAuth(),
		headers: make(map[string]string),
		timeout: DefaultTimeout,
		backoff: defaultBackoff,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.http == nil {
		// Clone the default transport and widen its per-host idle pool so
		// concurrent ingest reuses keep-alive connections instead of
		// re-handshaking past Go's default of 2.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConns = 100
		tr.MaxIdleConnsPerHost = idleConnsPerHost
		c.http = &http.Client{
			Timeout:   c.timeout,
			Transport: tr,
			// Don't follow redirects — auth headers would otherwise be
			// replayed to whatever the upstream forwards to.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return c
}

// WithAuth installs an authentication strategy.
func WithAuth(auth AuthFunc) Option { return func(c *Client) { c.auth = auth } }

// WithHeader sets a header included on every request.
func WithHeader(name, value string) Option {
	return func(c *Client) { c.headers[name] = value }
}

// WithProviderName tags this client so error messages identify which
// upstream produced them.
func WithProviderName(name string) Option {
	return func(c *Client) { c.providerName = name }
}

// WithHTTPDoer replaces the default *http.Client. Tests pass an
// httptest-backed client through here.
func WithHTTPDoer(doer HTTPDoer) Option {
	return func(c *Client) { c.http = doer }
}

// WithTimeout adjusts the default client timeout (no effect if
// WithHTTPDoer is also set).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithRetryBackoff overrides the retry schedule for transient (429/5xx)
// failures. Mainly for tests, which pass zero-length sleeps to keep fast;
// a nil or empty schedule disables retries.
func WithRetryBackoff(schedule []time.Duration) Option {
	return func(c *Client) { c.backoff = schedule }
}

// StatusError carries a non-2xx HTTP response so callers can branch on
// the status code (e.g. treat 404 / 409 as "missing" rather than fatal)
// without resorting to string matching on the error message.
type StatusError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
	Provider   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s %s returned %d: %s",
		e.Provider, e.Method, e.Path, e.StatusCode, e.Body)
}

// Do executes an HTTP request against baseURL + path + ?rawQuery. The
// returned response has a non-2xx status surfaced as a *StatusError
// (response body up to 1 KiB is included).
func (c *Client) Do(ctx context.Context, method, path, rawQuery string, body io.Reader) (*http.Response, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL %q: %w", c.baseURL, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("base URL %q must use http or https", c.baseURL)
	}

	u := *base
	u.Path = path
	u.RawQuery = rawQuery
	fullURL := u.String()

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s %s: build request: %w", c.displayName(), method, path, err)
	}

	c.auth(req)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.send(ctx, req, method, path, body == nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.toStatusError(resp, method, path)
	}
	return resp, nil
}

// send executes req, retrying transient failures (429 and 5xx) on
// idempotent GETs (retriable) with bounded backoff. Transport-level errors
// are NOT retried — they surface immediately. A non-retriable or final
// response is returned as-is for the caller to status-check.
func (c *Client) send(ctx context.Context, req *http.Request, method, path string, retriable bool) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s %s %s: %w", c.displayName(), method, path, err)
		}
		if resp == nil {
			return nil, fmt.Errorf("%s %s %s: nil response", c.displayName(), method, path)
		}
		if !retriable {
			return resp, nil
		}
		delay, retry := c.retryDelay(resp, attempt)
		if !retry {
			return resp, nil
		}
		if n := retryNotifyFrom(ctx); n != nil {
			n(attempt+1, delay)
		}
		// Drain + close so the keep-alive connection can be reused, then
		// wait — honouring cancellation — before the next attempt.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s %s %s (waiting to retry): %w", c.displayName(), method, path, ctx.Err())
		case <-time.After(delay):
		}
	}
}

// retryDelay decides whether a response should be retried and how long to
// wait first. 429 honours a usable Retry-After hint (giving up if it's
// longer than maxRetryAfter), otherwise falls back to the backoff
// schedule; 5xx always uses the schedule. attempt indexes c.backoff, so
// retries stop once it's exhausted.
func (c *Client) retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	if attempt >= len(c.backoff) {
		return 0, false
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			if d > maxRetryAfter {
				return 0, false
			}
			return d, true
		}
		return c.backoff[attempt], true
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return c.backoff[attempt], true
	default:
		return 0, false
	}
}

// toStatusError drains a non-2xx response body (capped at 1KiB for the
// returned snippet, rest discarded for keep-alive reuse) and returns a
// StatusError describing it.
func (c *Client) toStatusError(resp *http.Response, method, path string) error {
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return &StatusError{
		StatusCode: resp.StatusCode,
		Method:     method,
		Path:       path,
		Body:       string(respBody),
		Provider:   c.displayName(),
	}
}

func (c *Client) displayName() string {
	if c.providerName != "" {
		return c.providerName
	}
	return "api"
}

// parseRetryAfter converts an HTTP Retry-After header value to a duration.
// Per RFC 9110 §10.2.3 it can be either a non-negative integer (seconds)
// or an HTTP-date. The second return is true when the header carried a
// usable hint — "0" yields (0, true) while an absent/invalid header
// yields (0, false), so callers can distinguish "retry now" from "don't
// retry".
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(value); err == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0, true
		}
		return d, true
	}
	return 0, false
}

// DecodeJSON reads and decodes a JSON response body into dest, then closes
// the body. Response size is capped at MaxResponseBodyBytes.
func DecodeJSON(resp *http.Response, dest interface{}) error {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > MaxResponseBodyBytes {
		return fmt.Errorf("response body exceeds %d bytes", MaxResponseBodyBytes)
	}
	if err := json.Unmarshal(body, dest); err != nil {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return fmt.Errorf("decode JSON: %w (body: %s)", err, snippet)
	}
	return nil
}
