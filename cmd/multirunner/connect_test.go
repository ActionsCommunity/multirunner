package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/ghapp"
)

// failingReader fails the test if the flow tries to read stdin, proving a
// non-interactive path never prompts.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Error("unexpected stdin read: non-interactive flow must not prompt")
	return 0, io.EOF
}

func TestWriteConnectSuccessOmitsCredentials(t *testing.T) {
	creds := &ghapp.Credentials{
		AppID:          42,
		Slug:           "multirunner-test",
		PEM:            "private-key-material",
		WebhookSecret:  "webhook-secret-material",
		InstallationID: 84,
		HTMLURL:        "https://github.com/apps/multirunner-test",
	}
	var out bytes.Buffer
	if err := writeConnectSuccess(&out, "config.yaml", "app.pem", "app.webhook-secret", "", "", creds); err != nil {
		t.Fatalf("writeConnectSuccess: %v", err)
	}
	got := out.String()
	for _, forbidden := range []string{creds.PEM, creds.WebhookSecret} {
		if strings.Contains(got, forbidden) {
			t.Errorf("output exposed credential %q: %q", forbidden, got)
		}
	}
	lower := strings.ToLower(got)
	for _, forbidden := range []string{"ghp_", "github_pat_"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("output contains PAT marker %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "app.webhook-secret") {
		t.Errorf("output missing webhook secret path: %q", got)
	}
	if !strings.Contains(got, "multirunner run --config config.yaml") {
		t.Errorf("output missing correct follow-up command: %q", got)
	}
}

func TestWriteConnectSuccessWebhookPaths(t *testing.T) {
	creds := &ghapp.Credentials{Slug: "mr", HTMLURL: "https://github.com/apps/mr"}

	var active bytes.Buffer
	if err := writeConnectSuccess(&active, "config.yaml", "app.pem", "", "", "https://mr.example.com/webhook", creds); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(active.String(), "https://mr.example.com/webhook") {
		t.Errorf("active webhook output missing delivery URL: %q", active.String())
	}

	var inactive bytes.Buffer
	if err := writeConnectSuccess(&inactive, "config.yaml", "app.pem", "", "", "", creds); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"placeholder", "workflow_job", "actions:read"} {
		if !strings.Contains(inactive.String(), want) {
			t.Errorf("inactive webhook output missing %q: %q", want, inactive.String())
		}
	}
}

func TestWriteConnectSuccessWarnsOnSecretFailure(t *testing.T) {
	creds := &ghapp.Credentials{Slug: "mr", HTMLURL: "https://github.com/apps/mr"}
	var out bytes.Buffer
	warning := "could not save the webhook secret (disk full); regenerate it under App settings > Webhook secret"
	if err := writeConnectSuccess(&out, "config.yaml", "app.pem", "", warning, "", creds); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "regenerate it") {
		t.Errorf("output missing webhook-secret warning: %q", out.String())
	}
}

func TestWriteConnectSuccessReturnsWriterError(t *testing.T) {
	want := errors.New("write failed")
	err := writeConnectSuccess(errorWriter{err: want}, "config.yaml", "app.pem", "", "", "", &ghapp.Credentials{})
	if !errors.Is(err, want) {
		t.Fatalf("writeConnectSuccess error = %v, want %v", err, want)
	}
}

func TestConnectPlanOrgManifest(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	keyPath := filepath.Join(dir, "keys", "runner-app.pem")
	var out bytes.Buffer
	f := connectFlags{org: "octo", name: "runner-app", port: 4040, keyOut: keyPath}
	if err := connectPlan(&out, configPath, f); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"no changes",
		`"organization_self_hosted_runners": "write"`,
		`"metadata": "read"`,
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
	// Org scope with no detect must not request contents:read.
	if strings.Contains(out.String(), `"contents": "read"`) {
		t.Errorf("org plan unexpectedly requested contents:read:\n%s", out.String())
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("dry run created config: %v", err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("dry run created private key: %v", err)
	}
}

func TestConnectPlanRepoManifest(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	var out bytes.Buffer
	f := connectFlags{repo: "octo/repo", name: "multirunner"}
	if err := connectPlan(&out, configPath, f); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"administration": "write"`,
		`"metadata": "read"`,
		`"contents": "read"`, // repo scope always: doctor's workflow scan
		`"actions": "read"`,  // no config on disk => PollQueue defaults on
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, out.String())
		}
	}
}

func TestConnectPlanRepoWebhook(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	var out bytes.Buffer
	f := connectFlags{repo: "octo/repo", name: "multirunner", webhookURL: "https://mr.example.com/webhook"}
	if err := connectPlan(&out, configPath, f); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"active": true`,
		"workflow_job",
		`"actions": "read"`,
		"https://mr.example.com/webhook",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("webhook plan missing %q:\n%s", want, out.String())
		}
	}
}

