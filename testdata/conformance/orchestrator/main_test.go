package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	githubapi "github.com/google/go-github/v66/github"
)

const testToken = "REDACTED"

func TestParseTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "two targets", raw: `[{"repository":"owner/one","ref":"main"},{"repository":"owner/two","ref":"release/v1"}]`},
		{name: "requires two", raw: `[{"repository":"owner/one","ref":"main"}]`, wantErr: "between 2 and 20"},
		{name: "rejects duplicates", raw: `[{"repository":"owner/one","ref":"main"},{"repository":"OWNER/ONE","ref":"main"}]`, wantErr: "duplicate"},
		{name: "rejects owner traversal", raw: `[{"repository":"../one","ref":"main"},{"repository":"owner/two","ref":"main"}]`, wantErr: "invalid target repository"},
		{name: "rejects repository traversal", raw: `[{"repository":"owner/..","ref":"main"},{"repository":"owner/two","ref":"main"}]`, wantErr: "invalid target repository"},
		{name: "rejects unsafe ref", raw: `[{"repository":"owner/one","ref":"../main"},{"repository":"owner/two","ref":"main"}]`, wantErr: "invalid target ref"},
		{name: "rejects unknown fields", raw: `[{"repository":"owner/one","ref":"main","token":"nope"},{"repository":"owner/two","ref":"main"}]`, wantErr: "unknown field"},
		{name: "rejects extra JSON", raw: `[{"repository":"owner/one","ref":"main"},{"repository":"owner/two","ref":"main"}] {}`, wantErr: "one JSON value"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseTargets(test.raw)
			if test.wantErr == "" && err != nil {
				t.Fatalf("parseTargets() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("parseTargets() error = %v, want text %q", err, test.wantErr)
			}
		})
	}
}

func TestExecuteRecordsSuccessfulTargets(t *testing.T) {
	t.Parallel()
	server := newFakeGitHub(t, false)
	defer server.Close()
	var output strings.Builder

	reports, err := execute(context.Background(), newTestAPIClient(t, server), testOptions(), &output)
	if err != nil {
		t.Fatalf("execute() error = %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("len(reports) = %d, want 2", len(reports))
	}
	for _, report := range reports {
		if report.Conclusion != "success" || report.QueueMillis != 2000 {
			t.Errorf("report = %+v", report)
		}
	}
	if strings.Contains(output.String(), "http") {
		t.Errorf("output contains an API-supplied URL: %s", output.String())
	}
	if !strings.Contains(output.String(), "pool=conformance runner=mr-conformance-linux-enabled-owned") {
		t.Errorf("output does not identify pool and runner: %s", output.String())
	}
}

func TestExecuteRejectsUnprotectedTargetBeforeDispatch(t *testing.T) {
	t.Parallel()
	server := newFakeGitHubWithProtection(t, false)
	defer server.Close()

	_, err := execute(context.Background(), newTestAPIClient(t, server), testOptions(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "protected branch") {
		t.Fatalf("execute() error = %v, want protected branch error", err)
	}
}

func TestWaitForCleanupPollsUntilOwnedRegistrationsDisappear(t *testing.T) {
	t.Parallel()
	server := newFakeGitHub(t, true)
	defer server.Close()
	var output strings.Builder

	err := waitForCleanup(context.Background(), newTestAPIClient(t, server), testOptions().Targets,
		"mr-conformance-linux", time.Millisecond, &output)
	if err != nil {
		t.Fatalf("waitForCleanup() error = %v", err)
	}
	if !strings.Contains(output.String(), "registrations=0") {
		t.Errorf("cleanup output = %s", output.String())
	}
}

func TestQueueLatencyValidation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	before := now.Add(-time.Second)
	after := now.Add(2 * time.Second)
	tests := []struct {
		name    string
		created time.Time
		jobs    []*githubapi.WorkflowJob
		want    time.Duration
		wantErr string
	}{
		{name: "uses earliest start", created: now, jobs: []*githubapi.WorkflowJob{
			{StartedAt: &githubapi.Timestamp{Time: now.Add(3 * time.Second)}},
			{StartedAt: &githubapi.Timestamp{Time: after}},
		}, want: 2 * time.Second},
		{name: "rejects start before creation", created: now, jobs: []*githubapi.WorkflowJob{
			{StartedAt: &githubapi.Timestamp{Time: before}},
		}, wantErr: "before"},
		{name: "requires timestamps", wantErr: "timestamps"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := queueLatency(test.created, test.jobs)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("queueLatency() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("queueLatency() = %s, %v; want %s", got, err, test.want)
			}
		})
	}
}

