package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/winvm"
)

func TestBakeCommandExposesIntegrityFlags(t *testing.T) {
	cmd := bakeCmd()
	for _, name := range []string{"iso-sha256", "runner-sha256", "vnc", "vnc-web"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("bake flag --%s is missing", name)
		}
	}
}

func TestInstallerDryRunFlagsExist(t *testing.T) {
	root := rootCmd()
	found := 0
	for _, cmd := range root.Commands() {
		if !strings.HasPrefix(cmd.Name(), "install-") {
			continue
		}
		found++
		if cmd.Flags().Lookup("dry-run") == nil {
			t.Errorf("%s flag --dry-run is missing", cmd.Name())
		}
	}
	if found == 0 {
		t.Fatal("no install-* commands found")
	}
}

func TestOtherMutatingCommandsExposeDryRun(t *testing.T) {
	root := rootCmd()
	if root.Flags().Lookup("dry-run") == nil {
		t.Error("default run flag --dry-run is missing")
	}
	for _, path := range [][]string{{"run"}, {"connect"}, {"bake"}, {"service", "install"}, {"service", "uninstall"}, {"service", "start"}, {"service", "stop"}, {"service", "restart"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Flag("dry-run") == nil {
			t.Errorf("%s flag --dry-run is missing", strings.Join(path, " "))
		}
	}
}

func TestPlanRunReportsStartupMutationsWithoutCreatingThem(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := `github:
  scope: repo
  owner: octo
  repo: hello
auth:
  pat: test
pools:
  - name: linux
    os: linux
    size: 1
    docker:
      host: unix:///var/run/docker.sock
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := planRun(&out, configPath, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no changes", "image=", "runner registrations", "without --dry-run"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, out.String())
		}
	}
}

func TestBakeEmptyVNCWebDisablesDefaultVNC(t *testing.T) {
	cmd := bakeCmd()
	cmd.SetArgs([]string{"--vnc-web=", "--prepare-only"})
	_ = cmd.Execute()
	got, err := cmd.Flags().GetString("vnc")
	if err != nil || got != "" {
		t.Fatalf("--vnc after --vnc-web empty = %q, %v; want disabled", got, err)
	}
}

func TestQEMUHousekeepingRejectsUnexpectedISO(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "windows.iso")
	if err := os.WriteFile(iso, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Pools: []config.Pool{{
		Name: "windows-vm", Backend: "qemu",
		QEMU: config.QEMU{
			Golden: "golden.qcow2", BakeISO: iso,
			BakeISOSHA256: strings.Repeat("0", 64),
			RunnerVersion: winvm.DefaultRunnerVersion,
		},
	}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := runQEMUHousekeeping(t.Context(), cfg, logger); err == nil ||
		!strings.Contains(err.Error(), "Windows ISO SHA256 mismatch") {
		t.Fatalf("housekeeping error = %v", err)
	}
}

func TestDoctorDoesNotCleanQEMUWorkDir(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifacts := []string{"runner.qcow2", "runner.iso", "runner.vars.fd", "runner.serial.log"}
	for _, name := range artifacts {
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := fmt.Sprintf(`github:
  scope: org
  owner: example
auth:
  pat: test
pools:
  - name: vm
    os: windows
    backend: qemu
    qemu:
      golden: %s
      work_dir: %s
`, filepath.ToSlash(filepath.Join(dir, "golden.qcow2")), filepath.ToSlash(workDir))
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = doctor(configPath)
	for _, name := range artifacts {
		if _, err := os.Stat(filepath.Join(workDir, name)); err != nil {
			t.Errorf("doctor changed %s: %v", name, err)
		}
	}
}

func TestPreparePoolCleansQEMUOrphans(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "runner.qcow2")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	pc := config.Pool{
		Name: "vm", OS: "windows", Backend: "qemu",
		QEMU: config.QEMU{Golden: filepath.Join(dir, "missing-golden.qcow2"), WorkDir: dir},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, _ = preparePool(t.Context(), pc, false, false, logger)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still exists after service pool preparation: %v", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

func starvedConfig(provisioning config.Provisioning, size int) *config.Config {
	return &config.Config{
		GitHub: config.GitHub{
			Scope: config.ScopeRepos,
			Owner: "o",
			Repos: []string{"o/a", "o/b", "o/c"},
		},
		Provisioning: provisioning,
		Pools:        []config.Pool{{Name: "linux", OS: "linux", Size: size}},
	}
}

// TestWarnStarvedReposFlagsUndersizedPool covers the misconfiguration that made
// six of eleven repos unrunnable: in pool mode every slot pins to one repo for
// its whole life, so a pool smaller than the repo list strands the remainder.
func TestWarnStarvedReposFlagsUndersizedPool(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		err = warnStarvedRepos(starvedConfig(config.ProvisioningPool, 2))
	})
	if err == nil {
		t.Fatal("undersized pool must make doctor fail")
	}
	if !strings.Contains(out, "WARNING") {
		t.Fatalf("no warning for size=2 repos=3: %q", out)
	}
	for _, want := range []string{"size=2", "repos=3", "1 repo", "autoscale"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q: %q", want, out)
		}
	}
}

func TestWarnStarvedReposSilentWhenPoolCoversEveryRepo(t *testing.T) {
	for _, size := range []int{3, 4} {
		out := captureStdout(t, func() {
			warnStarvedRepos(starvedConfig(config.ProvisioningPool, size))
		})
		if out != "" {
			t.Errorf("size=%d repos=3 warned: %q", size, out)
		}
	}
}

// TestWarnStarvedReposSilentUnderAutoscale pins the reason the warning is
// conditional: autoscale places each runner on the repo that queued the job, so
// a small pool is a capacity choice rather than a starvation bug.
func TestWarnStarvedReposSilentUnderAutoscale(t *testing.T) {
	for _, p := range []config.Provisioning{config.ProvisioningAutoscale, config.ProvisioningWebhook} {
		out := captureStdout(t, func() {
			warnStarvedRepos(starvedConfig(p, 1))
		})
		if out != "" {
			t.Errorf("provisioning=%s warned: %q", p, out)
		}
	}
}

// TestWarnStarvedReposSilentOutsideReposScope pins that single-repo, org, and
// enterprise scopes are unaffected: their runners are not pinned per repo.
func TestWarnStarvedReposSilentOutsideReposScope(t *testing.T) {
	for _, scope := range []config.Scope{config.ScopeRepo, config.ScopeOrg, config.ScopeEnterprise} {
		cfg := starvedConfig(config.ProvisioningPool, 1)
		cfg.GitHub.Scope = scope
		out := captureStdout(t, func() { warnStarvedRepos(cfg) })
		if out != "" {
			t.Errorf("scope=%s warned: %q", scope, out)
		}
	}
}

// TestCheckActionsEnabledFlagsDisabledRepos covers the misconfiguration found on
// this owner's config: six of eleven repos had Actions switched off, so they
// could never queue a job, yet the scaler polled them every interval and looked
// exactly like idle repos.
func TestCheckActionsEnabledFlagsDisabledRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Only o/b has Actions on.
		on := strings.Contains(r.URL.Path, "/repos/o/b/")
		if _, err := fmt.Fprintf(w, `{"enabled":%v}`, on); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	cfg := starvedConfig(config.ProvisioningAutoscale, 3)
	cfg.GitHub.URL = srv.URL
	cfg.Auth = config.Auth{PAT: "x"}

	var err error
	out := captureStdout(t, func() { err = checkActionsEnabled(context.Background(), cfg) })
	if err == nil {
		t.Fatal("want error when repos have Actions disabled, got nil")
	}
	for _, want := range []string{"WARNING", "2 of 3", "o/a", "o/c", "Settings > Actions"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "o/b,") || strings.Contains(out, ", o/b") {
		t.Errorf("enabled repo o/b listed as disabled: %q", out)
	}
}

func TestCheckActionsEnabledSilentWhenAllEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"enabled":true}`); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	cfg := starvedConfig(config.ProvisioningAutoscale, 3)
	cfg.GitHub.URL = srv.URL
	cfg.Auth = config.Auth{PAT: "x"}

	var err error
	out := captureStdout(t, func() { err = checkActionsEnabled(context.Background(), cfg) })
	if err != nil {
		t.Fatalf("want nil error when all repos enabled, got %v", err)
	}
	if out != "" {
		t.Errorf("warned when all repos enabled: %q", out)
	}
}

