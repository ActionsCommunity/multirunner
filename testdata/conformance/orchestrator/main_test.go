package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "two targets", raw: `[{"repository":"owner/one","ref":"main"},{"repository":"owner/two","ref":"release/v1"}]`},
		{name: "requires two", raw: `[{"repository":"owner/one","ref":"main"}]`, wantErr: "at least two"},
		{name: "rejects duplicates case insensitively", raw: `[{"repository":"owner/one","ref":"main"},{"repository":"OWNER/ONE","ref":"main"}]`, wantErr: "duplicate"},
		{name: "rejects unsafe ref", raw: `[{"repository":"owner/one","ref":"../main"},{"repository":"owner/two","ref":"main"}]`, wantErr: "invalid target ref"},
		{name: "rejects unknown fields", raw: `[{"repository":"owner/one","ref":"main","token":"nope"},{"repository":"owner/two","ref":"main"}]`, wantErr: "unknown field"},
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
	client := &apiClient{baseURL: server.URL, token: "REDACTED", httpClient: server.Client()}
	opts := testOptions(t.TempDir())
	var output strings.Builder

	reports, err := execute(context.Background(), client, opts, &output)
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
	if !strings.Contains(output.String(), "phase=provisioned targets=2") {
		t.Errorf("output did not record provisioning: %s", output.String())
	}
}

func TestWaitForCleanupPollsUntilOwnedRegistrationsDisappear(t *testing.T) {
	t.Parallel()
	server := newFakeGitHub(t, true)
	defer server.Close()
	client := &apiClient{baseURL: server.URL, token: "REDACTED", httpClient: server.Client()}
	var output strings.Builder

	err := waitForCleanup(context.Background(), client, testOptions(t.TempDir()).Targets,
		"mr-conformance-linux", time.Millisecond, &output)
	if err != nil {
		t.Fatalf("waitForCleanup() error = %v", err)
	}
	if !strings.Contains(output.String(), "registrations=0") {
		t.Errorf("cleanup output = %s", output.String())
	}
}

func TestQueueLatencyRejectsInvalidTimestamps(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	started := now.Add(-time.Second)
	_, err := queueLatency(now, workflowJobs{
		Jobs: []struct {
			Name      string     `json:"name"`
			Status    string     `json:"status"`
			StartedAt *time.Time `json:"started_at"`
		}{{Name: "smoke", Status: "completed", StartedAt: &started}},
	})
	if err == nil || !strings.Contains(err.Error(), "before") {
		t.Fatalf("queueLatency() error = %v, want invalid timestamp error", err)
	}
}

func TestRunCLIRequiresToken(t *testing.T) {
	t.Setenv("MR_CONFORMANCE_PAT", "")
	err := runCLI([]string{"run"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "MR_CONFORMANCE_PAT") {
		t.Fatalf("runCLI() error = %v, want missing token", err)
	}
}

func TestRunCLIWritesReport(t *testing.T) {
	server := newFakeGitHub(t, false)
	defer server.Close()
	t.Setenv("MR_CONFORMANCE_PAT", "REDACTED")
	t.Setenv("GITHUB_API_URL", server.URL)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	targets := `[{"repository":"owner/one","ref":"main"},{"repository":"owner/two","ref":"main"}]`

	err := runCLI([]string{
		"run",
		"--targets-json", targets,
		"--workflow", "runner-conformance-dispatch.yml",
		"--runner-label", "mr-conformance-linux-enabled",
		"--runner-prefix", "mr-conformance-linux",
		"--fixture-repository", "ActionsCommunity/multirunner",
		"--fixture-ref", "main",
		"--platform", "linux",
		"--cache-mode", "enabled",
		"--queue-limit", "10s",
		"--poll", "1ms",
		"--report", reportPath,
	}, io.Discard)
	if err != nil {
		t.Fatalf("runCLI() error = %v", err)
	}
	content, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(content), `"queue_millis": 2000`) {
		t.Errorf("report = %s", content)
	}
}

