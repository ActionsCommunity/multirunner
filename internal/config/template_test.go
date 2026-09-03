package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteAuthProducesRunnableConfig is the guarantee that matters: what
// connect writes into an empty directory loads and validates, so the next
// command the user runs does something instead of reporting a missing pool.
func TestWriteAuthProducesRunnableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := WriteDeviceAuth(path, ScopeOrg, "acme", "", "cid", "tok.json"); err != nil {
		t.Fatal(err)
	}
	added, err := EnsureStarterPool(path, "unix:///probed.sock")
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatal("no starter pool was added to a config that has none")
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("connect wrote a config that does not load: %v", err)
	}
	if len(c.Pools) != 1 {
		t.Fatalf("pools = %d, want the one connect writes", len(c.Pools))
	}
	if got := c.Pools[0].Docker.Host; got != "unix:///probed.sock" {
		t.Errorf("docker.host = %q, want the discovered endpoint", got)
	}
}

// TestStarterPoolFallsBackWhenNothingFound pins that an empty discovery result
// still yields a loadable config, with the platform placeholder to correct.
func TestStarterPoolFallsBackWhenNothingFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteDeviceAuth(path, ScopeOrg, "acme", "", "cid", "tok.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureStarterPool(path, ""); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("fallback config does not load: %v", err)
	}
	if got := c.Pools[0].Docker.Host; got != FallbackDockerHost() {
		t.Errorf("docker.host = %q, want the platform fallback %q", got, FallbackDockerHost())
	}
}

// TestWriteAuthAddsThePoolOnce pins that re-running connect does not stack a
// second pool on top of the first, which would fail on duplicate pool names.
func TestWriteAuthAddsThePoolOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	for i := 0; i < 2; i++ {
		if err := WriteDeviceAuth(path, ScopeOrg, "acme", "", "cid", "tok.json"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if _, err := EnsureStarterPool(path, "unix:///probed.sock"); err != nil {
			t.Fatalf("pool %d: %v", i, err)
		}
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("second connect broke the config: %v", err)
	}
	if len(c.Pools) != 1 {
		t.Errorf("pools = %d after two connects, want 1", len(c.Pools))
	}
}

// TestWriteAuthLeavesExistingPoolsAlone pins that a config which already has
// pools keeps them, and is not given the starter pool on top.
func TestWriteAuthLeavesExistingPoolsAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "github:\n  scope: org\n  owner: acme\n" + ExamplePoolYAML
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteDeviceAuth(path, ScopeOrg, "acme", "", "cid", "tok.json"); err != nil {
		t.Fatal(err)
	}
	if added, err := EnsureStarterPool(path, "unix:///probed.sock"); err != nil || added {
		t.Fatalf("EnsureStarterPool added=%v err=%v, want it to leave existing pools alone", added, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "- name:"); n != 1 {
		t.Errorf("pool count = %d, want the one that was already there:\n%s", n, data)
	}
	if !strings.Contains(string(data), "tcp://127.0.0.1:2375") {
		t.Errorf("existing docker.host was replaced:\n%s", data)
	}
}

// TestExamplePoolYAMLValidates pins the short snippet the missing-pool error
// prints: it has to be paste-ready, not merely illustrative.
func TestExamplePoolYAMLValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "github:\n  scope: org\n  owner: acme\nauth:\n  pat: ghp_x\n" + ExamplePoolYAML
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("the example printed on a missing pool does not validate: %v", err)
	}
}

// TestWritersKeepTheFileHeader pins that a comment block at the top of a config
// survives being rewritten. The parser hangs it off the document node rather
// than the root mapping, so a writer that marshals the mapping alone silently
// deletes whatever the operator wrote above their first key.
func TestWritersKeepTheFileHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "# Runner host for the build farm.\n# Owned by platform-eng.\n\ngithub:\n  scope: org\n  owner: acme\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteDeviceAuth(path, ScopeOrg, "acme", "", "cid", "tok.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureStarterPool(path, "unix:///probed.sock"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"# Runner host for the build farm.", "# Owned by platform-eng."} {
		if !strings.Contains(string(data), line) {
			t.Errorf("the file header line %q was dropped:\n%s", line, data)
		}
	}
}

// TestStarterPoolSurvivesAFlowStyleRoot pins that the pool is grafted onto the
// tree rather than appended as text. Appending a block `pools:` under a root
// written in flow style produced a file that parsed without error and still had
// no pools, so the next command reported the same missing-pool error connect had
// just tried to prevent - and a second connect appended a second copy.
func TestStarterPoolSurvivesAFlowStyleRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "{github: {scope: org, owner: acme}, auth: {pat: ghp_x}}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	added, err := EnsureStarterPool(path, "unix:///probed.sock")
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatal("no pool was added to a flow-style config that has none")
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("config does not load after the pool was added: %v", err)
	}
	if len(c.Pools) != 1 {
		t.Fatalf("pools = %d, want the one that was just added", len(c.Pools))
	}

	// A second connect must see the pool it wrote, not append another.
	if added, err := EnsureStarterPool(path, "unix:///probed.sock"); err != nil || added {
		t.Errorf("second call added=%v err=%v, want it to leave the pool alone", added, err)
	}
}