func TestCheckActionsEnabledFailsOnIncompleteRepoCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/repos/o/b/") {
			http.Error(w, `{"message":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"enabled":true}`)
	}))
	defer srv.Close()

	cfg := starvedConfig(config.ProvisioningAutoscale, 3)
	cfg.GitHub.URL = srv.URL
	cfg.Auth = config.Auth{PAT: "x"}
	var err error
	out := captureStdout(t, func() { err = checkActionsEnabled(context.Background(), cfg) })
	if err == nil {
		t.Fatal("incomplete Actions check must make doctor fail")
	}
	if !strings.Contains(out, "o/b") || !strings.Contains(out, "could not check Actions") {
		t.Fatalf("incomplete output does not identify repo: %q", out)
	}
}

func TestCheckActionsEnabledChecksRepoScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/repos/o/a/actions/permissions" {
			t.Errorf("request path = %q, want repo Actions permissions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"enabled":false}`)
	}))
	defer srv.Close()

	cfg := starvedConfig(config.ProvisioningAutoscale, 1)
	cfg.GitHub = config.GitHub{URL: srv.URL, Scope: config.ScopeRepo, Owner: "o", Repo: "a"}
	cfg.Auth = config.Auth{PAT: "x"}
	var err error
	out := captureStdout(t, func() { err = checkActionsEnabled(context.Background(), cfg) })
	if err == nil || !strings.Contains(out, "o/a") {
		t.Fatalf("single-repo check error = %v, output = %q", err, out)
	}
}