func TestParseCleanupOptionsRejectsInvalidPrefix(t *testing.T) {
	t.Parallel()
	targets := `[{"repository":"owner/one","ref":"main"},{"repository":"owner/two","ref":"main"}]`
	_, _, _, _, err := parseCleanupOptions([]string{
		"--targets-json", targets,
		"--runner-prefix", "../unsafe",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid runner prefix") {
		t.Fatalf("parseCleanupOptions() error = %v, want invalid prefix", err)
	}
}

func TestValidateOptionsRejectsInvalidFields(t *testing.T) {
	t.Parallel()
	valid := testOptions(t.TempDir())
	tests := []struct {
		name   string
		mutate func(*options)
	}{
		{name: "workflow", mutate: func(opts *options) { opts.Workflow = "../unsafe.yml" }},
		{name: "runner label", mutate: func(opts *options) { opts.RunnerLabel = "unsafe label" }},
		{name: "fixture repository", mutate: func(opts *options) { opts.FixtureRepository = "missing-owner" }},
		{name: "fixture ref", mutate: func(opts *options) { opts.FixtureRef = "../unsafe" }},
		{name: "platform", mutate: func(opts *options) { opts.Platform = "macos" }},
		{name: "cache mode", mutate: func(opts *options) { opts.CacheMode = "sometimes" }},
		{name: "durations", mutate: func(opts *options) { opts.QueueLimit = 0 }},
		{name: "report path", mutate: func(opts *options) { opts.ReportPath = "" }},
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

func TestRunCLICleanupAndUnknownCommand(t *testing.T) {
	server := newFakeGitHub(t, true)
	defer server.Close()
	t.Setenv("MR_CONFORMANCE_PAT", "REDACTED")
	t.Setenv("GITHUB_API_URL", server.URL)
	targets := `[{"repository":"owner/one","ref":"main"},{"repository":"owner/two","ref":"main"}]`

	err := runCLI([]string{
		"cleanup",
		"--targets-json", targets,
		"--runner-prefix", "mr-conformance-linux",
		"--poll", "1ms",
	}, io.Discard)
	if err != nil {
		t.Fatalf("cleanup error = %v", err)
	}
	if err := runCLI([]string{"unknown"}, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command error = %v", err)
	}
}

func TestQueueLatencyRequiresTimestamps(t *testing.T) {
	t.Parallel()
	if _, err := queueLatency(time.Time{}, workflowJobs{}); err == nil {
		t.Fatal("queueLatency() error = nil")
	}
}

func testOptions(reportDirectory string) options {
	return options{
		Targets: []target{
			{Repository: "owner/one", Ref: "main"},
			{Repository: "owner/two", Ref: "main"},
		},
		Workflow:          "runner-conformance-dispatch.yml",
		RunnerLabel:       "mr-conformance-linux-enabled",
		RunnerPrefix:      "mr-conformance-linux",
		FixtureRepository: "ActionsCommunity/multirunner",
		FixtureRef:        "main",
		Platform:          "linux",
		CacheMode:         "enabled",
		QueueLimit:        10 * time.Second,
		Timeout:           time.Minute,
		PollInterval:      time.Millisecond,
		ReportPath:        filepath.Join(reportDirectory, "report.json"),
	}
}

func newFakeGitHub(t *testing.T, cleanupSequence bool) *httptest.Server {
	t.Helper()
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	started := created.Add(2 * time.Second)
	var mu sync.Mutex
	runnerRequests := map[string]int{}

	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer REDACTED" {
			http.Error(writer, "missing auth", http.StatusUnauthorized)
			return
		}
		if request.Header.Get("X-GitHub-Api-Version") != apiVersion {
			http.Error(writer, "wrong API version", http.StatusBadRequest)
			return
		}
		repository := repositoryFromPath(request.URL.Path)
		switch {
		case strings.HasSuffix(request.URL.Path, "/actions/runners"):
			mu.Lock()
			runnerRequests[repository]++
			count := runnerRequests[repository]
			mu.Unlock()
			if cleanupSequence && count == 1 {
				writeJSON(t, writer, map[string]any{"runners": []map[string]any{{
					"name": "mr-conformance-linux-owned", "status": "offline",
					"labels": []map[string]string{{"name": "mr-conformance-linux-enabled"}},
				}}})
				return
			}
			runners := []map[string]any{{
				"name": "mr-conformance-linux-owned", "status": "online",
				"labels": []map[string]string{{"name": "mr-conformance-linux-enabled"}},
			}}
			if cleanupSequence {
				runners = nil
			}
			writeJSON(t, writer, map[string]any{"runners": runners})
		case strings.HasSuffix(request.URL.Path, "/dispatches"):
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
			writeJSON(t, writer, dispatchResponse{
				WorkflowRunID: runID,
				HTMLURL:       fmt.Sprintf("https://example.test/%d", runID),
			})
		case strings.Contains(request.URL.Path, "/actions/runs/") && strings.HasSuffix(request.URL.Path, "/jobs"):
			writeJSON(t, writer, map[string]any{"jobs": []map[string]any{{
				"name": "smoke", "status": "completed", "started_at": started.Format(time.RFC3339),
			}}})
		case strings.Contains(request.URL.Path, "/actions/runs/"):
			writeJSON(t, writer, map[string]any{
				"status": "completed", "conclusion": "success",
				"created_at": created.Format(time.RFC3339), "html_url": "https://example.test/run",
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
