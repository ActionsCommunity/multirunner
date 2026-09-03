package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/ghapp"
)

// fakeDeviceFlow returns a deviceFlow whose network steps are canned. Each call
// to listInstalls advances through installsSeq (holding at the last element), so a
// test can model "empty, then installed".
func fakeDeviceFlow(installsSeq [][]ghapp.Installation) deviceFlow {
	i := 0
	return deviceFlow{
		requestCode: func(context.Context) (*ghapp.DeviceCode, error) {
			return &ghapp.DeviceCode{VerificationURI: "https://github.com/login/device", UserCode: "TEST-CODE"}, nil
		},
		pollToken: func(context.Context, *ghapp.DeviceCode) (*ghapp.UserToken, error) {
			return &ghapp.UserToken{AccessToken: "fake-token"}, nil
		},
		listInstalls: func(context.Context, string) ([]ghapp.Installation, error) {
			out := installsSeq[i]
			if i < len(installsSeq)-1 {
				i++
			}
			return out, nil
		},
		clientID:     "test-client",
		baseURL:      "https://github.com",
		pollInterval: time.Millisecond,
	}
}

func assertStepHeadings(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing step heading %q:\n%s", w, out)
		}
	}
}

func TestRunDeviceConnectInteractiveStepNumbering(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	var out bytes.Buffer
	st := newSteps(&out, 3)
	st.begin("Authentication") // step 1, as main renders via wantOwnApp
	df := fakeDeviceFlow([][]ghapp.Installation{{orgInstall(1, "acme")}})
	if err := runDeviceConnect(cfgPath, connectFlags{org: "acme"}, strings.NewReader(""), &out, true, st, df); err != nil {
		t.Fatal(err)
	}
	assertStepHeadings(t, out.String(),
		"[1/3] Authentication", "[2/3] Authorize on GitHub", "[3/3] Organization")
	for _, forbidden := range []string{"[4/3]", "[1/2]", "[2/2]"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("unexpected step %q:\n%s", forbidden, out.String())
		}
	}
	for _, want := range []string{"using acme", "Connected to acme (org) via device flow", "multirunner doctor --config"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

func TestRunDeviceConnectNonInteractiveStepNumbering(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	var out bytes.Buffer
	df := fakeDeviceFlow([][]ghapp.Installation{{orgInstall(1, "acme")}})
	// failingReader proves the non-interactive path never reads stdin.
	if err := runDeviceConnect(cfgPath, connectFlags{org: "acme"}, failingReader{t}, &out, false, nil, df); err != nil {
		t.Fatal(err)
	}
	assertStepHeadings(t, out.String(), "[1/2] Authorize on GitHub", "[2/2] Organization")
	for _, forbidden := range []string{"[1/3]", "[3/2]", "[3/3]"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("non-interactive must have two steps, saw %q:\n%s", forbidden, out.String())
		}
	}
}

func TestRunDeviceConnectInteractiveWaitsForInstall(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	var out bytes.Buffer
	// Empty on the first poll (authorized but not installed), then an org appears:
	// the flow must wait and continue with the token already held.
	df := fakeDeviceFlow([][]ghapp.Installation{{}, {orgInstall(1, "acme")}})
	if err := runDeviceConnect(cfgPath, connectFlags{}, strings.NewReader(""), &out, true, nil, df); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Waiting for an installation", "installations/new", "found acme", "Connected to acme"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config not written after wait resolved: %v", err)
	}
}

func TestRunDeviceConnectNonInteractiveNotInstalledFailsFast(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	var out bytes.Buffer
	df := fakeDeviceFlow([][]ghapp.Installation{{}}) // never installed
	err := runDeviceConnect(cfgPath, connectFlags{org: "acme"}, failingReader{t}, &out, false, nil, df)
	if err == nil || !strings.Contains(err.Error(), "installations/new") {
		t.Fatalf("want fail-fast install URL error, got %v", err)
	}
	if strings.Contains(out.String(), "Waiting for an installation") {
		t.Errorf("non-interactive must not wait:\n%s", out.String())
	}
	if _, statErr := os.Stat(cfgPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed connect must not write config: %v", statErr)
	}
}