func TestRunCLIRequiresToken(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("MR_CONFORMANCE_PAT", "")
	err := runCLI([]string{"run"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "MR_CONFORMANCE_PAT") {
		t.Fatalf("runCLI() error = %v, want missing token", err)
	}
}

func TestValidateCommandDoesNotRequireToken(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("MR_CONFORMANCE_PAT", "")
	if err := runCLI([]string{"validate"}, io.Discard); err != nil {
		t.Fatalf("runCLI(validate) error = %v", err)
	}
}

func TestRunCLIRejectsUnknownCommand(t *testing.T) {
	setValidEnvironment(t)
	if err := runCLI([]string{"unknown"}, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("runCLI() error = %v, want unknown command", err)
	}
}

func TestLoadOptionsRejectsInvalidQueueLimit(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("MR_QUEUE_LIMIT", "invalid")
	if _, err := loadOptions(os.Getenv); err == nil || !strings.Contains(err.Error(), "MR_QUEUE_LIMIT") {
		t.Fatalf("loadOptions() error = %v, want queue limit error", err)
	}
}

func TestLoadOptionsRejectsInvalidRunID(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("GITHUB_RUN_ID", "not-a-number")
	if _, err := loadOptions(os.Getenv); err == nil || !strings.Contains(err.Error(), "GITHUB_RUN_ID") {
		t.Fatalf("loadOptions() error = %v, want run ID error", err)
	}
}

func TestValidationRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()
	valid := testOptions()
	tests := []struct {
		name   string
		mutate func(*options)
	}{
		{name: "runner label", mutate: func(opts *options) { opts.RunnerLabel = "unsafe label" }},
		{name: "fixture repository", mutate: func(opts *options) { opts.FixtureRepository = "missing-owner" }},
		{name: "fixture ref", mutate: func(opts *options) { opts.FixtureRef = "../unsafe" }},
		{name: "platform", mutate: func(opts *options) { opts.Platform = "macos" }},
		{name: "cache mode", mutate: func(opts *options) { opts.CacheMode = "sometimes" }},
		{name: "queue limit", mutate: func(opts *options) { opts.QueueLimit = maxQueueLimit + time.Second }},
		{name: "repository owner", mutate: func(opts *options) { opts.RepositoryOwner = "other" }},
		{name: "repository", mutate: func(opts *options) { opts.Repository = "other/repository" }},
		{name: "trusted ref", mutate: func(opts *options) { opts.Ref = "refs/heads/untrusted" }},
		{name: "workflow", mutate: func(opts *options) { opts.Workflow = "other" }},
		{name: "workflow ref", mutate: func(opts *options) { opts.WorkflowRef = "owner/repo/.github/workflows/other.yml@refs/heads/main" }},
		{name: "event", mutate: func(opts *options) { opts.EventName = "pull_request" }},
		{name: "API base", mutate: func(opts *options) { opts.APIURL = "https://example.test/api/v3" }},
		{name: "backend", mutate: func(opts *options) { opts.Backend = "qemu" }},
		{name: "daemon host", mutate: func(opts *options) { opts.DockerHost = "tcp://attacker.test:2375" }},
		{name: "image", mutate: func(opts *options) { opts.Image = "attacker/image:latest" }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			opts := valid
			test.mutate(&opts)
			if err := validateOptions(opts); err == nil {
				t.Fatal("validateOptions() error = nil")
			}
		})
	}
}

func TestRunCLIRejectsExtraArguments(t *testing.T) {
	setValidEnvironment(t)
	if err := runCLI([]string{"validate", "extra"}, io.Discard); err == nil {
		t.Fatal("runCLI() accepted an extra argument")
	}
}

func TestAPIClientIsFixedToGitHub(t *testing.T) {
	t.Parallel()
	client, err := newAPIClient(testToken, "https://github.com", "https://api.github.com")
	if err != nil {
		t.Fatalf("newAPIClient() error = %v", err)
	}
	if got := client.github.BaseURL.String(); got != "https://api.github.com/" {
		t.Fatalf("BaseURL = %q", got)
	}
	redirectURL, err := url.Parse("https://example.test/redirect")
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	redirect := &http.Request{URL: redirectURL}
	if err := client.github.Client().CheckRedirect(redirect, nil); err == nil {
		t.Fatal("API client accepted a redirect outside api.github.com")
	}
	redirectURL, err = url.Parse("https://api.github.com/repos")
	if err != nil {
		t.Fatalf("parse GitHub redirect URL: %v", err)
	}
	redirect.URL = redirectURL
	if err := client.github.Client().CheckRedirect(redirect, nil); err != nil {
		t.Fatalf("API client rejected a GitHub redirect: %v", err)
	}
	if err := client.github.Client().CheckRedirect(redirect, make([]*http.Request, 3)); err == nil {
		t.Fatal("API client accepted too many redirects")
	}
}

