// Package selfupdate fetches the latest Loupe release tag from GitHub and
// caches it on disk so `loupe status` can surface "update available" without
// hitting the network on every invocation.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	cacheTTL    = 24 * time.Hour
	httpTimeout = 2 * time.Second
)

// releasesURL is a var (not const) so tests can point it at an httptest server.
var releasesURL = "https://api.github.com/repos/StephanSchmidt/loupe/releases/latest"

// cachePath is a var so tests can redirect the cache to t.TempDir().
var cachePath = defaultCachePath

// Check returns the latest release tag and whether it is newer than current.
// On any error, on dev builds, or when LOUPE_NO_UPDATE_CHECK is set, it
// returns ("", false) — callers treat this as "no update notice". Errors are
// swallowed because a network blip must never break `loupe status`.
func Check(ctx context.Context, current string) (latest string, newer bool) {
	if shouldSkip(current) {
		return "", false
	}
	tag, err := cachedLatest(ctx)
	if err != nil || tag == "" {
		return "", false
	}
	return tag, isNewer(current, tag)
}

func shouldSkip(current string) bool {
	if current == "" || current == "dev" {
		return true
	}
	v := os.Getenv("LOUPE_NO_UPDATE_CHECK")
	return v == "1" || strings.EqualFold(v, "true")
}

type cacheEntry struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

func cachedLatest(ctx context.Context) (string, error) {
	path, err := cachePath()
	if err == nil {
		if entry, ok := readCache(path); ok && time.Since(entry.CheckedAt) < cacheTTL {
			return entry.Latest, nil
		}
	}
	tag, err := fetchLatest(ctx)
	if err != nil {
		return "", err
	}
	if path != "" {
		_ = writeCache(path, cacheEntry{CheckedAt: time.Now(), Latest: tag})
	}
	return tag, nil
}

func defaultCachePath() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "loupe", "version.json"), nil
}

func readCache(path string) (cacheEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cacheEntry{}, false
	}
	var e cacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return cacheEntry{}, false
	}
	return e, true
}

func writeCache(path string, entry cacheEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func fetchLatest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "loupe-cli")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: %s", resp.Status)
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.TagName, nil
}

// isNewer compares dotted-integer versions. Pre-release/build suffixes are
// dropped (so "1.2.3-rc1" parses to {1,2,3}); a malformed version on either
// side returns false so a bad tag never spuriously triggers the update line.
func isNewer(current, latest string) bool {
	c, ok := parseVersion(current)
	if !ok {
		return false
	}
	l, ok := parseVersion(latest)
	if !ok {
		return false
	}
	for i := 0; i < len(c) || i < len(l); i++ {
		var ci, li int
		if i < len(c) {
			ci = c[i]
		}
		if i < len(l) {
			li = l[i]
		}
		if li != ci {
			return li > ci
		}
	}
	return false
}

func parseVersion(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