func TestRunOwnAppConnectStepNumbering(t *testing.T) {
	fakeConnect := func(context.Context, ghapp.Options) (*ghapp.Credentials, error) {
		return &ghapp.Credentials{Slug: "mr", AppID: 1, InstallationID: 2, PEM: "key", HTMLURL: "https://github.com/apps/mr"}, nil
	}

	t.Run("interactive continues the auth numbering", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		var out bytes.Buffer
		st := newSteps(&out, 3)
		st.begin("Authentication")
		if err := runOwnAppConnect(cfgPath, connectFlags{org: "acme"}, strings.NewReader(""), &out, true, st, fakeConnect); err != nil {
			t.Fatal(err)
		}
		assertStepHeadings(t, out.String(),
			"[1/3] Authentication", "[2/3] Create and install the App on GitHub", "[3/3] Save credentials")
		if strings.Contains(out.String(), "[4/3]") {
			t.Errorf("step numbering overflowed:\n%s", out.String())
		}
	})

	t.Run("own-app flag numbers two steps", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		var out bytes.Buffer
		if err := runOwnAppConnect(cfgPath, connectFlags{org: "acme"}, failingReader{t}, &out, false, nil, fakeConnect); err != nil {
			t.Fatal(err)
		}
		assertStepHeadings(t, out.String(),
			"[1/2] Create and install the App on GitHub", "[2/2] Save credentials")
		if strings.Contains(out.String(), "[3/") || strings.Contains(out.String(), "[1/3]") {
			t.Errorf("own-app flag flow must have two steps:\n%s", out.String())
		}
	})
}

func TestAwaitInstallationWaitsThenResolves(t *testing.T) {
	var out bytes.Buffer
	d := &stepDetail{out: &out}
	calls := 0
	list := func(context.Context) ([]ghapp.Installation, error) {
		calls++
		if calls < 3 {
			return nil, nil
		}
		return []ghapp.Installation{orgInstall(1, "acme")}, nil
	}
	got, err := awaitInstallation(context.Background(), list, "", "https://github.com", ghapp.DefaultAppSlug, d, true, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Account != "acme" {
		t.Fatalf("resolved installs %+v", got)
	}
	for _, want := range []string{"Waiting for an installation", "installations/new"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
}

func TestAwaitInstallationNonInteractiveDoesNotWait(t *testing.T) {
	var out bytes.Buffer
	d := &stepDetail{out: &out}
	calls := 0
	list := func(context.Context) ([]ghapp.Installation, error) {
		calls++
		return nil, nil // never installed
	}
	got, err := awaitInstallation(context.Background(), list, "acme", "https://github.com", ghapp.DefaultAppSlug, d, false, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("non-interactive must poll once and return, polled %d times", calls)
	}
	if out.String() != "" {
		t.Errorf("non-interactive must not print a wait line: %q", out.String())
	}
	// The fail-fast install URL is then surfaced by selectInstallation.
	p := newPrompt(failingReader{t}, &bytes.Buffer{})
	if _, err := selectInstallation(got, "acme", "https://github.com", ghapp.DefaultAppSlug, true, p, false); err == nil || !strings.Contains(err.Error(), "installations/new") {
		t.Fatalf("want fail-fast install URL, got %v", err)
	}
}

func TestAwaitInstallationHonorsContextCancellation(t *testing.T) {
	var out bytes.Buffer
	d := &stepDetail{out: &out}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	list := func(context.Context) ([]ghapp.Installation, error) { return nil, nil }
	if _, err := awaitInstallation(ctx, list, "acme", "https://github.com", ghapp.DefaultAppSlug, d, true, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestOwnAppConnectWritesOwnerOnlyCredentials pins that the App private key and
// the webhook secret get the same protection as the token sidecar. os.WriteFile's
// mode argument is ignored on Windows, so writing the most valuable credential in
// the manifest flow with a plain 0600 left it readable by every local account.
func TestOwnAppConnectWritesOwnerOnlyCredentials(t *testing.T) {
	fakeConnect := func(context.Context, ghapp.Options) (*ghapp.Credentials, error) {
		return &ghapp.Credentials{
			Slug: "mr", AppID: 1, InstallationID: 2,
			PEM:           "-----BEGIN RSA PRIVATE KEY-----\nnot-a-real-key\n-----END RSA PRIVATE KEY-----\n",
			WebhookSecret: "s3cret",
			HTMLURL:       "https://github.com/apps/mr",
		}, nil
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	var out bytes.Buffer
	if err := runOwnAppConnect(cfgPath, connectFlags{org: "acme"}, failingReader{t}, &out, false, nil, fakeConnect); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"multirunner-app.private-key.pem", "multirunner-app.webhook-secret"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
		if err := ghapp.CheckOwnerOnly(path); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
