package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	githubapi "github.com/google/go-github/v66/github"
)

type waitPoll struct {
	status     string
	conclusion string
	createdAt  time.Time
	observedAt time.Time
	jobs       []*githubapi.WorkflowJob
	runStatus  int
	jobsStatus int
}

type waitServerState struct {
	mu           sync.Mutex
	activePoll   int
	runRequests  int
	jobsRequests int
}

func TestWaitForRunFailsAtQueueLimit(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	server, state := newWaitForRunServer(t, []waitPoll{{
		status:     "queued",
		createdAt:  createdAt,
		observedAt: createdAt.Add(10 * time.Second),
	}})
	defer server.Close()
	opts := testOptions()

	_, err := waitForRun(context.Background(), newTestAPIClient(t, server), "owner/one", 101, opts, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "remained queued for 10s") {
		t.Fatalf("waitForRun() error = %v, want queue limit error", err)
	}
	if runs, jobs := state.requestCounts(); runs != 1 || jobs != 1 {
		t.Fatalf("API requests = runs %d, jobs %d; want 1, 1", runs, jobs)
	}
}

func TestWaitForRunRecordsJobStartingBeforeThreshold(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	startedJob := successfulJob(createdAt.Add(2 * time.Second))
	server, state := newWaitForRunServer(t, []waitPoll{
		{
			status:     "queued",
			createdAt:  createdAt,
			observedAt: createdAt.Add(time.Second),
		},
		{
			status:     "in_progress",
			createdAt:  createdAt,
			observedAt: createdAt.Add(2 * time.Second),
			jobs:       []*githubapi.WorkflowJob{startedJob},
		},
		{
			status:     "completed",
			conclusion: "success",
			createdAt:  createdAt,
			observedAt: createdAt.Add(6 * time.Second),
			jobs:       []*githubapi.WorkflowJob{startedJob},
		},
	})
	defer server.Close()
	opts := testOptions()

	got, err := waitForRun(context.Background(), newTestAPIClient(t, server), "owner/one", 101, opts, io.Discard)
	if err != nil {
		t.Fatalf("waitForRun() error = %v", err)
	}
	if got.QueueMillis != 2000 || got.Conclusion != "success" {
		t.Fatalf("waitForRun() report = %+v", got)
	}
	if runs, jobs := state.requestCounts(); runs != 3 || jobs != 3 {
		t.Fatalf("API requests = runs %d, jobs %d; want 3, 3", runs, jobs)
	}
}

func TestWaitForRunHandlesTerminalPaths(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		poll            waitPoll
		wantError       string
		wantJobRequests int
	}{
		{
			name: "canceled",
			poll: waitPoll{
				status: "completed", conclusion: "cancelled",
				createdAt: createdAt, observedAt: createdAt.Add(time.Second),
			},
			wantError: "concluded cancelled",
		},
		{
			name: "completed without materialized jobs",
			poll: waitPoll{
				status: "completed", conclusion: "success",
				createdAt: createdAt, observedAt: createdAt.Add(time.Second),
			},
			wantError: "exposed no started jobs", wantJobRequests: 1,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, state := newWaitForRunServer(t, []waitPoll{test.poll})
			defer server.Close()

			_, err := waitForRun(
				context.Background(),
				newTestAPIClient(t, server),
				"owner/one",
				101,
				testOptions(),
				io.Discard,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("waitForRun() error = %v, want %q", err, test.wantError)
			}
			if _, jobs := state.requestCounts(); jobs != test.wantJobRequests {
				t.Fatalf("job requests = %d, want %d", jobs, test.wantJobRequests)
			}
		})
	}
}

func TestWaitForRunReturnsAPIErrors(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		poll      waitPoll
		wantError string
	}{
		{
			name: "workflow run",
			poll: waitPoll{
				runStatus: http.StatusInternalServerError,
			},
			wantError: "get run 101",
		},
		{
			name: "workflow jobs",
			poll: waitPoll{
				status: "queued", createdAt: createdAt, observedAt: createdAt.Add(time.Second),
				jobsStatus: http.StatusServiceUnavailable,
			},
			wantError: "get jobs for run 101",
		},
		{
			name: "server timestamp",
			poll: waitPoll{
				status: "queued", createdAt: createdAt,
			},
			wantError: "Date header",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, _ := newWaitForRunServer(t, []waitPoll{test.poll})
			defer server.Close()

			_, err := waitForRun(
				context.Background(),
				newTestAPIClient(t, server),
				"owner/one",
				101,
				testOptions(),
				io.Discard,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("waitForRun() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func newWaitForRunServer(t *testing.T, polls []waitPoll) (*httptest.Server, *waitServerState) {
	t.Helper()
	state := &waitServerState{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		if strings.HasSuffix(request.URL.Path, "/jobs") {
			poll := polls[state.activePoll]
			state.jobsRequests++
			if poll.jobsStatus != 0 {
				http.Error(writer, "jobs unavailable", poll.jobsStatus)
				return
			}
			writeJSON(t, writer, map[string]any{"jobs": poll.jobs})
			return
		}

		index := state.runRequests
		if index >= len(polls) {
			index = len(polls) - 1
		}
		state.activePoll = index
		state.runRequests++
		poll := polls[index]
		if poll.runStatus != 0 {
			http.Error(writer, "run unavailable", poll.runStatus)
			return
		}
		writer.Header().Set("Date", poll.observedAt.Format(http.TimeFormat))
		writeJSON(t, writer, map[string]any{
			"status": poll.status, "conclusion": poll.conclusion,
			"created_at": poll.createdAt.Format(time.RFC3339),
		})
	}))
	return server, state
}

func (s *waitServerState) requestCounts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runRequests, s.jobsRequests
}

func successfulJob(startedAt time.Time) *githubapi.WorkflowJob {
	return &githubapi.WorkflowJob{
		Name:       githubapi.String("smoke"),
		Status:     githubapi.String("completed"),
		Conclusion: githubapi.String("success"),
		StartedAt:  &githubapi.Timestamp{Time: startedAt},
		RunnerName: githubapi.String("mr-conformance-linux-enabled-owned"),
	}
}
