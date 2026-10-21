package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/hostrunner"
)

func hostConfig(mutate func(p *Pool)) *Config {
	c := &Config{
		GitHub:       GitHub{Scope: ScopeRepo, Owner: "acme", Repo: "site"},
		Auth:         Auth{PAT: "x"},
		Provisioning: ProvisioningAutoscale,
		Pools: []Pool{{
			Name: "sync", OS: "windows", Backend: "host", Size: 1, WorkFolder: "_work",
			Repository: "acme/site", Workflows: []string{".github/workflows/sync.yml"},
			WorkflowEvent: "schedule", WorkflowActor: "octocat", WorkflowRef: "main",
		}},
	}
	if mutate != nil {
		mutate(&c.Pools[0])
	}
	return c
}

func TestHostPoolValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *Pool)
		want   string // "" = valid
	}{
		{"allowlisted", nil, ""},
		{"unrestricted opt-in", func(p *Pool) {
			p.Repository, p.Workflows, p.WorkflowEvent, p.WorkflowActor, p.WorkflowRef = "", nil, "", "", ""
			p.Host.AllowUnrestricted = true
		}, ""},
		{"no allowlist", func(p *Pool) {
			p.Repository, p.Workflows, p.WorkflowEvent, p.WorkflowActor, p.WorkflowRef = "", nil, "", "", ""
		}, "full access"},
		{"linux", func(p *Pool) { p.OS = "linux" }, "os: windows only"},
		{"size 2", func(p *Pool) { p.Size = 2 }, "size: 1"},
		{"image", func(p *Pool) { p.Image = "x" }, "has no image"},
		{"docker", func(p *Pool) { p.Docker.Host = "npipe:////./pipe/docker" }, "docker settings"},
		{"container", func(p *Pool) { p.Container.CPUs = 2 }, "container settings"},
		{"tool cache", func(p *Pool) { p.ToolCache.Mode = "shared-volume" }, "tool_cache"},
		{"nested work folder", func(p *Pool) { p.WorkFolder = `a\b` }, "work_folder"},
		{"reserved work folder", func(p *Pool) { p.WorkFolder = `bin` }, "work_folder"},
		{"custom version without digest", func(p *Pool) { p.Host.RunnerVersion = "2.400.0" }, "runner_sha256"},
		{"relative dir", func(p *Pool) { p.Host.StateDir = "state" }, "absolute"},
		{"incomplete UNC dir", func(p *Pool) { p.Host.StateDir = `\\server` }, "absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := hostConfig(tc.mutate).Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestHostPoolsCannotShareDirectories(t *testing.T) {
	c := hostConfig(func(p *Pool) { p.Host.RunnerDir = `C:\mr\runner` })
	second := c.Pools[0]
	second.Name, second.WorkflowEvent = "sync-dispatch", "workflow_dispatch"
	second.Host.RunnerDir = `c:\MR\runner\`
	c.Pools = append(c.Pools, second)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "share host directory") {
		t.Fatalf("want shared directory error, got %v", err)
	}
	c.Pools[1].Host.RunnerDir = `C:\mr\runner2`
	if err := c.Validate(); err != nil {
		t.Fatalf("distinct directories should validate: %v", err)
	}
}

func TestHostDirectoriesCannotAliasOrOverlap(t *testing.T) {
	tests := []struct {
		name   string
		first  string
		second string
	}{
		{"dot segments", `C:\mr\shared`, `c:\MR\other\..\shared`},
		{"nested", `C:\mr\shared`, `C:\mr\shared\state`},
		{"trailing dot alias", `C:\mr\shared`, `C:\mr\shared.`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := hostConfig(func(p *Pool) {
				p.Host.RunnerDir = tc.first
				p.Host.StateDir = `C:\mr\state-one`
			})
			second := c.Pools[0]
			second.Name, second.WorkflowEvent = "sync-dispatch", "workflow_dispatch"
			second.Host.RunnerDir = tc.second
			second.Host.StateDir = `C:\mr\state-two`
			c.Pools = append(c.Pools, second)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "share host directory") {
				t.Fatalf("want overlapping directory error, got %v", err)
			}
		})
	}

	c := hostConfig(func(p *Pool) {
		p.Host.RunnerDir = `C:\mr\runner`
		p.Host.StateDir = `C:\mr\runner\state`
	})
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "share host directory") {
		t.Fatalf("one pool's runner/state directories must not overlap, got %v", err)
	}
}

func TestHostPoolDefaultDirsCollideCaseInsensitively(t *testing.T) {
	c := hostConfig(func(p *Pool) { p.Name = "Nightly" })
	second := c.Pools[0]
	second.Name, second.WorkflowEvent = "nightly", "workflow_dispatch"
	c.Pools = append(c.Pools, second)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "share host directory") {
		t.Fatalf("pool names differing only by case share default dirs; got %v", err)
	}
}

func TestExplicitHostDirCollidesWithAnotherPoolsDefault(t *testing.T) {
	c := hostConfig(nil)
	runnerDir, _ := hostrunner.DefaultDirs("sync")
	second := c.Pools[0]
	second.Name, second.WorkflowEvent = "other", "workflow_dispatch"
	second.Host.RunnerDir = strings.ToUpper(runnerDir)
	if !windowsAbsPath(second.Host.RunnerDir) {
		t.Skip("default dir is not a Windows path on this OS")
	}
	c.Pools = append(c.Pools, second)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "share host directory") {
		t.Fatalf("explicit dir equal to another pool's default should collide; got %v", err)
	}
}

func TestRunnerEnvSecretsCoverDotEnvAndReferences(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_TEST_PAT", "")
	os.Unsetenv("MR_TEST_PAT")
	t.Setenv("MR_TEST_EXTRA", "")
	os.Unsetenv("MR_TEST_EXTRA")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MR_TEST_PAT=secret-pat\nMR_TEST_EXTRA=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "github: {scope: repo, owner: acme, repo: site}\nauth: {pat: \"${MR_TEST_PAT}\"}\nprovisioning: pool\n" +
		"pools: [{name: p, os: linux, docker: {host: \"unix:///var/run/docker.sock\"}}]\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	names, values := c.RunnerEnvSecrets()
	joined := strings.Join(names, ",")
	for _, want := range []string{"MR_TEST_PAT", "MR_TEST_EXTRA"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("names %v missing %s", names, want)
		}
	}
	if len(values) != 1 || values[0] != "secret-pat" {
		t.Fatalf("values = %v", values)
	}
}

func TestFullRefWorkflowRefWarns(t *testing.T) {
	c := hostConfig(func(p *Pool) { p.WorkflowRef = "refs/heads/main" })
	warnings := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(warnings, `will never match`) || !strings.Contains(warnings, `"main"`) {
		t.Fatalf("want a refs/ warning suggesting main, got %q", warnings)
	}
	if w := hostConfig(nil).Warnings(); len(w) != 0 {
		t.Fatalf("branch-name ref should not warn: %v", w)
	}
}

func TestUnknownBackendRejected(t *testing.T) {
	err := hostConfig(func(p *Pool) { p.Backend = "hots" }).Validate()
	if err == nil || !strings.Contains(err.Error(), "backend must be") {
		t.Fatalf("want unknown backend error, got %v", err)
	}
}

func TestHostSettingsRequireHostBackend(t *testing.T) {
	c := hostConfig(func(p *Pool) {
		p.Backend = "docker"
		p.Docker.Host = "unix:///var/run/docker.sock"
		p.Host.KeepWorkspace = true
	})
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "require backend: host") {
		t.Fatalf("want host settings error, got %v", err)
	}
}
