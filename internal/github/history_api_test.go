package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
)

func TestListRepositoryWorkflowRunsByCreatedRange(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.FixedZone("offset", -7*60*60))
	to := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/hello/actions/runs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got, want := r.URL.Query().Get("created"), "2026-10-01T07:00:00Z..2026-10-08T00:00:00Z"; got != want {
			t.Errorf("created = %q, want %q", got, want)
		}
		if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "50" {
			t.Errorf("pagination query = %q", r.URL.RawQuery)
		}
		w.Header().Set("Link", "<"+srv.URL+r.URL.Path+"?page=3>; rel=\"next\", <"+srv.URL+r.URL.Path+"?page=4>; rel=\"last\"")
		w.Header().Set("X-GitHub-Request-Id", "request-1")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4990")
		w.Header().Set("X-RateLimit-Reset", "1791417600")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workflow_runs": []map[string]any{{
				"id": 101, "name": "Build", "display_title": "Build main",
				"run_number": 17, "run_attempt": 2, "event": "push",
				"status": "completed", "conclusion": "success", "workflow_id": 9,
				"path": ".github/workflows/build.yml", "head_branch": "main",
				"head_sha": "abc123", "html_url": "https://github.example/runs/101",
				"actor":            map[string]any{"login": "octocat"},
				"triggering_actor": map[string]any{"login": "hubot"},
				"created_at":       "2026-10-07T20:00:00Z",
			}},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, config.ScopeRepo, "octo", "hello")
	runs, meta, err := c.ListRepositoryWorkflowRuns(context.Background(), WorkflowRunListOptions{
		CreatedFrom: from,
		CreatedTo:   to,
		PageOptions: PageOptions{Page: 2, PerPage: 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %#v", runs)
	}
	run := runs[0]
	if run.ID != 101 || run.RunAttempt != 2 || run.WorkflowPath != ".github/workflows/build.yml" ||
		run.HeadSHA != "abc123" || run.Actor != "octocat" || run.TriggeringActor != "hubot" {
		t.Fatalf("run = %#v", run)
	}
	if meta.StatusCode != http.StatusOK || meta.RequestID != "request-1" ||
		meta.NextPage != 3 || meta.LastPage != 4 ||
		meta.RateLimit.Limit != 5000 || meta.RateLimit.Remaining != 4990 ||
		meta.RateLimit.Reset.IsZero() {
		t.Fatalf("metadata = %#v", meta)
	}
}

func TestWorkflowRunAndJobReadAPIs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/hello/actions/runs/101":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 101, "name": "Build", "run_attempt": 2, "head_sha": "abc123",
			})
		case "/repos/octo/hello/actions/runs/101/jobs":
			if r.URL.Query().Get("filter") != "all" {
				t.Errorf("filter = %q, want all", r.URL.Query().Get("filter"))
			}
			if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "25" {
				t.Errorf("jobs query = %q", r.URL.RawQuery)
			}
			writeJobs(w)
		case "/repos/octo/hello/actions/runs/101/attempts/2/jobs":
			if r.URL.Query().Get("page") != "3" {
				t.Errorf("attempt query = %q", r.URL.RawQuery)
			}
			writeJobs(w)
		case "/repos/octo/hello/actions/jobs/501":
			writeJob(w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, config.ScopeRepo, "octo", "hello")

	run, _, err := c.GetWorkflowRun(context.Background(), 101)
	if err != nil || run.ID != 101 || run.RunAttempt != 2 || run.HeadSHA != "abc123" {
		t.Fatalf("GetWorkflowRun = %#v, %v", run, err)
	}
	jobs, _, err := c.ListWorkflowJobs(context.Background(), 101, PageOptions{Page: 2, PerPage: 25})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListWorkflowJobs = %#v, %v", jobs, err)
	}
	attemptJobs, _, err := c.ListWorkflowJobsAttempt(context.Background(), 101, 2, PageOptions{Page: 3})
	if err != nil || len(attemptJobs) != 1 {
		t.Fatalf("ListWorkflowJobsAttempt = %#v, %v", attemptJobs, err)
	}
	job, _, err := c.GetWorkflowJob(context.Background(), 501)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != 501 || job.RunID != 101 || job.RunAttempt != 2 ||
		job.Name != "compile" || job.WorkflowName != "Build" ||
		job.HeadBranch != "main" || job.HeadSHA != "abc123" ||
		job.RunnerName != "runner-7" || len(job.Steps) != 1 ||
		job.Steps[0].Name != "checkout" {
		t.Fatalf("job = %#v", job)
	}
}

