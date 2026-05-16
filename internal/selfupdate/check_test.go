package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"0.1.0", "v0.2.0", true},
		{"0.1.0", "v0.1.0", false},
		{"0.2.0", "v0.1.0", false},
		{"0.1.0", "v0.1.1", true},
		{"0.1.0", "v1.0.0", true},
		{"1.2", "v1.2.0", false},
		{"1.2.0", "v1.2", false},
		{"dev", "v0.1.0", false},
		{"", "v0.1.0", false},
		{"0.1.0", "", false},
		{"0.1.0", "garbage", false},
	}
	for _, c := range cases {
		if got := isNewer(c.cur, c.latest); got != c.want {
			t.Errorf("isNewer(%q,%q)=%v want %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestShouldSkip(t *testing.T) {
	t.Setenv("LOUPE_NO_UPDATE_CHECK", "")
	if !shouldSkip("dev") {
		t.Error("dev build must skip")
	}
	if !shouldSkip("") {
		t.Error("empty version must skip")
	}
	if shouldSkip("0.1.0") {
		t.Error("real version must not skip")
	}
	t.Setenv("LOUPE_NO_UPDATE_CHECK", "1")
	if !shouldSkip("0.1.0") {
		t.Error("env=1 must skip")
	}
	t.Setenv("LOUPE_NO_UPDATE_CHECK", "true")
	if !shouldSkip("0.1.0") {
		t.Error("env=true must skip")
	}
}

func TestCheck_CachedFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "version.json")
	if err := writeCache(path, cacheEntry{CheckedAt: time.Now(), Latest: "v0.9.9"}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	withCachePath(t, path)
	// Point at a server that would fail the test if hit — cache must short-circuit.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("network must not be hit when cache is fresh")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withReleasesURL(t, srv.URL)

	latest, newer := Check(context.Background(), "0.1.0")
	if latest != "v0.9.9" || !newer {
		t.Errorf("want v0.9.9/newer, got %q/%v", latest, newer)
	}
}

func TestCheck_FetchesAndCaches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "version.json")
	withCachePath(t, path)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.5.0"})
	}))
	defer srv.Close()
	withReleasesURL(t, srv.URL)

	latest, newer := Check(context.Background(), "0.1.0")
	if latest != "v0.5.0" || !newer {
		t.Errorf("first call: want v0.5.0/newer, got %q/%v", latest, newer)
	}
	if hits != 1 {
		t.Errorf("first call: want 1 hit, got %d", hits)
	}

	// Second call within TTL must reuse the cache, not re-hit the server.
	latest2, _ := Check(context.Background(), "0.1.0")
	if latest2 != "v0.5.0" {
		t.Errorf("cached call: got %q", latest2)
	}
	if hits != 1 {
		t.Errorf("cached call: server hit again (%d)", hits)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("cache file not written: %v", err)
	}
}

func TestCheck_StaleCacheRefetches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "version.json")
	if err := writeCache(path, cacheEntry{CheckedAt: time.Now().Add(-48 * time.Hour), Latest: "v0.0.1"}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	withCachePath(t, path)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.0"})
	}))
	defer srv.Close()
	withReleasesURL(t, srv.URL)

	latest, _ := Check(context.Background(), "0.1.0")
	if latest != "v0.3.0" {
		t.Errorf("want refreshed v0.3.0, got %q", latest)
	}
}

func TestCheck_NetworkErrorSilent(t *testing.T) {
	dir := t.TempDir()
	withCachePath(t, filepath.Join(dir, "version.json"))
	withReleasesURL(t, "http://127.0.0.1:1") // unroutable; will fail fast

	latest, newer := Check(context.Background(), "0.1.0")
	if latest != "" || newer {
		t.Errorf("network error must return empty/false, got %q/%v", latest, newer)
	}
}

func TestCheck_DevBuildShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("dev build must not contact GitHub")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withReleasesURL(t, srv.URL)

	latest, newer := Check(context.Background(), "dev")
	if latest != "" || newer {
		t.Errorf("dev build: want empty/false, got %q/%v", latest, newer)
	}
}

func withReleasesURL(t *testing.T, url string) {
	t.Helper()
	prev := releasesURL
	releasesURL = url
	t.Cleanup(func() { releasesURL = prev })
}

func withCachePath(t *testing.T, path string) {
	t.Helper()
	prev := cachePath
	cachePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { cachePath = prev })
}
