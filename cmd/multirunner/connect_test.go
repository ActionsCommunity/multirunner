package main

import (
	"bytes"
	"errors"
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

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}