// TestConnectRepoActionsReadFollowsProvisioning proves an empty or provisioning-less
// config keeps actions:read for repo scope (so enabling autoscale later does not
// 403), while an explicit provisioning:pool drops it.
func TestConnectRepoActionsReadFollowsProvisioning(t *testing.T) {
	cases := []struct {
		name        string
		configBody  string
		wantActions bool
	}{
		{"no config file", "", true},
		{"empty config file", "\n", true},
		{"provisioning unset", "github:\n  scope: repo\n", true},
		{"provisioning autoscale", "provisioning: autoscale\n", true},
		{"provisioning pool", "provisioning: pool\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			if tc.name != "no config file" {
				if err := os.WriteFile(configPath, []byte(tc.configBody), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			f := connectFlags{repo: "octo/repo", name: "multirunner"}
			if err := connectPlan(&out, configPath, f); err != nil {
				t.Fatal(err)
			}
			gotActions := strings.Contains(out.String(), `"actions": "read"`)
			if gotActions != tc.wantActions {
				t.Errorf("actions:read = %v, want %v\n%s", gotActions, tc.wantActions, out.String())
			}
		})
	}
}

func TestConnectPlanRejectsInvalidWebhookURL(t *testing.T) {
	var out bytes.Buffer
	f := connectFlags{org: "octo", webhookURL: "https://127.0.0.1/webhook"}
	err := connectPlan(&out, "config.yaml", f)
	if err == nil || !strings.Contains(err.Error(), "publicly reachable") {
		t.Fatalf("connectPlan error = %v, want webhook validation error", err)
	}
}

func TestConnectPlanRejectsTwoTargets(t *testing.T) {
	var out bytes.Buffer
	err := connectPlan(&out, "config.yaml", connectFlags{org: "octo", repo: "octo/repo", name: "runner-app"})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("connectPlan error = %v, want exactly-one error", err)
	}
}

func TestConnectCmdNonInteractiveNoTargetErrors(t *testing.T) {
	var out bytes.Buffer
	err := connectCmd("config.yaml", connectFlags{}, failingReader{t}, &out, false, nil)
	if err == nil || !strings.Contains(err.Error(), "specify") {
		t.Fatalf("connectCmd error = %v, want target error", err)
	}
	for _, prompt := range []string{"Scope", "Public https", "contents:read"} {
		if strings.Contains(out.String(), prompt) {
			t.Errorf("non-interactive flow printed a prompt %q: %q", prompt, out.String())
		}
	}
}

func TestConnectCmdRejectsInvalidWebhookBeforePrompt(t *testing.T) {
	var out bytes.Buffer
	// No target and a bad webhook URL: validation must fire before any prompt or
	// listener/browser. failingReader guarantees no stdin read.
	f := connectFlags{webhookURL: "https://10.0.0.1/webhook"}
	err := connectCmd("config.yaml", f, failingReader{t}, &out, true, nil)
	if err == nil || !strings.Contains(err.Error(), "publicly reachable") {
		t.Fatalf("connectCmd error = %v, want webhook validation error before prompt", err)
	}
}

func TestDeriveNeedsNonInteractiveNoRead(t *testing.T) {
	p := newPrompt(failingReader{t}, &bytes.Buffer{})
	needs, err := deriveNeeds(config.ScopeOrg, connectFlags{}, "does-not-exist.yaml", p, false)
	if err != nil {
		t.Fatal(err)
	}
	if needs.WebhookURL != "" || needs.DetectRepo {
		t.Errorf("non-interactive derive should stay at defaults, got %+v", needs)
	}
}

// TestDeriveNeedsDetectIsFlagOnly pins that contents:read is never granted by
// answering a question: an interactive run whose reader says yes to everything
// still gets DetectRepo=false unless --detect was passed.
func TestDeriveNeedsDetectIsFlagOnly(t *testing.T) {
	var out bytes.Buffer
	p := newPrompt(strings.NewReader("y\ny\ny\n"), &out)
	needs, err := deriveNeeds(config.ScopeOrg, connectFlags{org: "o"}, "does-not-exist.yaml", p, true)
	if err != nil {
		t.Fatal(err)
	}
	if needs.DetectRepo {
		t.Error("contents:read must require --detect, not a prompt")
	}
	if strings.Contains(out.String(), "contents:read") {
		t.Errorf("no detect question may be asked, got %q", out.String())
	}

	needs, err = deriveNeeds(config.ScopeOrg, connectFlags{org: "o", detect: true}, "does-not-exist.yaml", p, true)
	if err != nil {
		t.Fatal(err)
	}
	if !needs.DetectRepo {
		t.Error("--detect must request contents:read")
	}
}