func TestWorkflowJobLogUsesSafeBoundedRedirectDownload(t *testing.T) {
	blob := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("line one\nsecret ghp_123456789012345678901234567890\n"))
	}))
	defer blob.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/hello/actions/jobs/501/logs" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", blob.URL+"/job.txt")
		w.WriteHeader(http.StatusFound)
	}))
	defer api.Close()
	client := newTestClient(t, api, config.ScopeRepo, "octo", "hello")
	client.downloadClient = blob.Client()
	content, _, err := client.WorkflowJobLog(t.Context(), 501, 1024)
	if err != nil || string(content) != "line one\nsecret ghp_123456789012345678901234567890\n" {
		t.Fatalf("WorkflowJobLog = %q, %v", content, err)
	}
	if _, _, err := client.WorkflowJobLog(t.Context(), 501, 4); !errors.Is(err, ErrLogTooLarge) {
		t.Fatalf("oversized log error = %v", err)
	}
}

func TestHistoryReadAPIsRequireRepositoryScope(t *testing.T) {
	c := &Client{scope: config.ScopeOrg, owner: "octo"}
	if _, _, err := c.ListRepositoryWorkflowRuns(context.Background(), WorkflowRunListOptions{}); err == nil {
		t.Fatal("org client listed repository runs")
	}
	if _, _, err := c.GetWorkflowRun(context.Background(), 1); err == nil {
		t.Fatal("org client got repository run")
	}
	if _, _, err := c.ListWorkflowJobs(context.Background(), 1, PageOptions{}); err == nil {
		t.Fatal("org client listed repository jobs")
	}
	if _, _, err := c.ListWorkflowJobsAttempt(context.Background(), 1, 1, PageOptions{}); err == nil {
		t.Fatal("org client listed repository attempt jobs")
	}
	if _, _, err := c.GetWorkflowJob(context.Background(), 1); err == nil {
		t.Fatal("org client got repository job")
	}
}

func TestListRepositoryWorkflowRunsRejectsReversedRange(t *testing.T) {
	c := &Client{scope: config.ScopeRepo, owner: "octo", repo: "hello"}
	_, _, err := c.ListRepositoryWorkflowRuns(context.Background(), WorkflowRunListOptions{
		CreatedFrom: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
		CreatedTo:   time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("reversed created range was accepted")
	}
}

func writeJobs(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jobs": []map[string]any{jobPayload()},
	})
}

func writeJob(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(jobPayload())
}

func jobPayload() map[string]any {
	return map[string]any{
		"id": 501, "run_id": 101, "run_attempt": 2, "name": "compile",
		"workflow_name": "Build", "head_branch": "main", "head_sha": "abc123",
		"html_url": "https://github.example/jobs/501", "status": "completed",
		"conclusion": "success", "labels": []string{"self-hosted", "linux"},
		"runner_id": 7, "runner_name": "runner-7", "runner_group_id": 8,
		"runner_group_name": "Default", "created_at": "2026-10-07T20:00:00Z",
		"started_at": "2026-10-07T20:01:00Z", "completed_at": "2026-10-07T20:02:00Z",
		"steps": []map[string]any{{
			"name": "checkout", "status": "completed", "conclusion": "success",
			"number": 1, "started_at": "2026-10-07T20:01:00Z",
			"completed_at": "2026-10-07T20:01:30Z",
		}},
	}
}
