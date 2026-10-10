package consoleapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/history"
)

type runInspectionResponse struct {
	Run            runResponse             `json:"run"`
	Jobs           []jobResponse           `json:"jobs"`
	RunnerSessions []runnerSessionResponse `json:"runner_sessions"`
}

type runResponse struct {
	ID              int64      `json:"id"`
	Repository      string     `json:"repository"`
	Name            string     `json:"name"`
	WorkflowName    string     `json:"workflow_name"`
	DisplayTitle    string     `json:"display_title"`
	Event           string     `json:"event"`
	Status          string     `json:"status"`
	Conclusion      string     `json:"conclusion"`
	HeadBranch      string     `json:"head_branch"`
	HeadSHA         string     `json:"head_sha"`
	Actor           string     `json:"actor"`
	TriggeringActor string     `json:"triggering_actor"`
	HTMLURL         string     `json:"html_url"`
	RunNumber       int64      `json:"run_number"`
	RunAttempt      int        `json:"run_attempt"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

type jobResponse struct {
	ID                    int64          `json:"id"`
	RunID                 int64          `json:"run_id"`
	RunAttempt            int            `json:"run_attempt"`
	Name                  string         `json:"name"`
	WorkflowName          string         `json:"workflow_name"`
	Status                string         `json:"status"`
	Conclusion            string         `json:"conclusion"`
	RunnerName            string         `json:"runner_name"`
	PoolName              string         `json:"pool_name"`
	LocalSessionID        string         `json:"local_session_id"`
	AttributionSource     string         `json:"attribution_source"`
	AttributionConfidence int            `json:"attribution_confidence"`
	HTMLURL               string         `json:"html_url"`
	CreatedAt             time.Time      `json:"created_at"`
	StartedAt             *time.Time     `json:"started_at,omitempty"`
	CompletedAt           *time.Time     `json:"completed_at,omitempty"`
	Steps                 []stepResponse `json:"steps"`
}

type stepResponse struct {
	Number      int        `json:"number"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type runnerSessionResponse struct {
	ID                    string     `json:"id"`
	RunnerName            string     `json:"runner_name"`
	PoolName              string     `json:"pool_name"`
	Target                string     `json:"target"`
	Status                string     `json:"status"`
	Conclusion            string     `json:"conclusion"`
	BackendID             string     `json:"backend_id"`
	AttributionSource     string     `json:"attribution_source"`
	AttributionConfidence int        `json:"attribution_confidence"`
	PlannedAt             time.Time  `json:"planned_at"`
	RegisteredAt          *time.Time `json:"registered_at,omitempty"`
	LaunchedAt            *time.Time `json:"launched_at,omitempty"`
	StartedAt             time.Time  `json:"started_at"`
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
	Error                 string     `json:"error,omitempty"`
}

func runInspectionHandler(reader RunInspectionReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repository := strings.TrimSpace(r.URL.Query().Get("repository"))
		if repository == "" || len(repository) > 256 {
			writeError(w, http.StatusBadRequest, "validation_failed", "repository is required and cannot exceed 256 characters")
			return
		}
		runID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || runID <= 0 {
			writeError(w, http.StatusBadRequest, "validation_failed", "run ID must be positive")
			return
		}
		run, err := reader.WorkflowRun(r.Context(), repository, runID)
		if errors.Is(err, history.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "workflow run was not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "history_unavailable", "workflow run is unavailable")
			return
		}
		jobs, err := reader.ListWorkflowJobs(r.Context(), history.ListOptions{
			Repository: repository, RunID: runID, Limit: 1000,
		})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "history_unavailable", "workflow jobs are unavailable")
			return
		}
		sessions, err := reader.ListRunnerSessions(r.Context(), history.ListOptions{
			Repository: repository, RunID: runID, Limit: 1000,
		})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "history_unavailable", "runner sessions are unavailable")
			return
		}
		writeJSON(w, http.StatusOK, runInspectionResponse{
			Run:            runAPIResponse(run),
			Jobs:           jobAPIResponses(jobs),
			RunnerSessions: runnerSessionAPIResponses(sessions),
		})
	}
}

func runAPIResponse(run history.WorkflowRun) runResponse {
	return runResponse{
		ID: run.ID, Repository: run.Repository, Name: run.Name,
		WorkflowName: run.WorkflowName, DisplayTitle: run.DisplayTitle,
		Event: run.Event, Status: run.Status, Conclusion: run.Conclusion,
		HeadBranch: run.HeadBranch, HeadSHA: run.HeadSHA, Actor: run.Actor,
		TriggeringActor: run.TriggeringActor, HTMLURL: run.HTMLURL,
		RunNumber: run.RunNumber, RunAttempt: run.RunAttempt,
		CreatedAt: run.CreatedAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
	}
}

func jobAPIResponses(jobs []history.WorkflowJob) []jobResponse {
	responses := make([]jobResponse, 0, len(jobs))
	for _, job := range jobs {
		steps := make([]stepResponse, 0, len(job.Steps))
		for _, step := range job.Steps {
			steps = append(steps, stepResponse{
				Number: step.Number, Name: step.Name, Status: step.Status,
				Conclusion: step.Conclusion, StartedAt: step.StartedAt,
				CompletedAt: step.CompletedAt,
			})
		}
		responses = append(responses, jobResponse{
			ID: job.ID, RunID: job.RunID, RunAttempt: job.RunAttempt,
			Name: job.Name, WorkflowName: job.WorkflowName, Status: job.Status,
			Conclusion: job.Conclusion, RunnerName: job.RunnerName,
			PoolName: job.PoolName, LocalSessionID: job.LocalSessionID,
			AttributionSource:     job.AttributionSource,
			AttributionConfidence: job.AttributionConfidence,
			HTMLURL:               job.HTMLURL, CreatedAt: job.CreatedAt,
			StartedAt: job.StartedAt, CompletedAt: job.CompletedAt, Steps: steps,
		})
	}
	return responses
}

func runnerSessionAPIResponses(
	sessions []history.RunnerSession,
) []runnerSessionResponse {
	responses := make([]runnerSessionResponse, 0, len(sessions))
	for _, session := range sessions {
		responses = append(responses, runnerSessionResponse{
			ID: session.ID, RunnerName: session.RunnerName,
			PoolName: session.PoolName, Target: session.Target,
			Status: session.Status, Conclusion: session.Conclusion,
			BackendID:             session.BackendID,
			AttributionSource:     session.AttributionSource,
			AttributionConfidence: session.AttributionConfidence,
			PlannedAt:             session.PlannedAt, RegisteredAt: session.RegisteredAt,
			LaunchedAt: session.LaunchedAt, StartedAt: session.StartedAt,
			CompletedAt: session.CompletedAt, Error: session.Error,
		})
	}
	return responses
}
