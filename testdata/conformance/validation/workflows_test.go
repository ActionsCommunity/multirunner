package validation

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflow struct {
	On   map[string]any         `yaml:"on"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	If     string `yaml:"if"`
	RunsOn any    `yaml:"runs-on"`
}

func TestConformanceWorkflowsParseAsYAML(t *testing.T) {
	t.Parallel()
	for _, name := range workflowNames() {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_ = parseWorkflow(t, name)
		})
	}
}

func TestPullRequestsCannotReachPrivilegedJobs(t *testing.T) {
	t.Parallel()
	hosted := parseWorkflow(t, "test.yml")
	if _, ok := hosted.On["pull_request"]; !ok {
		t.Fatal("test.yml must provide secret-free pull request validation")
	}
	for _, name := range []string{"conformance-linux", "conformance-windows"} {
		job, ok := hosted.Jobs[name]
		if !ok {
			t.Fatalf("test.yml is missing %s", name)
		}
		runsOn, ok := job.RunsOn.(string)
		if !ok {
			t.Fatalf("%s runs-on is not a hosted runner string: %v", name, job.RunsOn)
		}
		if strings.Contains(runsOn, "self-hosted") {
			t.Errorf("%s must use a GitHub-hosted runner", name)
		}
	}

	main := parseWorkflow(t, "e2e-linux.yml")
	if len(main.On) != 2 {
		t.Fatalf("privileged triggers = %v, want schedule and workflow_dispatch", main.On)
	}
	if _, ok := main.On["schedule"]; !ok {
		t.Fatal("privileged workflow is missing schedule")
	}
	if _, ok := main.On["workflow_dispatch"]; !ok {
		t.Fatal("privileged workflow is missing workflow_dispatch")
	}
	for _, name := range []string{"privileged-linux", "privileged-windows"} {
		job, ok := main.Jobs[name]
		if !ok {
			t.Fatalf("e2e-linux.yml is missing %s", name)
		}
		if !strings.Contains(job.If, "github.event_name == 'schedule'") ||
			!strings.Contains(job.If, "github.event_name == 'workflow_dispatch'") ||
			!strings.Contains(job.If, "github.repository == 'ActionsCommunity/multirunner'") ||
			!strings.Contains(job.If, "github.ref == (vars.MR_CONFORMANCE_TRUSTED_REF || 'refs/heads/main')") {
			t.Errorf("%s condition does not restrict execution to trusted events: %q", name, job.If)
		}
	}

	target := parseWorkflow(t, "e2e-target.yml")
	if len(target.On) != 1 {
		t.Fatalf("e2e-target.yml triggers = %v, want workflow_dispatch only", target.On)
	}
	if _, ok := target.On["workflow_dispatch"]; !ok {
		t.Fatalf("e2e-target.yml triggers = %v, want workflow_dispatch", target.On)
	}
	for _, name := range []string{"linux-smoke", "windows-smoke"} {
		job := target.Jobs[name]
		if !strings.Contains(job.If, "inputs.fixture_repository == 'ActionsCommunity/multirunner'") ||
			!strings.Contains(job.If, "inputs.runner_prefix == inputs.runner_label") ||
			!strings.Contains(job.If, "startsWith(inputs.runner_label, 'mr-conformance-") {
			t.Errorf("%s does not constrain checkout and runner selection: %q", name, job.If)
		}
	}

	for _, name := range []string{"e2e-linux.yml", "e2e-target.yml"} {
		content := string(readProjectFile(t, ".github", "workflows", name))
		if strings.Contains(content, "pull_request") || strings.Contains(content, "workflow_call") {
			t.Errorf("%s exposes a pull request or reusable privileged entry point", name)
		}
	}
}

func TestEveryPullRequestWorkflowUsesHostedRunnersWithoutSecrets(t *testing.T) {
	t.Parallel()
	for _, name := range allWorkflowNames(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document := parseWorkflow(t, name)
			content := string(readProjectFile(t, ".github", "workflows", name))
			if strings.Contains(content, "pull_request_target") {
				t.Fatal("pull_request_target is forbidden")
			}
			if _, ok := document.On["pull_request"]; !ok {
				return
			}
			if strings.Contains(content, "secrets.") {
				t.Fatal("pull request workflow references a repository secret")
			}
			for jobName, job := range document.Jobs {
				if strings.Contains(fmt.Sprint(job.RunsOn), "self-hosted") &&
					!strings.Contains(job.If, "github.event_name != 'pull_request'") {
					t.Errorf("pull request job %s can select a self-hosted runner", jobName)
				}
			}
		})
	}
}

func TestHostedJobsReceiveNoConformanceSecret(t *testing.T) {
	t.Parallel()
	content := string(readProjectFile(t, ".github", "workflows", "test.yml"))
	if strings.Contains(content, "secrets.") || strings.Contains(content, "MR_CONFORMANCE_PAT") {
		t.Fatal("hosted pull request jobs must not receive conformance credentials")
	}
}

func TestWorkflowSanitizesLogsAndNeverPrintsConfiguration(t *testing.T) {
	t.Parallel()
	content := string(readProjectFile(t, ".github", "workflows", "e2e-linux.yml"))
	required := []string{
		`Write-Output "::add-mask::$env:MR_CONFORMANCE_PAT"`,
		`$content.Replace($env:MR_CONFORMANCE_PAT, '[REDACTED]')`,
		"persist-credentials: false",
		"MR_TRUSTED_REF:",
	}
	for _, fragment := range required {
		if !strings.Contains(content, fragment) {
			t.Errorf("e2e-linux.yml is missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"cat multirunner-conformance.yaml",
		"Get-Content multirunner-conformance.yaml",
		"set -x",
		"Write-Host $env:MR_CONFORMANCE_PAT",
	} {
		if strings.Contains(content, forbidden) {
			t.Errorf("e2e-linux.yml contains unsafe output %q", forbidden)
		}
	}
}

func TestPrivilegedTokenIsScopedToRequiredSteps(t *testing.T) {
	t.Parallel()
	content := string(readProjectFile(t, ".github", "workflows", "e2e-linux.yml"))
	if strings.Contains(content, "    env:\n      MR_CONFORMANCE_PAT:") {
		t.Fatal("privileged token is exposed through a job-level environment")
	}
	if count := strings.Count(content, "MR_CONFORMANCE_PAT: ${{ secrets.MR_E2E_PAT }}"); count != 5 {
		t.Fatalf("privileged token references = %d, want 5 narrowly scoped steps", count)
	}
}

func TestConformanceActionsArePinned(t *testing.T) {
	t.Parallel()
	pinnedAction := regexp.MustCompile(`@[0-9a-f]{40}(?:\s|$)`)
	for _, name := range []string{"e2e-linux.yml", "e2e-target.yml"} {
		content := string(readProjectFile(t, ".github", "workflows", name))
		for lineNumber, line := range strings.Split(content, "\n") {
			if strings.Contains(line, "uses:") && !pinnedAction.MatchString(line) {
				t.Errorf("%s:%d contains an unpinned action: %s", name, lineNumber+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestConformanceMatrixCoversIssueRequirements(t *testing.T) {
	t.Parallel()
	main := string(readProjectFile(t, ".github", "workflows", "e2e-linux.yml"))
	hosted := string(readProjectFile(t, ".github", "workflows", "test.yml"))
	target := string(readProjectFile(t, ".github", "workflows", "e2e-target.yml"))
	requiredMain := []string{
		"privileged-linux:",
		"privileged-windows:",
		"group: runner-conformance-privileged",
		"$targets.Count",
		`advertise_url: "http://host.docker.internal:3000"`,
		"phase=cleanup containers=0",
		"MR_CONFORMANCE_TARGETS",
	}
	for _, fragment := range requiredMain {
		if !strings.Contains(main, fragment) {
			t.Errorf("e2e-linux.yml is missing %q", fragment)
		}
	}
	for _, fragment := range []string{"conformance-linux:", "conformance-windows:"} {
		if !strings.Contains(hosted, fragment) {
			t.Errorf("test.yml is missing %q", fragment)
		}
	}
	requiredTarget := []string{
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		"actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9",
		"actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
		"pnpm/action-setup@f520eceda224fe1a4aed5a2a27a194379a409996",
		"astral-sh/setup-uv@20cfd1bf945f4377ade1205e4dbc17946fc9a30d",
		"dotnet restore",
		"dotnet build",
		"dotnet test",
		"--self-contained true",
		"Conformance.exe",
		"test ! -S /var/run/docker.sock",
		`test "$(id -u)" -ne 0`,
	}
	for _, fragment := range requiredTarget {
		if !strings.Contains(target, fragment) {
			t.Errorf("e2e-target.yml is missing %q", fragment)
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
	return []string{"test.yml", "e2e-linux.yml", "e2e-target.yml"}
}

func allWorkflowNames(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(projectRoot(t), ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, filepath.Base(match))
	}
	return names
}

func parseWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	var document workflow
	if err := yaml.Unmarshal(readProjectFile(t, ".github", "workflows", name), &document); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if len(document.On) == 0 || len(document.Jobs) == 0 {
		t.Fatalf("%s has no triggers or jobs", name)
	}
	return document
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
