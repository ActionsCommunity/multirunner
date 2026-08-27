package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
)

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
	out := captureStdout(t, func() {
		warnStarvedRepos(starvedConfig(config.ProvisioningPool, 2))
	})
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

// TestCheckActionsEnabledSkipsNonReposScope pins that org and enterprise scopes
// make no API calls: they have no per-repo list to validate.
func TestCheckActionsEnabledSkipsNonReposScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	for _, scope := range []config.Scope{config.ScopeRepo, config.ScopeOrg, config.ScopeEnterprise} {
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
	body := "jobs:\n  b:\n    runs-on: [self-hosted, Linux, X64]\n"
	srv := workflowServer(t, map[string]map[string]string{
		"o/a": {"ci.yml": body},
		"o/b": {"ci.yml": body},
		"o/c": {"deploy.yaml": body},
	})
	defer srv.Close()

	out := captureStdout(t, func() {
		warnNoSelfHostedWorkflows(context.Background(), selfHostedConfig(t, srv))
	})
	if out != "" {
		t.Errorf("warned when every repo targets self-hosted: %q", out)
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

func TestWarnNoSelfHostedWorkflowsSkipsNonReposScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	for _, scope := range []config.Scope{config.ScopeRepo, config.ScopeOrg, config.ScopeEnterprise} {
		cfg := selfHostedConfig(t, srv)
		cfg.GitHub.Scope = scope
		out := captureStdout(t, func() { warnNoSelfHostedWorkflows(context.Background(), cfg) })
		if out != "" {
			t.Errorf("scope=%s produced output: %q", scope, out)
		}
	}
}
