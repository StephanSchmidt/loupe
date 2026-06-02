package progress

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLive_DrivesAndStops exercises the animated reporter from several
// goroutines (as ingest does) and asserts it renders content and that Stop
// returns promptly — i.e. the render goroutine doesn't deadlock or leak.
func TestLive_DrivesAndStops(t *testing.T) {
	var buf bytes.Buffer
	r := Live(&buf, 80)

	r.Listing("acme")
	r.WorkspaceFound("acme", 3, 5) // 2 hidden

	var wg sync.WaitGroup
	for _, name := range []string{"acme/a", "acme/b", "acme/c"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			r.RepoStart(n)
			r.RepoBackoff(n, 1, 2*time.Second)
			r.RepoDone(n, 5, 2)
		}(name)
	}
	wg.Wait()

	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return — render goroutine likely deadlocked")
	}

	out := buf.String()
	if out == "" {
		t.Fatal("expected rendered output, got none")
	}
	// Completed repos are flushed as permanent lines.
	for _, want := range []string{"acme/a", "acme/b", "acme/c", "5 commits"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
}
