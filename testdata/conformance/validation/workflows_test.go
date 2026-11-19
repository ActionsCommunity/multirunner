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

func TestWorkflowsRejectKnownActionTagObjectPins(t *testing.T) {
	t.Parallel()
	const pnpmV6TagObject = "pnpm/action-setup@f520eceda224fe1a4aed5a2a27a194379a409996"
	for _, name := range allWorkflowNames(t) {
		content := string(readProjectFile(t, ".github", "workflows", name))
		if strings.Contains(content, pnpmV6TagObject) {
			t.Errorf("%s pins pnpm/action-setup to the v6 tag object instead of its commit", name)
		}
	}
}

func TestReleaseSigningWorkflowIsolatesPrivateKeys(t *testing.T) {
	t.Parallel()
	content := string(readProjectFile(t, ".github", "workflows", "release-sign.yml"))
	for _, job := range []string{
		"  authorize:", "  prepare-signing-request:", "  sign:",
		"  verify-final:", "  publish:",
	} {
		if !strings.Contains(content, job) {
			t.Fatalf("release-sign.yml is missing job %q", strings.TrimSpace(job))
		}
	}

	sign := workflowSection(t, content, "  sign:", "  verify-final:")
	for _, forbidden := range []string{
		"actions/checkout@", "actions/setup-go@", "actions/setup-node@",
		"actions/cache@", "pnpm/action-setup@", "go run ", "node ", "npm ", "pnpm ",
		"./cmd/", "./scripts/", "uses: ./", "cache:", "contents: write",
	} {
		if strings.Contains(sign, forbidden) {
			t.Errorf("key-bearing sign job contains forbidden capability %q", forbidden)
		}
	}
	for _, required := range []string{
		"environment: release-signing",
		"contents: read",
		"Independently validate request before exposing keys",
		"openssl pkeyutl -sign -rawin",
		"MULTIRUNNER_TUF_TIMESTAMP_PRIVATE_KEY_PEM",
		"MULTIRUNNER_TUF_SNAPSHOT_PRIVATE_KEY_PEM",
		"MULTIRUNNER_TUF_TARGETS_PRIVATE_KEY_PEM",
		"run-id: ${{ needs.authorize.outputs.source-run-id }}",
		"actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333",
		"actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9",
	} {
		if !strings.Contains(sign, required) {
			t.Errorf("key-bearing sign job is missing %q", required)
		}
	}
	for lineNumber, line := range strings.Split(sign, "\n") {
		if strings.Contains(line, "uses:") &&
			!strings.Contains(line, "actions/download-artifact@") &&
			!strings.Contains(line, "actions/upload-artifact@") {
			t.Errorf("sign job line %d invokes a non-artifact action: %s",
				lineNumber+1, strings.TrimSpace(line))
		}
	}

	beforeSign := workflowSection(t, content, "  authorize:", "  sign:")
	afterSign := workflowSection(t, content, "  verify-final:", "")
	if strings.Contains(beforeSign, "PRIVATE_KEY_PEM") ||
		strings.Contains(afterSign, "PRIVATE_KEY_PEM") {
		t.Fatal("private signing keys escape the isolated sign job")
	}
	publish := workflowSection(t, content, "  publish:", "")
	if !strings.Contains(publish, "contents: write") ||
		strings.Contains(publish, "environment: release-signing") ||
		strings.Contains(publish, "secrets.MULTIRUNNER_TUF") {
		t.Fatal("publish job must be no-key and independently contents:write")
	}
}

func TestReleaseBuildProducesBoundUnsignedSigningRequest(t *testing.T) {
	t.Parallel()
	content := string(readProjectFile(t, ".github", "workflows", "release.yml"))
	for _, required := range []string{
		"go run ./cmd/update-repository prepare",
		"-source-run-id \"$GITHUB_RUN_ID\"",
		"-metadata-version \"$GITHUB_RUN_ID\"",
		"-commit \"$GITHUB_SHA\"",
		"name: multirunner-update-signing-request",
		"actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("release.yml is missing unsigned request binding %q", required)
		}
	}
	if strings.Contains(content, "PRIVATE_KEY") {
		t.Fatal("release-build must not receive a signing private key")
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
		// TestConformanceActionsArePinned checks the full commit pins separately,
		// so dependency updates can change commits without changing this matrix.
		"actions/checkout@",
		"actions/cache@",
		"actions/upload-artifact@",
		"pnpm/action-setup@",
		"astral-sh/setup-uv@",
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

func workflowSection(t *testing.T, content, start, end string) string {
	t.Helper()
	startIndex := strings.Index(content, start)
	if startIndex < 0 {
		t.Fatalf("workflow section %q not found", strings.TrimSpace(start))
	}
	if end == "" {
		return content[startIndex:]
	}
	endIndex := strings.Index(content[startIndex+len(start):], end)
	if endIndex < 0 {
		t.Fatalf("workflow section terminator %q not found", strings.TrimSpace(end))
	}
	return content[startIndex : startIndex+len(start)+endIndex]
}
