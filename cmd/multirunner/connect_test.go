package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/ghapp"
)

func TestWriteConnectSuccessOmitsCredentials(t *testing.T) {
	creds := &ghapp.Credentials{
		AppID:          42,
		Slug:           "multirunner-test",
		PEM:            "private-key-material",
		InstallationID: 84,
		HTMLURL:        "https://github.com/apps/multirunner-test",
	}
	render := func(webhookSecret string) string {
		var out bytes.Buffer
		creds.WebhookSecret = webhookSecret
		if err := writeConnectSuccess(&out, "config.yaml", "app.pem", creds); err != nil {
			t.Fatalf("writeConnectSuccess: %v", err)
		}
		return out.String()
	}

	const webhookSecret = "webhook-secret-material"
	got := render(webhookSecret)
	if withoutSecret := render(""); got != withoutSecret {
		t.Errorf("webhook secret changed output:\nwith:    %q\nwithout: %q", got, withoutSecret)
	}
	for _, forbidden := range []string{creds.PEM, webhookSecret} {
		if strings.Contains(got, forbidden) {
			t.Errorf("output exposed credential %q: %q", forbidden, got)
		}
	}
	lower := strings.ToLower(got)
	for _, forbidden := range []string{"pat", "ghp_", "github_pat_"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("output contains PAT marker %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "configure webhook mode separately if needed") {
		t.Errorf("output missing safe webhook note: %q", got)
	}
	if !strings.Contains(got, "multirunner run --config config.yaml") {
		t.Errorf("output missing correct follow-up command: %q", got)
	}
	if strings.Contains(got, "multirunner run -config") {
		t.Errorf("output contains invalid follow-up flag: %q", got)
	}
}

func TestWriteConnectSuccessReturnsWriterError(t *testing.T) {
	want := errors.New("write failed")
	err := writeConnectSuccess(errorWriter{err: want}, "config.yaml", "app.pem", &ghapp.Credentials{})
	if !errors.Is(err, want) {
		t.Fatalf("writeConnectSuccess error = %v, want %v", err, want)
	}
}

func TestConnectPlanDoesNotWriteFiles(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	keyPath := filepath.Join(dir, "keys", "runner-app.pem")
	var out bytes.Buffer
	if err := connectPlan(&out, configPath, "octo", "", "runner-app", 4040, keyPath); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"no changes",
		"organization_self_hosted_runners=write",
		configPath,
		"remove auth.pat",
		"local port 4040",
		keyPath,
		"rerun this command without --dry-run",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("dry run created config: %v", err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("dry run created private key: %v", err)
	}
}

func TestConnectPlanRejectsTwoTargets(t *testing.T) {
	var out bytes.Buffer
	err := connectPlan(&out, "config.yaml", "octo", "octo/repo", "runner-app", 0, "")
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("connectPlan error = %v, want exactly-one error", err)
	}
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}
