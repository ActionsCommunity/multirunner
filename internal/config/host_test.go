package config

import (
	"strings"
	"testing"
)

func hostConfig(mutate func(p *Pool)) *Config {
	c := &Config{
		GitHub:       GitHub{Scope: ScopeRepo, Owner: "acme", Repo: "site"},
		Auth:         Auth{PAT: "x"},
		Provisioning: ProvisioningAutoscale,
		Pools: []Pool{{
			Name: "sync", OS: "windows", Backend: "host", Size: 1, WorkFolder: "_work",
			Repository: "acme/site", Workflows: []string{".github/workflows/sync.yml"},
			WorkflowEvent: "schedule", WorkflowActor: "octocat", WorkflowRef: "refs/heads/main",
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
		{"custom version without digest", func(p *Pool) { p.Host.RunnerVersion = "2.400.0" }, "runner_sha256"},
		{"relative dir", func(p *Pool) { p.Host.StateDir = "state" }, "absolute"},
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
