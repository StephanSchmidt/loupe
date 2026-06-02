// Package progress renders git-host ingest progress. It defines a small
// Reporter interface that the ingest package emits events to, plus three
// implementations: a no-op, a plain line-per-event writer (used when stdout
// isn't a TTY or --plain is set), and an animated inline TTY region with
// spinners, a progress bar, and colour (see live.go).
package progress

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Reporter receives ingest progress events. All methods may be called
// concurrently from multiple goroutines.
type Reporter interface {
	// Listing announces that a workspace's repositories are being listed.
	Listing(workspace string)
	// WorkspaceFound reports how many repositories the workspace holds.
	WorkspaceFound(workspace string, repoCount int)
	// RepoStart marks a repo as actively ingesting.
	RepoStart(fullName string)
	// RepoBackoff reports that a repo's requests are being retried after a
	// 429/5xx, sleeping for delay before attempt number `attempt`.
	RepoBackoff(fullName string, attempt int, delay time.Duration)
	// RepoDone marks a repo finished with its commit and PR counts.
	RepoDone(fullName string, commits, prs int)
	// Stop flushes any final state and releases the terminal. After Stop
	// the Reporter must not be used again.
	Stop()
}

// Nop returns a Reporter that discards every event.
func Nop() Reporter { return nopReporter{} }

type nopReporter struct{}

func (nopReporter) Listing(string)                          {}
func (nopReporter) WorkspaceFound(string, int)              {}
func (nopReporter) RepoStart(string)                        {}
func (nopReporter) RepoBackoff(string, int, time.Duration)  {}
func (nopReporter) RepoDone(string, int, int)               {}
func (nopReporter) Stop()                                   {}

// Plain returns a Reporter that writes one line per event to w. It mirrors
// the previous (pre-animation) output and is used when stdout isn't a
// terminal or the user passed --plain.
func Plain(w io.Writer) Reporter { return &plainReporter{w: w} }

type plainReporter struct {
	mu sync.Mutex
	w  io.Writer
}

func (p *plainReporter) line(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(p.w, format+"\n", a...)
}

func (p *plainReporter) Listing(ws string)             { p.line("  Listing repositories in %s…", ws) }
func (p *plainReporter) WorkspaceFound(ws string, n int) { p.line("  %s: %d repositories found", ws, n) }
func (p *plainReporter) RepoStart(name string)         { p.line("    → %s", name) }

func (p *plainReporter) RepoBackoff(name string, attempt int, d time.Duration) {
	p.line("    ⏳ %s: rate limited, retrying in %s (attempt %d)", name, d.Round(time.Second), attempt)
}

func (p *plainReporter) RepoDone(name string, commits, prs int) {
	p.line("    ✓ %s: %d commits, %d PRs", name, commits, prs)
}

func (p *plainReporter) Stop() {}
