package cmdinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StephanSchmidt/loupe/internal/config"
)

func runWizard(t *testing.T, dir, stdin string) (string, error) {
	t.Helper()
	cmd := BuildInitCmd()
	cmd.SetIn(bytes.NewBufferString(stdin))
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--out", filepath.Join(dir, "loupe.yaml")})
	err := cmd.Execute()
	return out.String(), err
}

func TestInit_WritesValidConfigThatLoads(t *testing.T) {
	dir := t.TempDir()
	stdin := strings.Join([]string{
		"acme-eng",           // org
		"",                   // bitbucket base url (accept default)
		"you@example.com",  // bitbucket username
		"acme.atlassian.net", // jira site
		"you@example.com",  // jira email
	}, "\n") + "\n"

	stdout, err := runWizard(t, dir, stdin)
	if err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, stdout)
	}

	cfgPath := filepath.Join(dir, "loupe.yaml")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	t.Logf("wrote:\n%s", body)

	for _, want := range []string{
		"org: acme-eng",
		"provider: bitbucket-cloud",
		"base_url: https://api.bitbucket.org/2.0",
		"username: you@example.com",
		"provider: jira-cloud",
		"site: acme.atlassian.net",
		"email: you@example.com",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("written config missing %q", want)
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("written config doesn't load: %v", err)
	}
	if cfg.Org != "acme-eng" || cfg.GitHost.Provider != config.ProviderBitbucketCloud {
		t.Errorf("loaded cfg = %+v", cfg)
	}
}

func TestInit_RefusesExistingFileWithoutForce(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "loupe.yaml")
	if err := os.WriteFile(cfgPath, []byte("preexisting"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := runWizard(t, dir, "\n\n\n\n\n")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got %v", err)
	}
}

func TestInit_RequiresOrg(t *testing.T) {
	dir := t.TempDir()
	stdin := strings.Join([]string{
		"", // org left blank
		"",
		"u",
		"s",
		"e",
	}, "\n") + "\n"
	_, err := runWizard(t, dir, stdin)
	if err == nil || !strings.Contains(err.Error(), "Org label") {
		t.Errorf("expected required-field error for org, got %v", err)
	}
}
