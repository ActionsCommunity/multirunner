package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/ghapp"
)

// TestSharedAppForTarget pins which shared App each target authorizes. They are
// separate Apps because no single App should hold both repository
// administration:write and org runner administration.
func TestSharedAppForTarget(t *testing.T) {
	if got := sharedAppFor(connectFlags{repo: "octocat/widgets"}); got.clientID != ghapp.DefaultPersonalClientID || got.slug != ghapp.DefaultPersonalAppSlug {
		t.Errorf("repo target uses %+v, want the repository App", got)
	}
	for _, f := range []connectFlags{{org: "acme"}, {}} {
		if got := sharedAppFor(f); got.clientID != ghapp.DefaultClientID || got.slug != ghapp.DefaultAppSlug {
			t.Errorf("target %+v uses %+v, want the organization App", f, got)
		}
	}
}

// TestRunDeviceConnectRepoTargetTakesPersonalInstall pins that a repository
// target accepts a personal-account installation: the repository App is installed
// wherever the repo lives, so the organization requirement must not apply there.
func TestRunDeviceConnectRepoTargetTakesPersonalInstall(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	var out bytes.Buffer
	personal := ghapp.Installation{ID: 7, Account: "octocat", AppSlug: ghapp.DefaultPersonalAppSlug}
	df := fakeDeviceFlow([][]ghapp.Installation{{personal}})

	if err := runDeviceConnect(cfgPath, connectFlags{repo: "octocat/widgets"}, failingReader{}, &out, false, nil, df); err != nil {
		t.Fatalf("runDeviceConnect: %v", err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"scope: repo", "owner: octocat", "repo: widgets"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config missing %q:\n%s", want, data)
		}
	}
}

// TestRunDeviceConnectRejectsForeignHost pins that a config already pointing at
// GHES is not given a github.com credential: the token would be sent to that host
// on every request.
func TestRunDeviceConnectRejectsForeignHost(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("github:\n  url: https://ghes.example.com\n  scope: org\n  owner: acme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	df := fakeDeviceFlow([][]ghapp.Installation{{orgInstall(1, "acme")}})

	err := runDeviceConnect(cfgPath, connectFlags{org: "acme"}, failingReader{}, &out, true, nil, df)
	if err == nil {
		t.Fatal("expected shared-App login to be refused for a GHES config")
	}
	if !strings.Contains(err.Error(), "ghes.example.com") || !strings.Contains(err.Error(), "--own-app") {
		t.Errorf("error = %v, want it to name the configured host and the --own-app fallback", err)
	}

	data, readErr := os.ReadFile(cfgPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "ghes.example.com") {
		t.Errorf("config was rewritten:\n%s", data)
	}
}

// TestRunDeviceConnectAllowsDotComVariants pins that an existing github.url that
// does name GitHub.com is not treated as a mismatch.
func TestRunDeviceConnectAllowsDotComVariants(t *testing.T) {
	for _, configured := range []string{"https://github.com", "https://github.com/", "https://www.github.com"} {
		t.Run(configured, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(cfgPath, []byte("github:\n  url: "+configured+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			df := fakeDeviceFlow([][]ghapp.Installation{{orgInstall(1, "acme")}})
			if err := runDeviceConnect(cfgPath, connectFlags{org: "acme"}, failingReader{}, &out, false, nil, df); err != nil {
				t.Fatalf("runDeviceConnect: %v", err)
			}
		})
	}
}

// TestSelectInstallationRejectsNamedPersonalAccount pins that --org naming a
// personal account fails: MatchInstallation finds it, but a user installation
// carries no organization runner permission.
func TestSelectInstallationRejectsNamedPersonalAccount(t *testing.T) {
	personal := ghapp.Installation{ID: 9, Account: "octocat", AppSlug: "multirunner-connect"}
	var out bytes.Buffer
	p := newPrompt(failingReader{}, &out)

	_, err := selectInstallation([]ghapp.Installation{personal}, "octocat", "https://github.com", ghapp.DefaultAppSlug, true, p, false)
	if err == nil {
		t.Fatal("expected a personal-account installation to be refused")
	}
	if !strings.Contains(err.Error(), "personal account") {
		t.Errorf("error = %v, want it to say the account is personal", err)
	}
}

// TestUnusableNoteExplainsTheWait pins that an installation which arrived but
// cannot satisfy the target is explained rather than waited on silently — the
// case that reads as a hang.
func TestUnusableNoteExplainsTheWait(t *testing.T) {
	personal := ghapp.Installation{ID: 1, Account: "octocat"}

	if got := unusableNote(nil, ""); got != "" {
		t.Errorf("no installations should produce no note, got %q", got)
	}
	if got := unusableNote([]ghapp.Installation{personal}, ""); !strings.Contains(got, "personal account") {
		t.Errorf("note = %q, want it to say the account is personal", got)
	}
	got := unusableNote([]ghapp.Installation{orgInstall(2, "acme")}, "other-org")
	if !strings.Contains(got, "acme") || !strings.Contains(got, "other-org") {
		t.Errorf("note = %q, want both the installed and the requested account", got)
	}
}

// TestWriteNextStepsPointsAtDoctor pins the follow-up connect prints. The pool
// it writes carries a platform-default docker.host that connect cannot verify,
// so the next command is doctor, which checks it, and run comes after.
func TestWriteNextStepsPointsAtDoctor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "github:\n  scope: org\n  owner: acme\n" + config.ExamplePoolYAML
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	writeNextSteps(&b, path)
	for _, want := range []string{"Next:  multirunner doctor", "docker.host", "Then:  multirunner run"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q:\n%s", want, b.String())
		}
	}
}
