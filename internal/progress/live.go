package progress

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// spinnerFrames is a braille spinner cycled once per render tick.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	tickInterval   = 100 * time.Millisecond
	maxActiveLines = 12 // cap the live block; excess shown as "…and N more"
	barWidth       = 22
)

var (
	styleDone    = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styleSpin    = lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan
	styleBackoff = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	styleDim     = lipgloss.NewStyle().Faint(true)
	styleBar     = lipgloss.NewStyle().Foreground(lipgloss.Color("4")) // blue
)

type evKind int

const (
	evListing evKind = iota
	evFound
	evStart
	evBackoff
	evDone
	evStop
)

type event struct {
	kind         evKind
	ws, name     string
	count        int // visible repos for evFound
	wsSize       int // workspace true repo total for evFound
	commits, prs int
	attempt      int
	delay        time.Duration
}

type repoState struct {
	name           string
	backoffUntil   time.Time
	backoffAttempt int
}

// live is an inline TTY reporter: completed repos and headers are printed as
// permanent lines that scroll up, while a live block at the bottom (cleared
// and redrawn each tick) shows a progress bar and the active repos with
// spinners — on-backoff repos highlighted in yellow.
type live struct {
	w      io.Writer
	width  int
	events chan event
	done   chan struct{}
}

// Live returns an animated Reporter writing to w, sized to width columns.
// Callers should only use it when w is a terminal; otherwise use Plain.
func Live(w io.Writer, width int) Reporter {
	if width <= 0 {
		width = 80
	}
	l := &live{
		w:      w,
		width:  width,
		events: make(chan event, 256),
		done:   make(chan struct{}),
	}
	go l.run()
	return l
}

func (l *live) send(e event) {
	select {
	case l.events <- e:
	case <-l.done:
	}
}

func (l *live) Listing(ws string) { l.send(event{kind: evListing, ws: ws}) }

func (l *live) WorkspaceFound(ws string, visible, total int) {
	l.send(event{kind: evFound, ws: ws, count: visible, wsSize: total})
}

func (l *live) RepoStart(name string) { l.send(event{kind: evStart, name: name}) }

func (l *live) RepoBackoff(name string, attempt int, d time.Duration) {
	l.send(event{kind: evBackoff, name: name, attempt: attempt, delay: d})
}

func (l *live) RepoDone(name string, commits, prs int) {
	l.send(event{kind: evDone, name: name, commits: commits, prs: prs})
}

func (l *live) Stop() {
	l.send(event{kind: evStop})
	<-l.done
}

func (l *live) run() {
	defer close(l.done)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	active := map[string]*repoState{}
	var (
		total, doneRepos     int
		totalCommits, totPRs int
		frame, prevLive      int
		pending              []string // permanent lines to flush this render
	)

	render := func() {
		live := l.liveBlock(active, frame, doneRepos, total, totalCommits, totPRs)
		// Move up over the previous live block and clear to end of screen,
		// then print the freshly-completed permanent lines (they become
		// scrollback) followed by the new live block.
		var b strings.Builder
		if prevLive > 0 {
			fmt.Fprintf(&b, "\033[%dA", prevLive)
		}
		b.WriteString("\033[0J")
		for _, ln := range pending {
			b.WriteString(l.truncate(ln))
			b.WriteByte('\n')
		}
		for _, ln := range live {
			b.WriteString(ln)
			b.WriteByte('\n')
		}
		_, _ = io.WriteString(l.w, b.String())
		prevLive = len(live)
		pending = nil
	}

	for {
		select {
		case e := <-l.events:
			switch e.kind {
			case evStop:
				// Clear the live block; leave only the permanent lines.
				if prevLive > 0 {
					_, _ = fmt.Fprintf(l.w, "\033[%dA\033[0J", prevLive)
				}
				for _, ln := range pending {
					_, _ = io.WriteString(l.w, l.truncate(ln)+"\n")
				}
				return
			case evListing:
				pending = append(pending, styleDim.Render("  Listing repositories in "+e.ws+"…"))
			case evFound:
				total += e.count
				line := fmt.Sprintf("  %s: %d repositories found", e.ws, e.count)
				if e.wsSize > e.count {
					line += styleBackoff.Render(fmt.Sprintf(" (%d hidden — credential lacks read access)", e.wsSize-e.count))
				}
				pending = append(pending, line)
			case evStart:
				if _, ok := active[e.name]; !ok {
					active[e.name] = &repoState{name: e.name}
				}
			case evBackoff:
				rs := active[e.name]
				if rs == nil {
					rs = &repoState{name: e.name}
					active[e.name] = rs
				}
				rs.backoffUntil = time.Now().Add(e.delay)
				rs.backoffAttempt = e.attempt
			case evDone:
				delete(active, e.name)
				doneRepos++
				totalCommits += e.commits
				totPRs += e.prs
				pending = append(pending, styleDone.Render(
					fmt.Sprintf("  ✓ %s: %d commits, %d PRs", e.name, e.commits, e.prs)))
			}
			render()
		case <-ticker.C:
			frame++
			render()
		}
	}
}

// liveBlock builds the bottom (redrawn) lines: a progress bar summary plus
// one line per active repo.
func (l *live) liveBlock(active map[string]*repoState, frame, done, total, commits, prs int) []string {
	spin := spinnerFrames[frame%len(spinnerFrames)]
	lines := []string{styleSpin.Render(spin) + " " + l.summary(done, total, commits, prs, len(active))}

	names := make([]string, 0, len(active))
	for n := range active {
		names = append(names, n)
	}
	sort.Strings(names)

	now := time.Now()
	shown := names
	extra := 0
	if len(shown) > maxActiveLines {
		extra = len(shown) - maxActiveLines
		shown = shown[:maxActiveLines]
	}
	for _, n := range shown {
		rs := active[n]
		if rs.backoffUntil.After(now) {
			wait := rs.backoffUntil.Sub(now).Round(time.Second)
			lines = append(lines, styleBackoff.Render(
				fmt.Sprintf("    ⏳ %s — rate limited, retry in %s (attempt %d)", n, wait, rs.backoffAttempt)))
		} else {
			lines = append(lines, "    "+styleSpin.Render(spin)+" "+n)
		}
	}
	if extra > 0 {
		lines = append(lines, styleDim.Render(fmt.Sprintf("    …and %d more", extra)))
	}
	return lines
}

func (l *live) summary(done, total, commits, prs, active int) string {
	bar := l.bar(done, total)
	pct := ""
	if total > 0 {
		pct = fmt.Sprintf(" %d/%d repos", done, total)
	} else {
		pct = fmt.Sprintf(" %d repos", done)
	}
	return fmt.Sprintf("%s%s · %d commits · %d PRs · %d active",
		styleBar.Render(bar), pct, commits, prs, active)
}

func (l *live) bar(done, total int) string {
	if total <= 0 {
		return ""
	}
	filled := done * barWidth / total
	if filled > barWidth {
		filled = barWidth
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled) + "]"
}

// truncate clamps a line's *visible* width to the terminal so it never wraps
// (wrapping would break the cursor-up line math). It measures with
// lipgloss.Width, which ignores ANSI escapes.
func (l *live) truncate(s string) string {
	if lipgloss.Width(s) <= l.width {
		return s
	}
	// Cheap clamp: drop runes from the end until it fits. Lines that carry
	// colour are short summaries/headers, so this rarely runs.
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > l.width {
		r = r[:len(r)-1]
	}
	return string(r)
}
