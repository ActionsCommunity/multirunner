package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	imageversions "github.com/GerardSmit/multirunner/images"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	p := writeConfig(t, `
github:
  scope: org
  owner: myorg
auth:
  pat: ghp_x
pools:
  - name: linux-pool
    os: linux
    docker:
      host: tcp://127.0.0.1:2375
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.GitHub.URL != "https://github.com" {
		t.Errorf("default url = %q", c.GitHub.URL)
	}
	if c.Provisioning != ProvisioningPool {
		t.Errorf("default provisioning = %q", c.Provisioning)
	}
	p0 := c.Pools[0]
	if p0.Size != 1 || p0.WorkFolder != "_work" || p0.NamePrefix != "multirunner" || p0.RunnerGroupID != 1 {
		t.Errorf("pool defaults not applied: %+v", p0)
	}
	if p0.ImageTier != "minimal" || p0.MaxConsecutiveFailures != 5 {
		t.Errorf("pool defaults not applied: %+v", p0)
	}
}

func TestLoadQEMUBakeChecksums(t *testing.T) {
	p := writeConfig(t, `
github: {scope: org, owner: myorg}
auth: {pat: ghp_x}
pools:
  - name: windows-vm
    os: windows
    backend: qemu
    qemu:
      golden: golden.qcow2
      bake_iso: windows.iso
      bake_iso_sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      runner_version: 1.2.3
      runner_sha256: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	qemu := c.Pools[0].QEMU
	if qemu.BakeISOSHA256 != strings.Repeat("a", 64) || qemu.RunnerSHA256 != strings.Repeat("b", 64) {
		t.Fatalf("QEMU checksums not loaded: %+v", qemu)
	}
}