func TestConnectPromptTargetFromReader(t *testing.T) {
	p := newPrompt(strings.NewReader("r\nocto/widget\n"), &bytes.Buffer{})
	scope, owner, repo, err := resolveTarget(connectFlags{}, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(scope) != "repo" || owner != "octo" || repo != "widget" {
		t.Errorf("resolved scope=%s owner=%s repo=%s", scope, owner, repo)
	}
}

func TestConnectPromptWebhookRejectsLoopback(t *testing.T) {
	var out bytes.Buffer
	p := newPrompt(strings.NewReader("https://127.0.0.1/webhook\nhttps://mr.example.com/webhook\n"), &out)
	got := p.webhookURL("")
	if got != "https://mr.example.com/webhook" {
		t.Errorf("webhookURL = %q, want the public URL after re-prompt", got)
	}
	if !strings.Contains(out.String(), "publicly reachable") {
		t.Errorf("expected re-prompt to explain rejection: %q", out.String())
	}
}

func TestConnectDevicePlanDescribesDeviceFlow(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	var out bytes.Buffer
	if err := connectDevicePlan(&out, configPath, connectFlags{org: "octo"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"no changes",
		"device flow",
		"Iv23liZGKUct4sAKjq2m",
		"octo (org)",
		"multirunner-user-token.json",
		"auth.client_id + auth.token_path",
		"remove auth.pat",
		"No App is created",
		"rerun this command without --dry-run",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("device plan missing %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("dry run created config: %v", err)
	}
}

func TestConnectDevicePlanRejectsManifestFlags(t *testing.T) {
	var out bytes.Buffer
	err := connectDevicePlan(&out, "config.yaml", connectFlags{org: "octo"}, []string{"--webhook-url", "--port"})
	if err == nil || !strings.Contains(err.Error(), "--own-app") {
		t.Fatalf("error = %v, want manifest-flag rejection pointing at --own-app", err)
	}
	for _, want := range []string{"--webhook-url", "--port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

func TestConnectDeviceCmdRejectsManifestFlagsBeforeNetwork(t *testing.T) {
	// A misused flag must fail before any device code is requested; failingReader
	// proves no prompt, and no network is contacted because rejection is first.
	var out bytes.Buffer
	err := connectDeviceCmd("config.yaml", connectFlags{org: "octo"}, []string{"--name"}, failingReader{t}, &out, false, nil)
	if err == nil || !strings.Contains(err.Error(), "--own-app") {
		t.Fatalf("error = %v, want manifest-flag rejection", err)
	}
}

func TestRejectManifestFlagsSingularPlural(t *testing.T) {
	if err := rejectManifestFlags(nil); err != nil {
		t.Errorf("no misused flags must pass: %v", err)
	}
	one := rejectManifestFlags([]string{"--port"})
	if one == nil || !strings.Contains(one.Error(), " is ") {
		t.Errorf("single flag should read singular: %v", one)
	}
	many := rejectManifestFlags([]string{"--port", "--name"})
	if many == nil || !strings.Contains(many.Error(), " are ") {
		t.Errorf("multiple flags should read plural: %v", many)
	}
}

// TestWantOwnAppPromptBehavior pins the one prompt connect still asks: the
// credential-ownership choice. It is skipped non-interactively (no stdin read),
// defaults to the shared app on an empty answer, and selects the manifest flow
// on "n".
func TestWantOwnAppPromptBehavior(t *testing.T) {
	// Non-interactive: never asked, never reads stdin, chooses the shared app.
	var out bytes.Buffer
	if wantOwnApp(failingReader{t}, newSteps(&out, 3), false) {
		t.Error("non-interactive must choose the shared app")
	}
	if out.String() != "" {
		t.Errorf("non-interactive must not print the question: %q", out.String())
	}

	// Empty answer -> shared app (the default), rendered as step 1 of 3.
	out.Reset()
	if wantOwnApp(strings.NewReader("\n"), newSteps(&out, 3), true) {
		t.Error("empty answer must choose the shared app")
	}
	for _, want := range []string{"[1/3] Authentication", "Shared app", "Own app", "Use the shared app?"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("prompt missing %q:\n%s", want, out.String())
		}
	}

	// "n" -> own app (manifest flow).
	out.Reset()
	if !wantOwnApp(strings.NewReader("n\n"), newSteps(&out, 3), true) {
		t.Error(`answer "n" must choose the own-app manifest flow`)
	}
}

func TestConnectHasOwnAppFlag(t *testing.T) {
	root := rootCmd()
	cmd, _, err := root.Find([]string{"connect"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Flag("own-app") == nil {
		t.Error("connect flag --own-app is missing")
	}
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

// TestDeriveNeedsPromptsForWebhookOnlyUnderAutoscale pins the property that a
// scaleset or pool host is never asked for a public webhook URL: neither mode
// receives a workflow_job delivery, so the question has no valid answer for
// them. Only autoscale, which polls and receives deliveries, is asked.
func TestDeriveNeedsPromptsForWebhookOnlyUnderAutoscale(t *testing.T) {
	cases := map[string]struct {
		config     string
		wantPrompt bool
	}{
		"no config file": {"", false},
		"scaleset":       {"provisioning: scaleset\n", false},
		"pool":           {"provisioning: pool\n", false},
		"autoscale":      {"provisioning: autoscale\n", true},
		"webhook alias":  {"provisioning: webhook\n", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			if tc.config != "" {
				if err := os.WriteFile(cfgPath, []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			p := newPrompt(strings.NewReader("\n"), &out)
			if _, err := deriveNeeds(config.ScopeOrg, connectFlags{org: "o"}, cfgPath, p, true); err != nil {
				t.Fatalf("deriveNeeds: %v", err)
			}
			asked := strings.Contains(out.String(), "Public https URL")
			if asked != tc.wantPrompt {
				t.Errorf("webhook prompt shown = %v, want %v (output %q)", asked, tc.wantPrompt, out.String())
			}
		})
	}
}

func orgInstall(id int64, account string) ghapp.Installation {
	return ghapp.Installation{ID: id, Account: account, IsOrg: true, AppSlug: "multirunner-connect"}
}

// TestSelectInstallationUsesGitHubsAnswers pins that the target comes from the
// installations GitHub reports, not from a login the user has to type: one org is
// taken silently, several are offered as a list.
func TestSelectInstallationUsesGitHubsAnswers(t *testing.T) {
	t.Run("single org taken without asking", func(t *testing.T) {
		p := newPrompt(failingReader{t}, &bytes.Buffer{})
		got, err := selectInstallation([]ghapp.Installation{orgInstall(1, "acme")}, "", "https://github.com", ghapp.DefaultAppSlug, true, p, true)
		if err != nil || got.Account != "acme" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	})

	t.Run("personal installation is not offered", func(t *testing.T) {
		installs := []ghapp.Installation{{ID: 9, Account: "gerard", IsOrg: false, AppSlug: "multirunner-connect"}}
		p := newPrompt(failingReader{t}, &bytes.Buffer{})
		_, err := selectInstallation(installs, "", "https://github.com", ghapp.DefaultAppSlug, true, p, true)
		if err == nil || !strings.Contains(err.Error(), "not installed on any organization") {
			t.Fatalf("want no-org error, got %v", err)
		}
	})

	t.Run("several orgs are listed and picked by number", func(t *testing.T) {
		var out bytes.Buffer
		p := newPrompt(strings.NewReader("2\n"), &out)
		installs := []ghapp.Installation{orgInstall(1, "acme"), orgInstall(2, "globex")}
		got, err := selectInstallation(installs, "", "https://github.com", ghapp.DefaultAppSlug, true, p, true)
		if err != nil {
			t.Fatal(err)
		}
		if got.Account != "globex" {
			t.Errorf("picked %q, want globex", got.Account)
		}
		for _, want := range []string{"1) acme", "2) globex"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("listing missing %q: %q", want, out.String())
			}
		}
	})

	t.Run("several orgs pickable by name", func(t *testing.T) {
		p := newPrompt(strings.NewReader("ACME\n"), &bytes.Buffer{})
		installs := []ghapp.Installation{orgInstall(1, "acme"), orgInstall(2, "globex")}
		got, err := selectInstallation(installs, "", "https://github.com", ghapp.DefaultAppSlug, true, p, true)
		if err != nil || got.Account != "acme" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	})

	t.Run("several orgs non-interactive demands --org", func(t *testing.T) {
		p := newPrompt(failingReader{t}, &bytes.Buffer{})
		installs := []ghapp.Installation{orgInstall(1, "acme"), orgInstall(2, "globex")}
		_, err := selectInstallation(installs, "", "https://github.com", ghapp.DefaultAppSlug, true, p, false)
		if err == nil || !strings.Contains(err.Error(), "--org") {
			t.Fatalf("want error naming --org, got %v", err)
		}
	})

	t.Run("named target that is not installed reports the install URL", func(t *testing.T) {
		p := newPrompt(failingReader{t}, &bytes.Buffer{})
		_, err := selectInstallation([]ghapp.Installation{orgInstall(1, "acme")}, "other", "https://github.com", ghapp.DefaultAppSlug, true, p, true)
		if err == nil || !strings.Contains(err.Error(), "installations/new") {
			t.Fatalf("want install URL, got %v", err)
		}
	})
}
