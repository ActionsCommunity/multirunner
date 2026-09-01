package validation

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConformanceWorkflowsParseAsYAML(t *testing.T) {
	t.Parallel()
	for _, name := range workflowNames() {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			content := readProjectFile(t, ".github", "workflows", name)
			var document yaml.Node
			if err := yaml.Unmarshal(content, &document); err != nil {
				t.Fatalf("parse workflow: %v", err)
			}
			if len(document.Content) == 0 {
				t.Fatal("workflow is empty")
			}
		})
	}
}

func TestPublicWorkflowKeepsPrivilegedJobsAwayFromPullRequests(t *testing.T) {
	t.Parallel()
	content := string(readProjectFile(t, ".github", "workflows", "runner-conformance.yml"))
	required := []string{
		"pull_request:",
		"hosted-linux:",
		"hosted-windows:",
		"github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'",
		"vars.MR_CONFORMANCE_TARGETS != ''",
	}
	for _, fragment := range required {
		if !strings.Contains(content, fragment) {
			t.Errorf("runner-conformance.yml is missing %q", fragment)
		}
	}
	if strings.Contains(content, "pull_request_target") {
		t.Error("privileged conformance must never use pull_request_target")
	}
}

func TestHostWorkflowRequiresTwoTargetsAndChecksCleanup(t *testing.T) {
	t.Parallel()
	content := string(readProjectFile(t, ".github", "workflows", "runner-conformance-host.yml"))
	required := []string{
		"$targets.Count -lt 2",
		"Administration:write",
		"conformance-orchestrator",
		`advertise_url: "http://host.docker.internal:3000"`,
		"phase=cleanup containers=0",
		"MR_REPORT",
	}
	for _, fragment := range required {
		if !strings.Contains(content, fragment) {
			t.Errorf("runner-conformance-host.yml is missing %q", fragment)
		}
	}
}

func TestTargetWorkflowCoversRequiredWorkloads(t *testing.T) {
	t.Parallel()
	content := string(readProjectFile(t, ".github", "workflows", "runner-conformance-target.yml"))
	required := []string{
		"actions/checkout@v7",
		"actions/cache@v6",
		"actions/upload-artifact@v7",
		"pnpm/action-setup@v6",
		"astral-sh/setup-uv@v10",
		"dotnet restore",
		"dotnet build",
		"dotnet test",
		"--self-contained true",
		"Conformance.exe",
		"test ! -S /var/run/docker.sock",
		`test "$(id -u)" -ne 0`,
	}
	for _, fragment := range required {
		if !strings.Contains(content, fragment) {
			t.Errorf("runner-conformance-target.yml is missing %q", fragment)
		}
	}
}

func TestConformanceFixturesArePresent(t *testing.T) {
	t.Parallel()
	files := [][]string{
		{"testdata", "conformance", "node", "pnpm-lock.yaml"},
		{"testdata", "conformance", "node", "test", "target.test.mjs"},
		{"testdata", "conformance", "python", "uv.lock"},
		{"testdata", "conformance", "python", "tests", "test_conformance.py"},
		{"testdata", "conformance", "dotnet", "Conformance.slnx"},
		{"testdata", "conformance", "dotnet", "src", "Conformance", "packages.lock.json"},
		{"testdata", "conformance", "dotnet", "tests", "Conformance.Tests", "packages.lock.json"},
	}
	for _, parts := range files {
		if _, err := os.Stat(filepath.Join(append([]string{projectRoot(t)}, parts...)...)); err != nil {
			t.Errorf("required fixture %s: %v", filepath.Join(parts...), err)
		}
	}
}

func workflowNames() []string {
	return []string{
		"runner-conformance.yml",
		"runner-conformance-dispatch.yml",
		"runner-conformance-host.yml",
		"runner-conformance-target.yml",
	}
}

func readProjectFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(append([]string{projectRoot(t)}, parts...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return content
}

func projectRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve validation source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
