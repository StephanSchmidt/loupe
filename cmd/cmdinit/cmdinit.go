package cmdinit

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

const (
	defaultConfigPath       = "loupe.yaml"
	defaultBitbucketBaseURL = "https://api.bitbucket.org/2.0"
)

// configTemplate is the YAML written by the wizard. Mirrors the schema in
// internal/config but with explanatory comments hand-marshalling can't
// preserve. Tokens are intentionally absent — they're prompted at
// `loupe baseline` time.
const configTemplate = `org: {{.Org}}

git_host:
  provider: bitbucket-cloud
  base_url: {{.BitbucketBaseURL}}
  username: {{.BitbucketUsername}}
  # The app password / API token is prompted at every ` + "`loupe baseline`" + ` run.

tracker:
  provider: jira-cloud
  site: {{.JiraSite}}
  email: {{.JiraEmail}}
  # The Jira API token is prompted at every ` + "`loupe baseline`" + ` run.

teams: []
  # - name: platform
  #   members: [alice@acme.com, bob@acme.com]

ai_adoption:
  # cutover_date: 2026-03-15        # uncomment to force a cutover date
  detection:
    co_author_trailers: true
    pr_labels: [ai-assisted]
  min_weekly_commits_for_cutover: 0.05

cycle_time:
  dev_started_statuses: [In Progress, In Development, In Review, Code Review]
  done_statuses: [Done, Closed, Resolved]
  # Terminal "dropped" statuses excluded from work-in-progress + lead-time.
  # Add your workflow's own (won't-do / archived / cancelled) names here.
  abandoned_statuses: []

windows:
  baseline_weeks: 12
  comparison_weeks: 12
  display_months: 6

output:
  path: ./reports
`

type templateData struct {
	Org               string
	BitbucketBaseURL  string
	BitbucketUsername string
	JiraSite          string
	JiraEmail         string
}

func BuildInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Interactive config wizard — writes loupe.yaml",
		Long: `Captures the non-secret coordinates for your Bitbucket and Jira
workspaces and writes loupe.yaml. Tokens are NOT stored — they're prompted
at every ` + "`loupe baseline`" + ` invocation.`,
		SilenceUsage: true,
		RunE:         runInit,
	}
	cmd.Flags().String("out", defaultConfigPath, "where to write the config")
	cmd.Flags().Bool("force", false, "overwrite an existing config")
	return cmd
}

func runInit(cmd *cobra.Command, args []string) error {
	outPath, _ := cmd.Flags().GetString("out")
	force, _ := cmd.Flags().GetBool("force")

	// Fail-fast UX check so the user isn't prompted through the whole
	// wizard before discovering the file exists. The real safety net
	// against a TOCTOU race is the O_EXCL in writeConfig.
	if _, err := os.Stat(outPath); err == nil && !force {
		return fmt.Errorf("%s already exists; pass --force to overwrite", outPath)
	}

	in := bufio.NewReader(cmd.InOrStdin())
	out := cmd.OutOrStdout()

	_, _ = fmt.Fprintln(out, "loupe init — let's bootstrap your loupe.yaml")
	_, _ = fmt.Fprintln(out)

	data, err := collectAnswers(in, out)
	if err != nil {
		return err
	}
	if err := writeConfig(outPath, force, data); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "\nWrote %s.\n", outPath)
	_, _ = fmt.Fprintln(out, "Tokens are NOT stored in the config — `loupe baseline` will prompt for them.")
	_, _ = fmt.Fprintln(out, "\nNext: `loupe baseline`")
	return nil
}

func collectAnswers(in *bufio.Reader, out io.Writer) (templateData, error) {
	var d templateData
	var err error

	if d.Org, err = promptRequired(in, out, "Org label (e.g. acme-eng)", ""); err != nil {
		return d, err
	}
	if d.BitbucketBaseURL, err = promptDefault(in, out, "Bitbucket API base URL", defaultBitbucketBaseURL); err != nil {
		return d, err
	}
	if d.BitbucketUsername, err = promptRequired(in, out, "Bitbucket username or email", ""); err != nil {
		return d, err
	}
	if d.JiraSite, err = promptRequired(in, out, "Jira site (e.g. acme.atlassian.net)", ""); err != nil {
		return d, err
	}
	if d.JiraEmail, err = promptRequired(in, out, "Jira email", ""); err != nil {
		return d, err
	}

	return d, nil
}

func writeConfig(outPath string, force bool, data templateData) error {
	tmpl, err := template.New("config").Parse(configTemplate)
	if err != nil {
		return fmt.Errorf("parse config template: %w", err)
	}
	// Without --force, use O_EXCL so the open atomically refuses to
	// clobber an existing file. The previous Stat → OpenFile sequence had
	// a TOCTOU window where a parallel actor could create the file
	// between the two calls.
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(outPath, flags, 0o600) // #nosec G304 -- wizard's own --out flag
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; pass --force to overwrite", outPath)
		}
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer func() { _ = f.Close() }()
	return tmpl.Execute(f, data)
}

func promptDefault(r *bufio.Reader, w io.Writer, label, def string) (string, error) {
	if def == "" {
		_, _ = fmt.Fprintf(w, "%s: ", label)
	} else {
		_, _ = fmt.Fprintf(w, "%s [%s]: ", label, def)
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

func promptRequired(r *bufio.Reader, w io.Writer, label, def string) (string, error) {
	v, err := promptDefault(r, w, label, def)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return v, nil
}
