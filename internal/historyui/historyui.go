// Package historyui provides a read-only localhost history dashboard.
package historyui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/history"
)

const (
	DefaultPageSize = 25
	MaxPageSize     = 100
	scanPageSize    = 1000
)

var (
	//go:embed templates/*.html assets/*
	embeddedFiles embed.FS

	repositoryPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	tokenPattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	allowedStatuses    = []string{"queued", "in_progress", "completed", "waiting", "requested", "pending"}
	allowedConclusions = []string{
		"success", "failure", "cancelled", "skipped", "timed_out",
		"action_required", "neutral", "stale", "startup_failure",
	}
)

// Reader is the narrow read-only history surface used by the dashboard.
// history.Store implements this interface.
type Reader interface {
	Summary(context.Context, string) (history.Summary, error)
	ListWorkflowRuns(context.Context, history.ListOptions) ([]history.WorkflowRun, error)
	ListWorkflowJobs(context.Context, history.ListOptions) ([]history.WorkflowJob, error)
	ListRunnerSessions(context.Context, history.ListOptions) ([]history.RunnerSession, error)
	SyncState(context.Context, string) (history.SyncState, error)
}

var _ Reader = (*history.Store)(nil)

// Handler serves the embedded dashboard and JSON endpoints.
type Handler struct {
	reader    Reader
	templates *template.Template
	assets    http.Handler
}