func TestLoadRejectsUnknownQEMUToolSelectors(t *testing.T) {
	// bake_iso is deliberately absent: without load-time validation a typo here
	// is never reported at all, because no rebuild ever resolves the selectors.
	for name, tools := range map[string]string{
		"unknown node major": `[node:23]`,
		"unknown kind":       `[nodejs]`,
		"trailing colon":     `[buildtools:]`,
	} {
		t.Run(name, func(t *testing.T) {
			p := writeConfig(t, `
github: {scope: org, owner: myorg}
auth: {pat: ghp_x}
pools:
  - name: windows-vm
    os: windows
    backend: qemu
    qemu:
      golden: golden.qcow2
      tools: `+tools+"\n")
			_, err := Load(p)
			if err == nil {
				t.Fatalf("tools %s should fail config load", tools)
			}
			if !strings.Contains(err.Error(), "windows-vm") || !strings.Contains(err.Error(), "qemu.tools") {
				t.Fatalf("error should name the pool and field: %v", err)
			}
		})
	}

	p := writeConfig(t, `
github: {scope: org, owner: myorg}
auth: {pat: ghp_x}
pools:
  - name: windows-vm
    os: windows
    backend: qemu
    qemu:
      golden: golden.qcow2
      tools: [dotnet, "node:24", "buildtools:17", "buildtools:18"]
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("valid selectors rejected: %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"repo without repo": `
github: {scope: repo, owner: o}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"no auth": `
github: {scope: org, owner: o}
auth: {}
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"bad scope": `
github: {scope: user, owner: o}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"no pools": `
github: {scope: org, owner: o}
auth: {pat: x}
pools: []`,
		"bad os": `
github: {scope: org, owner: o}
auth: {pat: x}
pools: [{name: p, os: bsd, docker: {host: h}}]`,
		"dup pool": `
github: {scope: org, owner: o}
auth: {pat: x}
pools:
  - {name: p, os: linux, docker: {host: h}}
  - {name: p, os: windows, docker: {host: h2}}`,
		"repos without repos list": `
github: {scope: repos, owner: o}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"repos without owner": `
github: {scope: repos, repos: [a, b]}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"repos blank entry": `
github: {scope: repos, owner: o, repos: [a, " "]}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"repos empty owner": `
github: {scope: repos, owner: o, repos: [/a]}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"repos empty repo": `
github: {scope: repos, owner: o, repos: [a/]}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"repos extra slash": `
github: {scope: repos, owner: o, repos: [o/a/extra]}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"repos duplicate case": `
github: {scope: repos, owner: o, repos: [a, O/A]}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Errorf("expected validation error for %q", name)
			}
		})
	}
}

func TestLoadScalesetProvisioning(t *testing.T) {
	p := writeConfig(t, `
github: {scope: org, owner: o}
auth: {pat: x}
provisioning: scaleset
pools:
  - {name: linux, os: linux, scale_set: linux-runners, docker: {host: h}}
  - {name: windows, os: windows, scale_set: windows-runners, docker: {host: h2}}
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Provisioning.IsScaleset() {
		t.Fatalf("provisioning = %q, want scaleset", c.Provisioning)
	}
}

func TestScalesetProvisioningRequiresUniqueNames(t *testing.T) {
	cases := map[string]string{
		"missing scale set": `
github: {scope: org, owner: o}
auth: {pat: x}
provisioning: scaleset
pools: [{name: p, os: linux, docker: {host: h}}]`,
		"duplicate scale set": `
github: {scope: org, owner: o}
auth: {pat: x}
provisioning: scaleset
pools:
  - {name: p1, os: linux, scale_set: shared, docker: {host: h}}
  - {name: p2, os: linux, scale_set: shared, docker: {host: h}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestPATEnvExpansion(t *testing.T) {
	t.Setenv("MR_TEST_PAT", "ghp_fromenv")
	p := writeConfig(t, `
github: {scope: org, owner: o}
auth: {pat: "${MR_TEST_PAT}"}
pools: [{name: p, os: linux, docker: {host: h}}]`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Auth.PAT != "ghp_fromenv" {
		t.Errorf("PAT = %q, want ghp_fromenv", c.Auth.PAT)
	}
}

func TestPATLiteralUnchanged(t *testing.T) {
	if got := expandEnvRef("ghp_literal"); got != "ghp_literal" {
		t.Errorf("expandEnvRef(literal) = %q", got)
	}
}

func TestImageRef(t *testing.T) {
	cases := []struct {
		os, tier, explicit, want string
	}{
		{"linux", "minimal", "", "gerardsmit/multirunner-runner-linux:latest"},
		{"linux", "", "", "gerardsmit/multirunner-runner-linux:latest"},
		{"linux", "dotnet", "", "gerardsmit/multirunner-runner-linux:dotnet"},
		{"linux", "node", "", "gerardsmit/multirunner-runner-linux:node"},
		{"linux", "native-build", "", "gerardsmit/multirunner-runner-linux:native-build"},
		{"linux", "rust", "", "gerardsmit/multirunner-runner-linux:rust"},
		{"linux", "go", "", "gerardsmit/multirunner-runner-linux:go"},
		{"windows", "node", "", "gerardsmit/multirunner-runner-windows:node"},
		{"windows", "dotnet", "", "gerardsmit/multirunner-runner-windows:dotnet"},
		{"windows", "buildtools", "", "gerardsmit/multirunner-runner-windows:buildtools"},
		{"windows", "buildtools:17", "", "gerardsmit/multirunner-runner-windows:buildtools-17"},
		{"windows", "buildtools:18", "", "gerardsmit/multirunner-runner-windows:buildtools-18"},
		{"linux", "custom", "", "multirunner/runner-linux-custom:dev"},
		{"windows", "minimal", "", "gerardsmit/multirunner-runner-windows:latest"},
		{"windows", "rust", "", "multirunner/runner-windows-rust:dev"},
		{"linux", "minimal", "ghcr.io/me/x:1", "ghcr.io/me/x:1"},
	}
	for _, c := range cases {
		p := Pool{OS: c.os, ImageTier: c.tier, Image: c.explicit}
		if got := p.ImageRef(); got != c.want {
			t.Errorf("ImageRef(os=%s tier=%s explicit=%s) = %q, want %q", c.os, c.tier, c.explicit, got, c.want)
		}
	}
}

func TestImageRefBuildToolsLinesFollowManifest(t *testing.T) {
	for _, line := range imageversions.MustEmbedded().BuildTools.ReleaseLines() {
		p := Pool{OS: "windows", ImageTier: "buildtools:" + line}
		want := "gerardsmit/multirunner-runner-windows:buildtools-" + line
		if got := p.ImageRef(); got != want {
			t.Errorf("ImageRef(buildtools:%s) = %q, want %q", line, got, want)
		}
	}
}

func TestValidateRejectsUnbuildableImageTier(t *testing.T) {
	cases := map[string]struct {
		os, tier string
	}{
		"published windows line on a linux pool": {"linux", "buildtools:17"},
		"unknown build tools line":               {"windows", "buildtools:19"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeConfig(t, `
github: {scope: org, owner: o}
auth: {pat: x}
pools: [{name: broken-pool, os: `+tc.os+`, image_tier: "`+tc.tier+`", docker: {host: h}}]`)
			_, err := Load(p)
			if err == nil {
				t.Fatalf("expected validation error for tier %q on os=%s", tc.tier, tc.os)
			}
			for _, want := range []string{"broken-pool", tc.tier, "minimal"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestValidateAllowsLocalDevImageTier(t *testing.T) {
	p := writeConfig(t, `
github: {scope: org, owner: o}
auth: {pat: x}
pools: [{name: dev-pool, os: linux, image_tier: custom, docker: {host: h}}]`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v (a colon-free unknown tier must keep the local :dev fallback)", err)
	}
	if got := c.Pools[0].ImageRef(); got != "multirunner/runner-linux-custom:dev" {
		t.Errorf("ImageRef = %q", got)
	}
}

func TestValidateAllowsPublishedBuildToolsTier(t *testing.T) {
	p := writeConfig(t, `
github: {scope: org, owner: o}
auth: {pat: x}
pools: [{name: win-pool, os: windows, image_tier: "buildtools:17", docker: {host: h}}]`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Pools[0].ImageRef(); got != "gerardsmit/multirunner-runner-windows:buildtools-17" {
		t.Errorf("ImageRef = %q", got)
	}
}

func TestWarningsQEMUImageTier(t *testing.T) {
	c := &Config{Pools: []Pool{
		{Name: "vm", Backend: "qemu", ImageTier: "dotnet"},
		{Name: "vm-ok", Backend: "qemu", ImageTier: "minimal"},
		{Name: "docker", ImageTier: "dotnet"},
	}}
	w := c.Warnings()
	if len(w) != 1 {
		t.Fatalf("warnings = %v, want 1 (only the qemu+image_tier pool)", w)
	}
	if !strings.Contains(w[0], "vm") || !strings.Contains(w[0], "qemu") {
		t.Errorf("unexpected warning: %q", w[0])
	}
}

func TestToolCachePath(t *testing.T) {
	if got := (Pool{OS: "linux"}).ToolCachePath(); got != "/opt/hostedtoolcache" {
		t.Errorf("linux tool cache = %q", got)
	}
	if got := (Pool{OS: "windows"}).ToolCachePath(); got != `C:\hostedtoolcache\windows` {
		t.Errorf("windows tool cache = %q", got)
	}
}

func TestGitCacheEnabled(t *testing.T) {
	if (GitCache{Mode: "off"}).Enabled() {
		t.Error("off should be disabled")
	}
	if (GitCache{Mode: "mirror"}).Enabled() {
		t.Error("mirror without path should be disabled")
	}
	if !(GitCache{Mode: "mirror", Path: "/x"}).Enabled() {
		t.Error("mirror with path should be enabled")
	}
}

func TestWriteAppAuth(t *testing.T) {
	p := writeConfig(t, `
github:
  url: https://github.com
  scope: org
  owner: oldorg
auth:
  pat: ghp_old
pools:
  - {name: linux-pool, os: linux, docker: {host: tcp://127.0.0.1:2375}}
`)
	if err := WriteAppAuth(p, ScopeOrg, "neworg", "", 111, 222, "/keys/app.pem"); err != nil {
		t.Fatalf("WriteAppAuth: %v", err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c.Auth.PAT != "" {
		t.Errorf("pat not removed: %q", c.Auth.PAT)
	}
	if !c.Auth.IsApp() || c.Auth.AppID != 111 || c.Auth.InstallationID != 222 || c.Auth.PrivateKeyPath != "/keys/app.pem" {
		t.Errorf("app auth not written: %+v", c.Auth)
	}
	if c.GitHub.Owner != "neworg" || c.GitHub.Scope != ScopeOrg {
		t.Errorf("github not updated: %+v", c.GitHub)
	}
	if len(c.Pools) != 1 || c.Pools[0].Name != "linux-pool" {
		t.Errorf("pools not preserved: %+v", c.Pools)
	}
}

func TestWriteAppAuthCreatesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "new.yaml")
	if err := WriteAppAuth(p, ScopeRepo, "octo", "hello", 1, 2, "/k.pem"); err != nil {
		t.Fatalf("WriteAppAuth: %v", err)
	}
	// File now has github+auth but no pools yet; validate the auth/github pieces directly.
	data, _ := os.ReadFile(p)
	for _, want := range []string{"app_id", "installation_id", "private_key_path", "octo", "hello"} {
		if !contains(string(data), want) {
			t.Errorf("written config missing %q:\n%s", want, data)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestAppAuthValid(t *testing.T) {
	p := writeConfig(t, `
github: {scope: enterprise, owner: ent}
auth: {app_id: 1, installation_id: 2, private_key_path: /tmp/k.pem}
pools: [{name: p, os: linux, docker: {host: h}}]`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Auth.IsApp() {
		t.Error("IsApp = false, want true")
	}
}

func TestScopeReposValid(t *testing.T) {
	p := writeConfig(t, `
github:
  scope: repos
  owner: octocat
  repos: [repo-a, repo-b, repo-c]
auth:
  pat: ghp_x
pools:
  - name: linux-pool
    os: linux
    docker:
      host: tcp://127.0.0.1:2375
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GitHub.Scope != ScopeRepos {
		t.Errorf("scope = %q, want repos", c.GitHub.Scope)
	}
	if c.GitHub.Owner != "octocat" {
		t.Errorf("owner = %q", c.GitHub.Owner)
	}
	if len(c.GitHub.Repos) != 3 {
		t.Fatalf("repos = %v, want 3 entries", c.GitHub.Repos)
	}
	if c.GitHub.Repos[0] != "repo-a" || c.GitHub.Repos[2] != "repo-c" {
		t.Errorf("repos = %v", c.GitHub.Repos)
	}
}

func TestScopeReposWarnsOnRepoField(t *testing.T) {
	p := writeConfig(t, `
github:
  scope: repos
  owner: o
  repo: stale
  repos: [a, b]
auth:
  pat: ghp_x
pools:
  - name: p
    os: linux
    docker:
      host: h
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w := c.Warnings()
	found := false
	for _, msg := range w {
		if strings.Contains(msg, "github.repo is ignored") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected warning about github.repo being ignored, got %v", w)
	}
}

func TestScopeReposMixedOwners(t *testing.T) {
	p := writeConfig(t, `
github:
  scope: repos
  owner: octocat
  repos:
    - repo-a
    - repo-b
    - otheruser/repo-c
auth:
  pat: ghp_x
pools:
  - name: linux-pool
    os: linux
    docker:
      host: tcp://127.0.0.1:2375
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	refs := c.GitHub.ResolvedRepos()
	if len(refs) != 3 {
		t.Fatalf("ResolvedRepos = %v, want 3 entries", refs)
	}
	if refs[0].Owner != "octocat" || refs[0].Repo != "repo-a" {
		t.Errorf("refs[0] = %+v, want octocat/repo-a", refs[0])
	}
	if refs[1].Owner != "octocat" || refs[1].Repo != "repo-b" {
		t.Errorf("refs[1] = %+v, want octocat/repo-b", refs[1])
	}
	if refs[2].Owner != "otheruser" || refs[2].Repo != "repo-c" {
		t.Errorf("refs[2] = %+v, want otheruser/repo-c", refs[2])
	}
}

func TestScopeReposNoOwnerWithFullPaths(t *testing.T) {
	p := writeConfig(t, `
github:
  scope: repos
  repos:
    - alice/repo-x
    - bob/repo-y
auth:
  pat: ghp_x
pools:
  - name: p
    os: linux
    docker:
      host: h
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v (full paths should not require owner)", err)
	}
	refs := c.GitHub.ResolvedRepos()
	if refs[0].Owner != "alice" || refs[0].Repo != "repo-x" {
		t.Errorf("refs[0] = %+v", refs[0])
	}
	if refs[1].Owner != "bob" || refs[1].Repo != "repo-y" {
		t.Errorf("refs[1] = %+v", refs[1])
	}
}

func TestScopeReposTrimsEntries(t *testing.T) {
	p := writeConfig(t, `
github: {scope: repos, owner: " octocat ", repos: [" repo-a ", " octocat/repo-b "]}
auth: {pat: x}
pools: [{name: p, os: linux, docker: {host: h}}]`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GitHub.Owner != "octocat" || c.GitHub.Repos[0] != "repo-a" || c.GitHub.Repos[1] != "octocat/repo-b" {
		t.Fatalf("GitHub config not normalized: %+v", c.GitHub)
	}
}

func TestRepoTargetsIncludesSingularAndPluralScopes(t *testing.T) {
	single := GitHub{Scope: ScopeRepo, Owner: "octo", Repo: "one"}.RepoTargets()
	if len(single) != 1 || single[0] != (RepoRef{Owner: "octo", Repo: "one"}) {
		t.Fatalf("single RepoTargets = %+v", single)
	}

	plural := GitHub{Scope: ScopeRepos, Owner: "octo", Repos: []string{"one", "other/two"}}.RepoTargets()
	if len(plural) != 2 || plural[0] != (RepoRef{Owner: "octo", Repo: "one"}) ||
		plural[1] != (RepoRef{Owner: "other", Repo: "two"}) {
		t.Fatalf("plural RepoTargets = %+v", plural)
	}

	if got := (GitHub{Scope: ScopeOrg, Owner: "octo"}).RepoTargets(); got != nil {
		t.Fatalf("organization RepoTargets = %+v, want nil", got)
	}
}

func TestScopeReposAppAuthRejectsMixedOwners(t *testing.T) {
	p := writeConfig(t, `
github: {scope: repos, owner: alice, repos: [one, bob/two]}
auth: {app_id: 1, installation_id: 2, private_key_path: key.pem}
pools: [{name: p, os: linux, docker: {host: h}}]`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "one installation account") {
		t.Fatalf("Load error = %v, want installation-account error", err)
	}
}

func TestParseRepoRef(t *testing.T) {
	cases := []struct {
		entry, defaultOwner string
		wantOwner, wantRepo string
	}{
		{"my-repo", "octocat", "octocat", "my-repo"},
		{"other/their-repo", "octocat", "other", "their-repo"},
		{"other/their-repo", "", "other", "their-repo"},
		{"bare-name", "", "", "bare-name"},
	}
	for _, tc := range cases {
		ref := ParseRepoRef(tc.entry, tc.defaultOwner)
		if ref.Owner != tc.wantOwner || ref.Repo != tc.wantRepo {
			t.Errorf("ParseRepoRef(%q, %q) = %+v, want %s/%s",
				tc.entry, tc.defaultOwner, ref, tc.wantOwner, tc.wantRepo)
		}
	}
}

func TestWindowsIsolationNotPinnedByDefaults(t *testing.T) {
	p := writeConfig(t, `
github:
  scope: org
  owner: myorg
auth:
  pat: ghp_x
pools:
  - name: windows-pool
    os: windows
    docker:
      host: npipe:////./pipe/docker_engine_windows
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Defaults must leave isolation empty so the backend can resolve it via
	// autoIsolation(). Pinning "process" here broke Windows client editions,
	// where process isolation needs an exact host/container build match.
	if got := c.Pools[0].Docker.Isolation; got != "" {
		t.Errorf("windows pool isolation = %q, want empty (backend resolves it)", got)
	}
}

func TestWindowsIsolationExplicitPreserved(t *testing.T) {
	p := writeConfig(t, `
github:
  scope: org
  owner: myorg
auth:
  pat: ghp_x
pools:
  - name: windows-pool
    os: windows
    docker:
      host: npipe:////./pipe/docker_engine_windows
      isolation: process
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Pools[0].Docker.Isolation; got != "process" {
		t.Errorf("windows pool isolation = %q, want process", got)
	}
}