// TestCheckActionsEnabledSkipsNonRepoScope pins that org and enterprise scopes
// make no API calls: they have no concrete per-repo target to validate.
func TestCheckActionsEnabledSkipsNonRepoScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	for _, scope := range []config.Scope{config.ScopeOrg, config.ScopeEnterprise} {
		cfg := starvedConfig(config.ProvisioningAutoscale, 3)
		cfg.GitHub.Scope = scope
		cfg.GitHub.URL = srv.URL
		cfg.Auth = config.Auth{PAT: "x"}

		var err error
		out := captureStdout(t, func() { err = checkActionsEnabled(context.Background(), cfg) })
		if err != nil {
			t.Errorf("scope=%s returned error: %v", scope, err)
		}
		if out != "" {
			t.Errorf("scope=%s produced output: %q", scope, out)
		}
	}
}

// workflowServer serves the three endpoints RepoFilePaths and RepoFile need,
// keyed by "owner/repo" then workflow filename.
func workflowServer(t *testing.T, workflows map[string]map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := strings.TrimPrefix(r.URL.Path, "/api/v3")
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) < 3 || parts[0] != "repos" {
			http.Error(w, "unexpected path "+p, http.StatusNotFound)
			return
		}
		name := parts[1] + "/" + parts[2]
		rest := strings.Join(parts[3:], "/")

		switch {
		case strings.HasPrefix(rest, "git/trees/"):
			var entries []string
			for f := range workflows[name] {
				entries = append(entries, fmt.Sprintf(`{"path":".github/workflows/%s","type":"blob"}`, f))
			}
			// A non-workflow blob pins that the scan filters by path, and a yml
			// one outside .github/workflows pins that it filters by directory
			// and not by extension alone.
			entries = append(entries,
				`{"path":"README.md","type":"blob"}`,
				`{"path":"deploy/runners.yml","type":"blob"}`)
			sort.Strings(entries)
			fmt.Fprintf(w, `{"tree":[%s]}`, strings.Join(entries, ","))
		case strings.HasPrefix(rest, "contents/"):
			if rest == "contents/deploy/runners.yml" {
				fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`,
					base64.StdEncoding.EncodeToString([]byte("# notes on self-hosted runners\n")))
				return
			}
			file := strings.TrimPrefix(rest, "contents/.github/workflows/")
			body, ok := workflows[name][file]
			if !ok {
				http.Error(w, "no such file "+rest, http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`,
				base64.StdEncoding.EncodeToString([]byte(body)))
		default:
			io.WriteString(w, `{"default_branch":"main"}`)
		}
	}))
}

func selfHostedConfig(t *testing.T, srv *httptest.Server) *config.Config {
	t.Helper()
	cfg := starvedConfig(config.ProvisioningAutoscale, 3)
	cfg.GitHub.URL = srv.URL
	cfg.Auth = config.Auth{PAT: "x"}
	return cfg
}

// TestWarnNoSelfHostedWorkflowsFlagsUnusedRepos covers the config that prompted
// this check: eleven repos listed, but only three had a workflow that could ever
// reach the pools. The rest ran entirely on GitHub-hosted runners, so the pools
// looked idle when they were simply never asked for anything.
func TestWarnNoSelfHostedWorkflowsFlagsUnusedRepos(t *testing.T) {
	srv := workflowServer(t, map[string]map[string]string{
		"o/a": {"ci.yml": "jobs:\n  b:\n    runs-on: ubuntu-latest\n"},
		"o/b": {"ci.yml": "jobs:\n  b:\n    runs-on: [self-hosted, Windows, X64]\n"},
		"o/c": {"ci.yml": "jobs:\n  b:\n    runs-on: macos-latest\n"},
	})
	defer srv.Close()

	out := captureStdout(t, func() {
		warnNoSelfHostedWorkflows(context.Background(), selfHostedConfig(t, srv))
	})
	for _, want := range []string{"NOTE", "2 of 3", "o/a", "o/c"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "o/b,") || strings.Contains(out, ", o/b") {
		t.Errorf("self-hosted repo o/b listed as unused: %q", out)
	}
}