func TestAPIClientAcceptsMatchingGHESBase(t *testing.T) {
	t.Parallel()
	client, err := newAPIClient(
		testToken,
		"https://github.example.test",
		"https://github.example.test/api/v3",
	)
	if err != nil {
		t.Fatalf("newAPIClient() error = %v", err)
	}
	if got := client.github.BaseURL.String(); got != "https://github.example.test/api/v3/" {
		t.Fatalf("BaseURL = %q", got)
	}
	redirectURL, err := url.Parse("https://github.example.test/outside")
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	if err := client.github.Client().CheckRedirect(&http.Request{URL: redirectURL}, nil); err == nil {
		t.Fatal("API client accepted a redirect outside the GHES API path")
	}
}

func TestAPIClientRejectsMismatchedBases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		serverURL string
		apiURL    string
	}{
		{name: "public host mismatch", serverURL: "https://github.com", apiURL: "https://example.test/api/v3"},
		{name: "GHES host mismatch", serverURL: "https://github.example.test", apiURL: "https://api.example.test/api/v3"},
		{name: "GHES path mismatch", serverURL: "https://github.example.test", apiURL: "https://github.example.test/other"},
		{name: "credentials", serverURL: "https://user@github.example.test", apiURL: "https://github.example.test/api/v3"},
		{name: "insecure scheme", serverURL: "http://github.example.test", apiURL: "http://github.example.test/api/v3"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := newAPIClient(testToken, test.serverURL, test.apiURL); err == nil {
				t.Fatal("newAPIClient() accepted unsafe API bases")
			}
		})
	}
}

func TestAPIErrorsRedactToken(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "reflected credential: "+testToken, http.StatusUnauthorized)
	}))
	defer server.Close()
	client := newTestAPIClient(t, server)

	_, err := client.runners(context.Background(), "owner/one")
	if err == nil {
		t.Fatal("runners() error = nil")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("runners() error exposed the token: %v", err)
	}
}

func TestWriteReport(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "report.json")
	reports := []report{{Repository: "owner/one", RunID: 42, Conclusion: "success"}}
	if err := writeReport(path, reports); err != nil {
		t.Fatalf("writeReport() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(content), `"run_id": 42`) {
		t.Fatalf("report = %s", content)
	}
	if err := writeReport(t.TempDir(), reports); err == nil {
		t.Fatal("writeReport() accepted a directory path")
	}
}

func testOptions() options {
	return options{
		Targets: []target{
			{Repository: "owner/one", Ref: "main"},
			{Repository: "owner/two", Ref: "main"},
		},
		RunnerLabel:       "mr-conformance-linux-enabled",
		RunnerPrefix:      "mr-conformance-linux",
		FixtureRepository: "ActionsCommunity/multirunner",
		FixtureRef:        "0123456789abcdef0123456789abcdef01234567",
		Platform:          "linux",
		CacheMode:         "enabled",
		QueueLimit:        10 * time.Second,
		Timeout:           time.Minute,
		PollInterval:      time.Millisecond,
		ReportPath:        "runner-conformance-report.json",
		ServerURL:         "https://github.com",
		APIURL:            "https://api.github.com",
		RepositoryOwner:   "ActionsCommunity",
		Repository:        "ActionsCommunity/multirunner",
		Ref:               "refs/heads/main",
		Workflow:          workflowName,
		WorkflowRef:       "ActionsCommunity/multirunner/.github/workflows/e2e-linux.yml@refs/heads/main",
		EventName:         "workflow_dispatch",
		TrustedRef:        "refs/heads/main",
		RunID:             12345,
		Backend:           "docker",
		DockerHost:        "unix:///var/run/docker.sock",
		Image:             "gerardsmit/multirunner-runner-linux:node",
	}
}

func setValidEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("MR_TARGETS", `[{"repository":"owner/one","ref":"main"},{"repository":"owner/two","ref":"main"}]`)
	t.Setenv("MR_RUNNER_LABEL", "mr-conformance-linux-enabled")
	t.Setenv("MR_RUNNER_PREFIX", "mr-conformance-linux")
	t.Setenv("GITHUB_REPOSITORY", "ActionsCommunity/multirunner")
	t.Setenv("GITHUB_SHA", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_API_URL", "https://api.github.com")
	t.Setenv("GITHUB_REPOSITORY_OWNER", "ActionsCommunity")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_WORKFLOW", workflowName)
	t.Setenv("GITHUB_WORKFLOW_REF", "ActionsCommunity/multirunner/.github/workflows/e2e-linux.yml@refs/heads/main")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_RUN_ID", "12345")
	t.Setenv("MR_TRUSTED_REF", "refs/heads/main")
	t.Setenv("MR_BACKEND", "docker")
	t.Setenv("MR_DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("MR_IMAGE", "gerardsmit/multirunner-runner-linux:node")
	t.Setenv("MR_PLATFORM", "linux")
	t.Setenv("MR_CACHE_MODE", "enabled")
	t.Setenv("MR_QUEUE_LIMIT", "3m")
}

func newTestAPIClient(t *testing.T, server *httptest.Server) *apiClient {
	t.Helper()
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := githubapi.NewClient(server.Client()).WithAuthToken(testToken)
	client.BaseURL = baseURL
	return &apiClient{github: client, token: testToken}
}

func newFakeGitHub(t *testing.T, cleanupSequence bool) *httptest.Server {
	return newFakeGitHubWithOptions(t, cleanupSequence, true)
}

func newFakeGitHubWithProtection(t *testing.T, protected bool) *httptest.Server {
	return newFakeGitHubWithOptions(t, false, protected)
}

func newFakeGitHubWithOptions(t *testing.T, cleanupSequence bool, protected bool) *httptest.Server {
	t.Helper()
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	started := created.Add(2 * time.Second)
	var mu sync.Mutex
	runnerRequests := map[string]int{}

	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(writer, "missing auth", http.StatusUnauthorized)
			return
		}
		repository := repositoryFromPath(request.URL.Path)
		switch {
		case strings.Contains(request.URL.Path, "/branches/"):
			writeJSON(t, writer, map[string]any{"name": "main", "protected": protected})
		case strings.HasSuffix(request.URL.Path, "/actions/runners"):
			mu.Lock()
			runnerRequests[repository]++
			count := runnerRequests[repository]
			mu.Unlock()
			runners := []map[string]any{{
				"name": "mr-conformance-linux-owned", "status": "online",
			}}
			if cleanupSequence && count > 1 {
				runners = nil
			}
			writeJSON(t, writer, map[string]any{"total_count": len(runners), "runners": runners})
		case strings.HasSuffix(request.URL.Path, "/dispatches"):
			if request.Header.Get("X-GitHub-Api-Version") != apiVersion {
				http.Error(writer, "wrong API version", http.StatusBadRequest)
				return
			}
			var body struct {
				Ref              string            `json:"ref"`
				ReturnRunDetails bool              `json:"return_run_details"`
				Inputs           map[string]string `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			if body.Ref != "main" || !body.ReturnRunDetails || body.Inputs["runner_label"] == "" {
				http.Error(writer, "invalid dispatch", http.StatusBadRequest)
				return
			}
			runID := int64(101)
			if strings.HasSuffix(repository, "/two") {
				runID = 102
			}
			writeJSON(t, writer, dispatchResponse{WorkflowRunID: runID})
		case strings.Contains(request.URL.Path, "/actions/runs/") && strings.HasSuffix(request.URL.Path, "/jobs"):
			writeJSON(t, writer, map[string]any{"jobs": []map[string]any{{
				"name": "smoke", "status": "completed", "conclusion": "success",
				"started_at":  started.Format(time.RFC3339),
				"runner_name": "mr-conformance-linux-enabled-owned",
			}}})
		case strings.Contains(request.URL.Path, "/actions/runs/"):
			writeJSON(t, writer, map[string]any{
				"status": "completed", "conclusion": "success",
				"created_at": created.Format(time.RFC3339),
			})
		default:
			http.NotFound(writer, request)
		}
	}))
}

func repositoryFromPath(requestPath string) string {
	parts := strings.Split(strings.Trim(requestPath, "/"), "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[1] + "/" + parts[2]
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
