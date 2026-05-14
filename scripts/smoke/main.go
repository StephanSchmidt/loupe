// Command smoke is Loupe's end-to-end test harness. It spins up an
// in-process HTTP server that emulates the Bitbucket Cloud + Jira Cloud
// REST surfaces Loupe consumes, runs `loupe baseline` and `loupe status`
// against it, and asserts the resulting deck and CLI output.
//
// Usage: go run ./scripts/smoke
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("PASS: end-to-end smoke green")
}

func run() error {
	srv := httptest.NewServer(http.HandlerFunc(dispatch))
	defer srv.Close()

	workDir, err := os.MkdirTemp("", "loupe-smoke-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	loupeBin, err := buildLoupe(workDir)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(workDir, "loupe.yaml")
	if err := writeConfig(cfgPath); err != nil {
		return err
	}

	fmt.Println("==> Fake API server:", srv.URL)
	fmt.Println("==> Running loupe baseline")
	if out, err := runCmd(workDir, loupeBin, "baseline",
		"--config", cfgPath,
		"--git-host-token", "fake-bb",
		"--tracker-token", "fake-jira",
		"--git-host-base-url", srv.URL,
		"--tracker-base-url", srv.URL,
	); err != nil {
		return fmt.Errorf("baseline failed:\n%s\n%w", out, err)
	} else {
		fmt.Println(indent(out))
	}

	fmt.Println("==> Running loupe status")
	statusOut, err := runCmd(workDir, loupeBin, "status")
	if err != nil {
		return fmt.Errorf("status failed:\n%s\n%w", statusOut, err)
	}
	fmt.Println(indent(statusOut))

	if err := assertStatus(statusOut); err != nil {
		return err
	}
	if err := assertDeck(workDir); err != nil {
		return err
	}
	return nil
}

// --- subprocess helpers ---

func buildLoupe(workDir string) (string, error) {
	bin := filepath.Join(workDir, "loupe-smoke")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/loupe") // #nosec G204 -- bin is a tempfile under our own workDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build loupe: %w", err)
	}
	return bin, nil
}