// New constructs a dashboard handler.
func New(reader Reader) (*Handler, error) {
	if reader == nil {
		return nil, errors.New("history reader is required")
	}
	functions := template.FuncMap{
		"displayRun":    displayRun,
		"pageURL":       pageURL,
		"runPath":       runPath,
		"safeGitHubURL": safeGitHubURL,
		"shortSHA":      shortSHA,
		"stateClass":    stateClass,
		"stateText":     stateText,
		"timeTag":       timeTag,
		"timeTagPtr":    timeTagPtr,
	}
	templates, err := template.New("").Funcs(functions).ParseFS(embeddedFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse history UI templates: %w", err)
	}
	assetsFS, err := fs.Sub(embeddedFiles, "assets")
	if err != nil {
		return nil, fmt.Errorf("open history UI assets: %w", err)
	}
	return &Handler{
		reader:    reader,
		templates: templates,
		assets:    http.StripPrefix("/assets/", http.FileServer(http.FS(assetsFS))),
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSONError(w, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		} else {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		h.assets.ServeHTTP(w, r)
	case r.URL.Path == "/":
		h.overview(w, r)
	case r.URL.Path == "/runs":
		h.runs(w, r)
	case strings.HasPrefix(r.URL.Path, "/runs/"):
		h.runDetail(w, r)
	case r.URL.Path == "/api/summary":
		h.apiSummary(w, r)
	case r.URL.Path == "/api/runs":
		h.apiRuns(w, r)
	case r.URL.Path == "/api/jobs":
		h.apiJobs(w, r)
	case r.URL.Path == "/api/repositories":
		h.apiRepositories(w, r)
	case r.URL.Path == "/api/pools":
		h.apiPools(w, r)
	case r.URL.Path == "/api/sync-state":
		h.apiSyncState(w, r)
	default:
		http.NotFound(w, r)
	}
}

type filter struct {
	Repository string
	Status     string
}

type pageRequest struct {
	filter
	Page     int
	PageSize int
}

type pagination struct {
	Page        int  `json:"page"`
	PageSize    int  `json:"page_size"`
	HasPrevious bool `json:"has_previous"`
	HasNext     bool `json:"has_next"`
}

type overviewData struct {
	Summary history.Summary
	Runs    []history.WorkflowRun
	Error   string
}

type runsData struct {
	Runs         []history.WorkflowRun
	Filter       filter
	Pagination   pagination
	Statuses     []string
	MaxPageSize  int
	PreviousPage int
	NextPage     int
	Error        string
}

type attribution struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

type attributedJob struct {
	Job         history.WorkflowJob `json:"job"`
	Attribution attribution         `json:"attribution"`
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
	UpdatedAt       time.Time  `json:"updated_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

type jobResponse struct {
	ID                    int64                  `json:"id"`
	RunID                 int64                  `json:"run_id"`
	RunAttempt            int                    `json:"run_attempt"`
	Repository            string                 `json:"repository"`
	Name                  string                 `json:"name"`
	Status                string                 `json:"status"`
	Conclusion            string                 `json:"conclusion"`
	RunnerName            string                 `json:"runner_name"`
	RunnerGroup           string                 `json:"runner_group"`
	PoolName              string                 `json:"pool_name"`
	LocalSessionID        string                 `json:"local_session_id"`
	AttributionSource     string                 `json:"attribution_source"`
	AttributionConfidence int                    `json:"attribution_confidence"`
	HTMLURL               string                 `json:"html_url"`
	CreatedAt             time.Time              `json:"created_at"`
	StartedAt             *time.Time             `json:"started_at,omitempty"`
	CompletedAt           *time.Time             `json:"completed_at,omitempty"`
	UpdatedAt             time.Time              `json:"updated_at"`
	Steps                 []history.WorkflowStep `json:"steps"`
}

type runData struct {
	Run   history.WorkflowRun
	Jobs  []attributedJob
	Error string
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	data := overviewData{}
	summary, err := h.reader.Summary(r.Context(), "")
	if err != nil {
		data.Error = err.Error()
	} else {
		data.Summary = summary
	}
	runs, err := h.reader.ListWorkflowRuns(r.Context(), history.ListOptions{Limit: 10})
	if err != nil {
		if data.Error == "" {
			data.Error = err.Error()
		}
	} else {
		data.Runs = runs
	}
	h.render(w, "index.html", data)
}

func (h *Handler) runs(w http.ResponseWriter, r *http.Request) {
	request, err := parsePageRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	runs, hasNext, err := h.listRunsPage(r.Context(), request)
	data := runsData{
		Runs: runs, Filter: request.filter,
		Pagination: pagination{
			Page: request.Page, PageSize: request.PageSize,
			HasPrevious: request.Page > 1, HasNext: hasNext,
		},
		Statuses: allowedStatuses, MaxPageSize: MaxPageSize,
		PreviousPage: request.Page - 1, NextPage: request.Page + 1,
	}
	if err != nil {
		data.Error = err.Error()
	}
	h.render(w, "runs.html", data)
}

func (h *Handler) runDetail(w http.ResponseWriter, r *http.Request) {
	repository, runID, err := parseRunPath(r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := h.findRun(r.Context(), repository, runID)
	if errors.Is(err, history.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.renderStatus(w, "run.html", runData{
			Run:   history.WorkflowRun{ID: runID, Repository: repository},
			Error: err.Error(),
		}, http.StatusServiceUnavailable)
		return
	}
	jobs, jobErr := h.jobsForRun(r.Context(), repository, runID)
	sessions, sessionErr := h.allSessions(r.Context(), repository)
	data := runData{Run: run}
	if jobErr != nil {
		data.Error = jobErr.Error()
	} else {
		data.Jobs = attributeJobs(jobs, sessions)
	}
	if sessionErr != nil && data.Error == "" {
		data.Error = "runner attribution unavailable: " + sessionErr.Error()
	}
	h.render(w, "run.html", data)
}

func (h *Handler) apiSummary(w http.ResponseWriter, r *http.Request) {
	repository, err := parseRepository(r.URL.Query().Get("repository"), true)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	summary, err := h.reader.Summary(r.Context(), repository)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		RunnerSessions         int64   `json:"runner_sessions"`
		PendingSessions        int64   `json:"pending_sessions"`
		WorkflowRuns           int64   `json:"workflow_runs"`
		WorkflowJobs           int64   `json:"workflow_jobs"`
		SuccessfulJobs         int64   `json:"successful_jobs"`
		FailedJobs             int64   `json:"failed_jobs"`
		CancelledJobs          int64   `json:"cancelled_jobs"`
		AverageDurationSeconds float64 `json:"average_duration_seconds"`
		WebhookDeliveries      int64   `json:"webhook_deliveries"`
	}{
		RunnerSessions: summary.RunnerSessions, PendingSessions: summary.PendingSessions,
		WorkflowRuns: summary.WorkflowRuns, WorkflowJobs: summary.WorkflowJobs,
		SuccessfulJobs: summary.SuccessfulJobs, FailedJobs: summary.FailedJobs,
		CancelledJobs:          summary.CancelledJobs,
		AverageDurationSeconds: summary.AverageDurationSeconds,
		WebhookDeliveries:      summary.WebhookDeliveries,
	})
}

func (h *Handler) apiRuns(w http.ResponseWriter, r *http.Request) {
	request, err := parsePageRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	options, err := parseAPIListOptions(r, request, false)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	runs, err := h.reader.ListWorkflowRuns(r.Context(), options)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	hasNext := len(runs) > request.PageSize
	if hasNext {
		runs = runs[:request.PageSize]
	}
	writeJSON(w, http.StatusOK, struct {
		Runs       []runResponse `json:"runs"`
		Pagination pagination    `json:"pagination"`
	}{
		Runs: runResponses(runs),
		Pagination: pagination{
			Page: request.Page, PageSize: request.PageSize,
			HasPrevious: request.Page > 1, HasNext: hasNext,
		},
	})
}

func (h *Handler) apiJobs(w http.ResponseWriter, r *http.Request) {
	request, err := parsePageRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	runID, err := positiveInt64(r.URL.Query().Get("run_id"), true)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid run_id: %w", err))
		return
	}
	var jobs []history.WorkflowJob
	var hasNext bool
	if runID == 0 {
		options, optionsErr := parseAPIListOptions(r, request, true)
		if optionsErr != nil {
			writeJSONError(w, http.StatusBadRequest, optionsErr)
			return
		}
		jobs, err = h.reader.ListWorkflowJobs(r.Context(), options)
		if len(jobs) > request.PageSize {
			hasNext, jobs = true, jobs[:request.PageSize]
		}
	} else {
		if request.Repository == "" {
			writeJSONError(w, http.StatusBadRequest, errors.New("repository is required with run_id"))
			return
		}
		var all []history.WorkflowJob
		all, err = h.jobsForRun(r.Context(), request.Repository, runID)
		if err == nil && request.Status != "" {
			all = slices.DeleteFunc(all, func(job history.WorkflowJob) bool { return job.Status != request.Status })
		}
		jobs, hasNext = slicePage(all, request.Page, request.PageSize)
	}
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Jobs       []jobResponse `json:"jobs"`
		Pagination pagination    `json:"pagination"`
	}{
		Jobs: jobResponses(jobs),
		Pagination: pagination{
			Page: request.Page, PageSize: request.PageSize,
			HasPrevious: request.Page > 1, HasNext: hasNext,
		},
	})
}

func (h *Handler) apiRepositories(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := parseSimplePage(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	repositories, err := h.repositories(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	values, hasNext := slicePage(repositories, page, pageSize)
	writeJSON(w, http.StatusOK, struct {
		Repositories []string   `json:"repositories"`
		Pagination   pagination `json:"pagination"`
	}{
		Repositories: nonnilStrings(values),
		Pagination: pagination{
			Page: page, PageSize: pageSize, HasPrevious: page > 1, HasNext: hasNext,
		},
	})
}

func (h *Handler) apiPools(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := parseSimplePage(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	sessions, err := h.allSessions(r.Context(), "")
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	set := make(map[string]struct{})
	for _, session := range sessions {
		if session.PoolName != "" {
			set[session.PoolName] = struct{}{}
		}
	}
	pools := sortedKeys(set)
	values, hasNext := slicePage(pools, page, pageSize)
	writeJSON(w, http.StatusOK, struct {
		Pools      []string   `json:"pools"`
		Pagination pagination `json:"pagination"`
	}{
		Pools: nonnilStrings(values),
		Pagination: pagination{
			Page: page, PageSize: pageSize, HasPrevious: page > 1, HasNext: hasNext,
		},
	})
}

func (h *Handler) apiSyncState(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" || len(key) > 200 || strings.ContainsAny(key, "\r\n\x00") {
		writeJSONError(w, http.StatusBadRequest, errors.New("valid sync state key is required"))
		return
	}
	state, err := h.reader.SyncState(r.Context(), key)
	if errors.Is(err, history.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Key              string     `json:"key"`
		Repository       string     `json:"repository,omitempty"`
		Phase            string     `json:"phase,omitempty"`
		Cursor           string     `json:"cursor"`
		LastSuccessAt    *time.Time `json:"last_success_at,omitempty"`
		LastError        string     `json:"last_error,omitempty"`
		RetryAfter       *time.Time `json:"retry_after,omitempty"`
		BackfillComplete bool       `json:"backfill_complete"`
		RateRemaining    int        `json:"rate_remaining"`
		RateResetAt      *time.Time `json:"rate_reset_at,omitempty"`
		UpdatedAt        time.Time  `json:"updated_at"`
	}{
		Key: state.Key, Repository: state.Repository, Phase: state.Phase,
		Cursor: state.Cursor, LastSuccessAt: state.LastSuccessAt,
		LastError: state.LastError, RetryAfter: state.RetryAfter,
		BackfillComplete: state.BackfillComplete, RateRemaining: state.RateRemaining,
		RateResetAt: state.RateResetAt, UpdatedAt: state.UpdatedAt,
	})
}

func (h *Handler) listRunsPage(ctx context.Context, request pageRequest) ([]history.WorkflowRun, bool, error) {
	runs, err := h.reader.ListWorkflowRuns(ctx, history.ListOptions{
		Repository: request.Repository, Status: request.Status,
		Limit: request.PageSize + 1, Offset: (request.Page - 1) * request.PageSize,
	})
	if err != nil {
		return nil, false, err
	}
	if len(runs) > request.PageSize {
		return runs[:request.PageSize], true, nil
	}
	return runs, false, nil
}

func (h *Handler) findRun(ctx context.Context, repository string, runID int64) (history.WorkflowRun, error) {
	var afterTime *time.Time
	var afterID string
	for {
		runs, err := h.reader.ListWorkflowRuns(ctx, history.ListOptions{
			Repository: repository, Limit: scanPageSize,
			AfterTime: afterTime, AfterID: afterID,
		})
		if err != nil {
			return history.WorkflowRun{}, err
		}
		for _, run := range runs {
			if run.ID == runID {
				return run, nil
			}
		}
		if len(runs) < scanPageSize {
			return history.WorkflowRun{}, history.ErrNotFound
		}
		last := runs[len(runs)-1]
		afterTime, afterID = &last.CreatedAt, strconv.FormatInt(last.ID, 10)
	}
}

func (h *Handler) jobsForRun(ctx context.Context, repository string, runID int64) ([]history.WorkflowJob, error) {
	var result []history.WorkflowJob
	for offset := 0; ; offset += scanPageSize {
		jobs, err := h.reader.ListWorkflowJobs(ctx, history.ListOptions{
			Repository: repository, Limit: scanPageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			if job.RunID == runID {
				result = append(result, job)
			}
		}
		if len(jobs) < scanPageSize {
			return result, nil
		}
	}
}

func (h *Handler) allSessions(ctx context.Context, repository string) ([]history.RunnerSession, error) {
	var result []history.RunnerSession
	var afterTime *time.Time
	var afterID string
	for {
		sessions, err := h.reader.ListRunnerSessions(ctx, history.ListOptions{
			Repository: repository, Limit: scanPageSize,
			AfterTime: afterTime, AfterID: afterID,
		})
		if err != nil {
			return nil, err
		}
		result = append(result, sessions...)
		if len(sessions) < scanPageSize {
			return result, nil
		}
		last := sessions[len(sessions)-1]
		afterTime, afterID = &last.StartedAt, last.ID
	}
}

func (h *Handler) repositories(ctx context.Context) ([]string, error) {
	set := make(map[string]struct{})
	var afterTime *time.Time
	var afterID string
	for {
		runs, err := h.reader.ListWorkflowRuns(ctx, history.ListOptions{
			Limit: scanPageSize, AfterTime: afterTime, AfterID: afterID,
		})
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			if run.Repository != "" {
				set[run.Repository] = struct{}{}
			}
		}
		if len(runs) < scanPageSize {
			break
		}
		last := runs[len(runs)-1]
		afterTime, afterID = &last.CreatedAt, strconv.FormatInt(last.ID, 10)
	}
	sessions, err := h.allSessions(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, session := range sessions {
		if session.Repository != "" {
			set[session.Repository] = struct{}{}
		}
	}
	return sortedKeys(set), nil
}

func attributeJobs(jobs []history.WorkflowJob, sessions []history.RunnerSession) []attributedJob {
	byJob := make(map[int64]history.RunnerSession)
	for _, session := range sessions {
		if session.WorkflowJobID != 0 {
			byJob[session.WorkflowJobID] = session
		}
	}
	result := make([]attributedJob, 0, len(jobs))
	for _, job := range jobs {
		item := attributedJob{Job: job}
		if job.PoolName != "" || job.AttributionSource != "" {
			item.Attribution = attribution{
				Name: firstNonempty(job.PoolName, job.RunnerGroup, job.RunnerName),
				Kind: attributionKind(job.AttributionSource),
			}
		} else if session, ok := byJob[job.ID]; ok {
			item.Attribution = attribution{Name: firstNonempty(session.PoolName, session.RunnerName), Kind: "exact"}
		} else if name := firstNonempty(job.RunnerGroup, job.RunnerName); name != "" {
			item.Attribution = attribution{Name: name, Kind: "inferred"}
		}

		result = append(result, item)
	}
	return result
}

func attributionKind(source string) string {
	if source == "" {
		return ""
	}
	if strings.Contains(source, "job_id") || strings.HasPrefix(source, "exact") {
		return "exact"
	}
	return "inferred"
}

func parsePageRequest(r *http.Request) (pageRequest, error) {
	repository, err := parseRepository(r.URL.Query().Get("repository"), true)
	if err != nil {
		return pageRequest{}, err
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !slices.Contains(allowedStatuses, status) {
		return pageRequest{}, fmt.Errorf("invalid status %q", status)
	}
	page, err := positiveInt(r.URL.Query().Get("page"), 1, 1_000_000)
	if err != nil {
		return pageRequest{}, fmt.Errorf("invalid page: %w", err)
	}
	pageSize, err := positiveInt(r.URL.Query().Get("page_size"), DefaultPageSize, MaxPageSize)
	if err != nil {
		return pageRequest{}, fmt.Errorf("invalid page_size: %w", err)
	}
	return pageRequest{
		filter: filter{Repository: repository, Status: status},
		Page:   page, PageSize: pageSize,
	}, nil
}

func parseAPIListOptions(r *http.Request, request pageRequest, jobs bool) (history.ListOptions, error) {
	query := r.URL.Query()
	conclusion := strings.TrimSpace(query.Get("conclusion"))
	if conclusion != "" && !slices.Contains(allowedConclusions, conclusion) {
		return history.ListOptions{}, fmt.Errorf("invalid conclusion %q", conclusion)
	}
	workflow, err := safeTextFilter(query.Get("workflow"), "workflow")
	if err != nil {
		return history.ListOptions{}, err
	}
	branch, err := safeTextFilter(query.Get("branch"), "branch")
	if err != nil {
		return history.ListOptions{}, err
	}
	actor, err := safeTextFilter(query.Get("actor"), "actor")
	if err != nil {
		return history.ListOptions{}, err
	}
	search, err := safeTextFilter(query.Get("q"), "q")
	if err != nil {
		return history.ListOptions{}, err
	}
	since, err := optionalTime(query.Get("since"), "since")
	if err != nil {
		return history.ListOptions{}, err
	}
	until, err := optionalTime(query.Get("until"), "until")
	if err != nil {
		return history.ListOptions{}, err
	}
	if since != nil && until != nil && since.After(*until) {
		return history.ListOptions{}, errors.New("since must not be after until")
	}
	options := history.ListOptions{
		Repository: request.Repository, Status: request.Status, Conclusion: conclusion,
		Workflow: workflow, Branch: branch, Actor: actor, Query: search,
		Since: since, Until: until, Limit: request.PageSize + 1,
		Offset: (request.Page - 1) * request.PageSize,
	}
	if !jobs {
		return options, nil
	}
	options.Job, err = safeTextFilter(query.Get("job"), "job")
	if err != nil {
		return history.ListOptions{}, err
	}
	options.Pool, err = safeTokenFilter(query.Get("pool"), "pool")
	if err != nil {
		return history.ListOptions{}, err
	}
	options.Attribution, err = safeTokenFilter(query.Get("attribution"), "attribution")
	if err != nil {
		return history.ListOptions{}, err
	}
	return options, nil
}

func parseSimplePage(r *http.Request) (int, int, error) {
	page, err := positiveInt(r.URL.Query().Get("page"), 1, 1_000_000)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid page: %w", err)
	}
	pageSize, err := positiveInt(r.URL.Query().Get("page_size"), DefaultPageSize, MaxPageSize)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid page_size: %w", err)
	}
	return page, pageSize, nil
}

func safeTextFilter(value, name string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > 200 || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("%s is invalid", name)
	}
	return value, nil
}

func safeTokenFilter(value, name string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "" && !tokenPattern.MatchString(value) {
		return "", fmt.Errorf("%s is invalid", name)
	}
	return value, nil
}

func optionalTime(value, name string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("%s must be RFC3339", name)
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func parseRepository(value string, optional bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" && optional {
		return "", nil
	}
	if len(value) > 200 || !repositoryPattern.MatchString(value) {
		return "", errors.New("repository must be in owner/name form")
	}
	return value, nil
}

func parseRunPath(path string) (string, int64, error) {
	value := strings.TrimPrefix(path, "/runs/")
	index := strings.LastIndexByte(value, '/')
	if index < 1 {
		return "", 0, errors.New("invalid run path")
	}
	repository, err := parseRepository(value[:index], false)
	if err != nil {
		return "", 0, err
	}
	runID, err := positiveInt64(value[index+1:], false)
	return repository, runID, err
}

func positiveInt(value string, defaultValue, max int) (int, error) {
	if value == "" {
		return defaultValue, nil
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 || number > max {
		return 0, fmt.Errorf("must be between 1 and %d", max)
	}
	return number, nil
}

func positiveInt64(value string, optional bool) (int64, error) {
	if value == "" && optional {
		return 0, nil
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 1 {
		return 0, errors.New("must be a positive integer")
	}
	return number, nil
}

func slicePage[T any](values []T, page, pageSize int) ([]T, bool) {
	start := (page - 1) * pageSize
	if start >= len(values) {
		return nil, false
	}
	end := min(start+pageSize, len(values))
	return values[start:end], end < len(values)
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	h.renderStatus(w, name, data, http.StatusOK)
}

func (h *Handler) renderStatus(w http.ResponseWriter, name string, data any, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		// Headers may already be committed; terminate the response without exposing internals.
		return
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{
		"code": "request_failed", "message": err.Error(),
		"correlation_id": "", "field_violations": []any{}, "retry": nil,
	})
}

func displayRun(run history.WorkflowRun) string {
	if run.DisplayTitle != "" {
		return run.DisplayTitle
	}
	if run.WorkflowName != "" {
		return run.WorkflowName
	}
	if run.Name != "" {
		return run.Name
	}
	return fmt.Sprintf("Run %d", run.ID)
}

func stateText(status, conclusion string) string {
	if conclusion != "" {
		return conclusion
	}
	if status != "" {
		return status
	}
	return "unknown"
}

func stateClass(status, conclusion string) string {
	return strings.ToLower(strings.ReplaceAll(stateText(status, conclusion), " ", "_"))
}

func shortSHA(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

func runPath(repository string, runID int64) string {
	parts := strings.Split(repository, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return "/runs/" + strings.Join(parts, "/") + "/" + strconv.FormatInt(runID, 10)
}

func pageURL(filter filter, pageSize, page int) string {
	query := make(url.Values)
	if filter.Repository != "" {
		query.Set("repository", filter.Repository)
	}
	if filter.Status != "" {
		query.Set("status", filter.Status)
	}
	query.Set("page", strconv.Itoa(page))
	query.Set("page_size", strconv.Itoa(pageSize))
	return "/runs?" + query.Encode()
}

func safeGitHubURL(value string) template.URL {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return ""
	}
	return template.URL(parsed.String())
}

func timeTag(value time.Time) template.HTML {
	if value.IsZero() {
		return template.HTML(`<span class="muted">Unknown</span>`)
	}
	escaped := template.HTMLEscapeString(value.UTC().Format(time.RFC3339))
	return template.HTML(`<time data-local datetime="` + escaped + `">` + escaped + `</time>`)
}

func timeTagPtr(value *time.Time) template.HTML {
	if value == nil {
		return timeTag(time.Time{})
	}
	return timeTag(*value)
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func runResponses(values []history.WorkflowRun) []runResponse {
	result := make([]runResponse, 0, len(values))
	for _, run := range values {
		result = append(result, runResponse{
			ID: run.ID, Repository: run.Repository, Name: run.Name,
			WorkflowName: run.WorkflowName, DisplayTitle: run.DisplayTitle,
			Event: run.Event, Status: run.Status, Conclusion: run.Conclusion,
			HeadBranch: run.HeadBranch, HeadSHA: run.HeadSHA, Actor: run.Actor,
			TriggeringActor: run.TriggeringActor, HTMLURL: run.HTMLURL,
			RunNumber: run.RunNumber, RunAttempt: run.RunAttempt,
			CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
			StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
		})
	}
	return result
}

func jobResponses(values []history.WorkflowJob) []jobResponse {
	result := make([]jobResponse, 0, len(values))
	for _, job := range values {
		steps := job.Steps
		if steps == nil {
			steps = []history.WorkflowStep{}
		}
		result = append(result, jobResponse{
			ID: job.ID, RunID: job.RunID, RunAttempt: job.RunAttempt,
			Repository: job.Repository, Name: job.Name, Status: job.Status,
			Conclusion: job.Conclusion, RunnerName: job.RunnerName,
			RunnerGroup: job.RunnerGroup, PoolName: job.PoolName,
			LocalSessionID: job.LocalSessionID, AttributionSource: job.AttributionSource,
			AttributionConfidence: job.AttributionConfidence, HTMLURL: job.HTMLURL,
			CreatedAt: job.CreatedAt, StartedAt: job.StartedAt,
			CompletedAt: job.CompletedAt, UpdatedAt: job.UpdatedAt, Steps: steps,
		})
	}
	return result
}

func nonnilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