func TestWarnNoSelfHostedWorkflowsSilentWhenAllUsed(t *testing.T) {
	srv := workflowServer(t, map[string]map[string]string{
		"o/a": {"ci.yml": "jobs:\n  b:\n    runs-on: [self-hosted, Linux, X64]\n"},
		"o/b": {"ci.yml": "jobs:\n  b:\n    runs-on: [SELF-HOSTED, Windows, X64]\n"},
		"o/c": {"deploy.yaml": "jobs:\n  b:\n    runs-on: [Self-Hosted, Linux, ARM64]\n"},
	})
	defer srv.Close()

	out := captureStdout(t, func() {
		warnNoSelfHostedWorkflows(context.Background(), selfHostedConfig(t, srv))
	})
	if out != "" {
		t.Errorf("warned when every repo targets self-hosted: %q", out)
	}
}

func TestWarnNoSelfHostedWorkflowsFailsOnTruncatedTree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/git/trees/") {
			_, _ = io.WriteString(w, `{"truncated":true,"tree":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"default_branch":"main"}`)
	}))
	defer srv.Close()

	var err error
	out := captureStdout(t, func() {
		err = warnNoSelfHostedWorkflows(context.Background(), selfHostedConfig(t, srv))
	})
	if err == nil {
		t.Fatal("truncated workflow scan must make doctor fail")
	}
	for _, repo := range []string{"o/a", "o/b", "o/c"} {
		if !strings.Contains(out, repo) {
			t.Errorf("truncated scan output missing %s: %q", repo, out)
		}
	}
}

// TestWarnNoSelfHostedWorkflowsIgnoresNonWorkflowFiles pins that a repo is not
// excused by the string appearing outside .github/workflows, and that a .yaml
// extension counts the same as .yml.
func TestWarnNoSelfHostedWorkflowsIgnoresNonWorkflowFiles(t *testing.T) {
	srv := workflowServer(t, map[string]map[string]string{
		"o/a": {"ci.yml": "runs-on: ubuntu-latest\n", "notes.md": "we use self-hosted runners"},
		"o/b": {"ci.yaml": "runs-on: [self-hosted]\n"},
		"o/c": {},
	})
	defer srv.Close()

	out := captureStdout(t, func() {
		warnNoSelfHostedWorkflows(context.Background(), selfHostedConfig(t, srv))
	})
	if !strings.Contains(out, "o/a") {
		t.Errorf("markdown mention excused o/a: %q", out)
	}
	if !strings.Contains(out, "o/c") {
		t.Errorf("repo with no workflows not flagged: %q", out)
	}
	if strings.Contains(out, "o/b") {
		t.Errorf(".yaml workflow not honoured for o/b: %q", out)
	}
}

func TestWarnNoSelfHostedWorkflowsChecksRepoScope(t *testing.T) {
	srv := workflowServer(t, map[string]map[string]string{
		"o/a": {"ci.yml": "runs-on: ubuntu-latest\n"},
	})
	defer srv.Close()

	cfg := selfHostedConfig(t, srv)
	cfg.GitHub.Scope = config.ScopeRepo
	cfg.GitHub.Repo = "a"
	cfg.GitHub.Repos = nil
	out := captureStdout(t, func() { warnNoSelfHostedWorkflows(context.Background(), cfg) })
	if !strings.Contains(out, "heuristic") || !strings.Contains(out, "o/a") {
		t.Fatalf("single-repo workflow output = %q", out)
	}
}

func TestWarnNoSelfHostedWorkflowsSkipsNonRepoScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	for _, scope := range []config.Scope{config.ScopeOrg, config.ScopeEnterprise} {
		cfg := selfHostedConfig(t, srv)
		cfg.GitHub.Scope = scope
		out := captureStdout(t, func() { warnNoSelfHostedWorkflows(context.Background(), cfg) })
		if out != "" {
			t.Errorf("scope=%s produced output: %q", scope, out)
		}
	}
}

func TestRemoteCheckBudgetScalesWithRepoWaves(t *testing.T) {
	cases := []struct {
		repos int
		want  time.Duration
	}{
		{0, remoteCheckTimeout},
		{1, remoteCheckTimeout},
		{repoCheckConcurrency * 4, remoteCheckTimeout},
		{repoCheckConcurrency*4 + 1, 5 * remoteRepoCheckTimeout},
		{repoCheckConcurrency * 10, 10 * remoteRepoCheckTimeout},
	}
	for _, tc := range cases {
		if got := remoteCheckBudget(tc.repos); got != tc.want {
			t.Errorf("remoteCheckBudget(%d) = %v, want %v", tc.repos, got, tc.want)
		}
	}
}