func runCmd(workDir, bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...) // #nosec G204 -- bin + args are hard-coded by this smoke harness
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeConfig(path string) error {
	body := `org: smoke-org
git_host:
  provider: bitbucket-cloud
  base_url: http://overridden-by-flag
  username: smoke
tracker:
  provider: jira-cloud
  site: smoke.atlassian.net
  email: smoke@inkmi.com
ai_adoption:
  detection:
    co_author_trailers: true
  min_weekly_commits_for_cutover: 0.10
output:
  path: ./reports
`
	return os.WriteFile(path, []byte(body), 0o600)
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// --- assertions ---

func assertStatus(out string) error {
	for _, want := range []string{
		"Bitbucket:", "Jira:", "Commits:", "Tickets:",
		"AI-tagged",
	} {
		if !strings.Contains(out, want) {
			return fmt.Errorf("status missing %q", want)
		}
	}
	if !strings.Contains(out, "AI-tagged, ") {
		return fmt.Errorf("status missing AI tagged percentage")
	}
	return nil
}

func assertDeck(workDir string) error {
	reports := filepath.Join(workDir, "reports")
	entries, err := os.ReadDir(reports)
	if err != nil {
		return fmt.Errorf("read reports dir: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no run dir under %s", reports)
	}
	runDir := filepath.Join(reports, entries[0].Name())
	for _, rel := range []string{
		"index.html",
		"assets/reveal.js",
		"assets/echarts.min.js",
		"charts/throughput.png",
		"charts/throughput.svg",
		"charts/adoption.png",
		"charts/adoption.svg",
	} {
		info, err := os.Stat(filepath.Join(runDir, rel))
		if err != nil {
			return fmt.Errorf("missing artifact %s: %w", rel, err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("artifact %s is empty", rel)
		}
	}
	return nil
}

// --- HTTP dispatch ---

func dispatch(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/2.0/workspaces":
		respond(w, map[string]any{
			"values": []map[string]string{
				{"slug": "acme", "name": "Acme"},
				{"slug": "beta", "name": "Beta"},
			},
		})
	case r.URL.Path == "/2.0/repositories/acme":
		respond(w, map[string]any{
			"values": []map[string]any{
				{"slug": "backend", "name": "backend", "full_name": "acme/backend", "workspace": map[string]string{"slug": "acme"}},
				{"slug": "frontend", "name": "frontend", "full_name": "acme/frontend", "workspace": map[string]string{"slug": "acme"}},
			},
		})
	case r.URL.Path == "/2.0/repositories/beta":
		respond(w, map[string]any{
			"values": []map[string]any{
				{"slug": "agent", "name": "agent", "full_name": "beta/agent", "workspace": map[string]string{"slug": "beta"}},
			},
		})
	case strings.HasSuffix(r.URL.Path, "/commits"):
		respond(w, map[string]any{"values": fakeCommits(r.URL.Path)})
	case strings.HasSuffix(r.URL.Path, "/pullrequests"):
		respond(w, map[string]any{"values": fakePRs(r.URL.Path)})
	case r.URL.Path == "/rest/api/3/project/search":
		startAt := r.URL.Query().Get("startAt")
		if startAt == "" || startAt == "0" {
			respond(w, map[string]any{
				"startAt": 0, "maxResults": 50, "total": 2, "isLast": true,
				"values": []map[string]string{
					{"id": "1", "key": "ENG", "name": "Engineering"},
					{"id": "2", "key": "OPS", "name": "Ops"},
				},
			})
			return
		}
		respond(w, map[string]any{"isLast": true, "values": []any{}})
	case r.URL.Path == "/rest/api/3/search/jql":
		respond(w, map[string]any{"issues": fakeIssues(r.URL.Query().Get("jql"))})
	default:
		http.NotFound(w, r)
	}
}

func respond(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// fakeCommits returns 12 commits, 5 of which carry a Claude trailer, with
// committed_at staggered across the previous 12 weeks so cutover detection
// has something to chew on.
func fakeCommits(path string) []map[string]any {
	// extract repo segment for variety in SHAs
	repoTag := path
	if u, err := url.Parse(path); err == nil {
		parts := strings.Split(u.Path, "/")
		if len(parts) >= 5 {
			repoTag = parts[3] + parts[4]
		}
	}
	now := time.Now().UTC()
	out := make([]map[string]any, 0, 12)
	for i := 0; i < 12; i++ {
		when := now.AddDate(0, 0, -7*i)
		msg := "chore: routine change " + repoTag
		if i < 5 {
			msg += "\n\nCo-Authored-By: Claude <noreply@anthropic.com>"
		}
		out = append(out, map[string]any{
			"hash":    fmt.Sprintf("%s-c%d", repoTag, i),
			"author":  map[string]any{"raw": "Alice <alice@acme.test>"},
			"date":    when.Format(time.RFC3339),
			"message": msg,
			"parents": []any{},
		})
	}
	return out
}

func fakePRs(path string) []map[string]any {
	now := time.Now().UTC()
	out := make([]map[string]any, 0, 3)
	for i := 0; i < 3; i++ {
		when := now.AddDate(0, 0, -7*i)
		out = append(out, map[string]any{
			"id":           100 + i,
			"title":        fmt.Sprintf("Feature %d (%s)", i, path),
			"state":        "MERGED",
			"author":       map[string]any{"raw": "Alice <alice@acme.test>"},
			"source":       map[string]any{"branch": map[string]any{"name": "feat/x"}},
			"destination":  map[string]any{"branch": map[string]any{"name": "main"}},
			"created_on":   when.Add(-2 * time.Hour).Format(time.RFC3339),
			"updated_on":   when.Format(time.RFC3339),
			"closed_on":    when.Format(time.RFC3339),
			"merge_commit": map[string]string{"hash": fmt.Sprintf("merge-%d", i)},
		})
	}
	return out
}

func fakeIssues(jql string) []map[string]any {
	projectKey := "ENG"
	if strings.Contains(jql, "project = OPS") {
		projectKey = "OPS"
	}
	out := make([]map[string]any, 0, 8)
	now := time.Now().UTC()
	for i := 0; i < 8; i++ {
		when := now.AddDate(0, 0, -3*i)
		out = append(out, map[string]any{
			"id":  fmt.Sprintf("%s-id-%d", projectKey, i),
			"key": fmt.Sprintf("%s-%d", projectKey, i+1),
			"fields": map[string]any{
				"summary":        fmt.Sprintf("Task %d", i+1),
				"issuetype":      map[string]any{"name": "Task"},
				"status":         map[string]any{"name": "Done"},
				"created":        when.Format("2006-01-02T15:04:05.000-0700"),
				"resolutiondate": when.Add(2 * time.Hour).Format("2006-01-02T15:04:05.000-0700"),
				"assignee":       map[string]any{"emailAddress": "alice@acme.test"},
				"project":        map[string]any{"key": projectKey},
				"timeestimate":   3600,
			},
		})
	}
	return out
}
