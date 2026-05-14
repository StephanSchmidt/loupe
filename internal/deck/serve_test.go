package deck

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/StephanSchmidt/loupe/internal/browser"
)

// syncBuffer is a bytes.Buffer guarded by a mutex so the test goroutine can
// poll String() while Serve is still writing status lines from its own
// goroutine. bytes.Buffer alone is not safe for concurrent read/write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestFindLatestRun(t *testing.T) {
	dir := t.TempDir()
	mustMkDeck := func(name string) {
		t.Helper()
		runDir := filepath.Join(dir, name)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "index.html"), []byte("<html></html>"), 0o644); err != nil {
			t.Fatalf("write index for %s: %v", name, err)
		}
	}
	mustMkDeck("2026-01-05T10-00-00Z")
	mustMkDeck("2026-05-13T14-30-00Z")
	mustMkDeck("2026-03-01T09-00-00Z")

	got, err := FindLatestRun(dir)
	if err != nil {
		t.Fatalf("FindLatestRun: %v", err)
	}
	want := filepath.Join(dir, "2026-05-13T14-30-00Z")
	if got != want {
		t.Errorf("FindLatestRun = %q, want %q", got, want)
	}
}

func TestFindLatestRun_SkipsDirsWithoutIndex(t *testing.T) {
	dir := t.TempDir()
	// 9999 has no index — should be ignored even though it's lex-greatest.
	if err := os.MkdirAll(filepath.Join(dir, "9999-bogus"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	good := filepath.Join(dir, "2026-05-13T14-30-00Z")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(good, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := FindLatestRun(dir)
	if err != nil {
		t.Fatalf("FindLatestRun: %v", err)
	}
	if got != good {
		t.Errorf("FindLatestRun = %q, want %q", got, good)
	}
}

func TestFindLatestRun_NoRuns(t *testing.T) {
	dir := t.TempDir()
	if _, err := FindLatestRun(dir); err == nil {
		t.Errorf("expected error when reports dir is empty")
	}
}

func TestServe_ServesIndexAndStopsOnSignal(t *testing.T) {
	deckDir := t.TempDir()
	indexBody := "<!doctype html><title>loupe-test</title>"
	if err := os.WriteFile(filepath.Join(deckDir, "index.html"), []byte(indexBody), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	var statusBuf syncBuffer

	done := make(chan error, 1)
	go func() {
		done <- Serve(deckDir, 0, &statusBuf, browser.NoopOpener{})
	}()

	// Wait for the status line to appear (server is up).
	deadline := time.After(2 * time.Second)
	for !strings.Contains(statusBuf.String(), "Serving") {
		select {
		case <-deadline:
			t.Fatalf("server didn't announce ready in 2s; status: %q", statusBuf.String())
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Extract the URL from the status line and hit it.
	line := statusBuf.String()
	urlStart := strings.Index(line, "http://")
	if urlStart < 0 {
		t.Fatalf("no URL in status: %q", line)
	}
	urlEnd := strings.IndexByte(line[urlStart:], ' ')
	if urlEnd < 0 {
		t.Fatalf("malformed status line: %q", line)
	}
	url := line[urlStart : urlStart+urlEnd]

	resp, err := http.Get(url)
	if err != nil || resp == nil {
		t.Fatalf("GET %s: err=%v resp=%v", url, err, resp)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "loupe-test") {
		t.Errorf("body = %q, want to contain loupe-test", body)
	}

	// Send SIGINT to ourselves — the in-process signal handler in Serve
	// should catch it and shut down gracefully.
	p, _ := os.FindProcess(os.Getpid())
	if err := p.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("self-signal: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve did not stop within 3s of SIGINT")
	}
}