func TestTCGReasonNamesTheActualCause(t *testing.T) {
	cases := []struct {
		detected     bool
		goos, goarch string
		want         string
	}{
		{false, "linux", "amd64", "qemu.accel is set to tcg"},
		{true, "darwin", "arm64", "x86-64 hardware acceleration requires an x86-64 host"},
		{true, "linux", "amd64", "/dev/kvm is missing or not accessible to this user"},
		{true, "freebsd", "amd64", "no hardware accelerator is available on this host"},
	}
	for _, tc := range cases {
		if got := tcgReason(tc.detected, tc.goos, tc.goarch); got != tc.want {
			t.Errorf("tcgReason(%v, %s, %s) = %q, want %q", tc.detected, tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// runnerAccessConfig points an org- or enterprise-scoped config at a stub server.
func runnerAccessConfig(scope config.Scope, srv *httptest.Server) *config.Config {
	return &config.Config{
		GitHub:       config.GitHub{Scope: scope, URL: srv.URL, Owner: "acme"},
		Auth:         config.Auth{PAT: "x"},
		Provisioning: config.ProvisioningPool,
		Pools:        []config.Pool{{Name: "linux", OS: "linux", Size: 1}},
	}
}

// TestCheckRunnerAccessProvesOrgAndEnterpriseCredentials covers the gap this
// check exists for: before it, org and enterprise scope made no GitHub call at
// all, so a bad token or a mistyped slug passed doctor and failed later on a
// runner host at registration time.
func TestCheckRunnerAccessProvesOrgAndEnterpriseCredentials(t *testing.T) {
	cases := []struct {
		name     string
		scope    config.Scope
		status   int
		wantErr  bool
		wantPath string
		wantOut  string
	}{
		{
			name: "org reachable", scope: config.ScopeOrg, status: http.StatusOK,
			wantPath: "/api/v3/orgs/acme/actions/runners", wantOut: "runner API reachable",
		},
		{
			name: "enterprise reachable", scope: config.ScopeEnterprise, status: http.StatusOK,
			wantPath: "/api/v3/enterprises/acme/actions/runners", wantOut: "runner API reachable",
		},
		{
			name: "org forbidden", scope: config.ScopeOrg, status: http.StatusForbidden,
			wantErr: true, wantOut: "cannot manage",
		},
		{
			name: "enterprise slug wrong", scope: config.ScopeEnterprise, status: http.StatusNotFound,
			wantErr: true, wantOut: "visible to these credentials",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					fmt.Fprint(w, `{"total_count":0,"runners":[]}`)
				} else {
					fmt.Fprint(w, `{"message":"nope"}`)
				}
			}))
			defer srv.Close()

			var err error
			out := captureStdout(t, func() {
				err = checkRunnerAccess(context.Background(), runnerAccessConfig(tc.scope, srv))
			})
			if tc.wantErr && err == nil {
				t.Fatal("want doctor failure, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want success, got %v", err)
			}
			if tc.wantPath != "" && gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
			}
			// A trailing slash would hit a different endpoint than the collection.
			if strings.HasSuffix(gotPath, "/") {
				t.Errorf("collection path must not end in a slash: %q", gotPath)
			}
			if tc.status == http.StatusOK && gotQuery != "per_page=1" {
				t.Errorf("query = %q, want per_page=1", gotQuery)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output missing %q: %q", tc.wantOut, out)
			}
		})
	}
}

// TestCheckRunnerAccessSkipsRepoScopes keeps the check off the scopes the
// Actions and workflow phases already exercise.
func TestCheckRunnerAccessSkipsRepoScopes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call to %s", r.URL.Path)
	}))
	defer srv.Close()

	for _, scope := range []config.Scope{config.ScopeRepo, config.ScopeRepos} {
		cfg := runnerAccessConfig(scope, srv)
		cfg.GitHub.Repo = "app"
		if err := checkRunnerAccess(context.Background(), cfg); err != nil {
			t.Errorf("scope %s: want skip, got %v", scope, err)
		}
	}
}

// TestIsTerminalRejectsNullDevice pins the property that made the previous
// os.ModeCharDevice check wrong: the null device is a character device on both
// Windows and Unix, so a CI or service job with stdin redirected there would
// have been treated as interactive and silently taken prompt defaults.
func TestIsTerminalRejectsNullDevice(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Errorf("%s must not count as a terminal", os.DevNull)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("a pipe must not count as a terminal")
	}
}
