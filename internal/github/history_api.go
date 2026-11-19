package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/GerardSmit/multirunner/internal/config"
)

var ErrLogTooLarge = errors.New("workflow job log exceeds the configured limit")

// PageOptions controls one page of a read-only GitHub API request.
type PageOptions struct {
	Page    int
	PerPage int
}

// WorkflowRunListOptions selects repository workflow runs by creation time.
// Zero endpoints leave that side of the range open.
type WorkflowRunListOptions struct {
	CreatedFrom time.Time
	CreatedTo   time.Time
	PageOptions
}

// RateLimit is a safe snapshot of the REST API rate-limit headers.
type RateLimit struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// ResponseMetadata exposes pagination and rate-limit state without leaking the
// authenticated client's response body or transport.
type ResponseMetadata struct {
	StatusCode int
	RequestID  string
	NextPage   int
	PrevPage   int
	FirstPage  int
	LastPage   int
	RateLimit  RateLimit
	RetryAfter time.Duration
}

// WorkflowRun is the read-only workflow-run model needed by history consumers.
type WorkflowRun struct {
	ID              int64
	Name            string
	DisplayTitle    string
	RunNumber       int
	RunAttempt      int
	Event           string
	Status          string
	Conclusion      string
	WorkflowID      int64
	WorkflowPath    string
	HeadBranch      string
	HeadSHA         string
	HTMLURL         string
	Actor           string
	TriggeringActor string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	RunStartedAt    time.Time
}

// WorkflowStep is one step in a workflow job.
type WorkflowStep struct {
	Name        string
	Status      string
	Conclusion  string
	Number      int64
	StartedAt   time.Time
	CompletedAt time.Time
}

// WorkflowJob is the read-only workflow-job model needed by history consumers.
type WorkflowJob struct {
	ID              int64
	RunID           int64
	RunAttempt      int64
	Name            string
	WorkflowName    string
	HeadBranch      string
	HeadSHA         string
	HTMLURL         string
	Status          string
	Conclusion      string
	Labels          []string
	RunnerID        int64
	RunnerName      string
	RunnerGroupID   int64
	RunnerGroupName string
	CreatedAt       time.Time
	StartedAt       time.Time
	CompletedAt     time.Time
	Steps           []WorkflowStep
}

// ListRepositoryWorkflowRuns returns one page of repository workflow runs.
func (c *Client) ListRepositoryWorkflowRuns(ctx context.Context, opts WorkflowRunListOptions) ([]WorkflowRun, ResponseMetadata, error) {
	if err := c.requireRepository("ListRepositoryWorkflowRuns"); err != nil {
		return nil, ResponseMetadata{}, err
	}
	if !opts.CreatedFrom.IsZero() && !opts.CreatedTo.IsZero() && opts.CreatedTo.Before(opts.CreatedFrom) {
		return nil, ResponseMetadata{}, fmt.Errorf("workflow run created range ends before it starts")
	}
	runs, resp, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, c.owner, c.repo, &gh.ListWorkflowRunsOptions{
		Created: createdRange(opts.CreatedFrom, opts.CreatedTo),
		ListOptions: gh.ListOptions{
			Page:    opts.Page,
			PerPage: opts.PerPage,
		},
	})
	meta := responseMetadata(resp)
	if err != nil {
		return nil, meta, fmt.Errorf("list repository workflow runs: %w", err)
	}
	out := make([]WorkflowRun, 0, len(runs.WorkflowRuns))
	for _, run := range runs.WorkflowRuns {
		if run != nil {
			out = append(out, workflowRunModel(run))
		}
	}
	return out, meta, nil
}

// GetWorkflowRun gets one repository workflow run.
func (c *Client) GetWorkflowRun(ctx context.Context, runID int64) (WorkflowRun, ResponseMetadata, error) {
	if err := c.requireRepository("GetWorkflowRun"); err != nil {
		return WorkflowRun{}, ResponseMetadata{}, err
	}
	if runID <= 0 {
		return WorkflowRun{}, ResponseMetadata{}, fmt.Errorf("workflow run id must be positive")
	}
	run, resp, err := c.gh.Actions.GetWorkflowRunByID(ctx, c.owner, c.repo, runID)
	meta := responseMetadata(resp)
	if err != nil {
		return WorkflowRun{}, meta, fmt.Errorf("get workflow run %d: %w", runID, err)
	}
	if run == nil {
		return WorkflowRun{}, meta, fmt.Errorf("get workflow run %d returned no run", runID)
	}
	return workflowRunModel(run), meta, nil
}

// ListWorkflowJobs returns one page of all jobs for a workflow run, including
// jobs from previous attempts.
func (c *Client) ListWorkflowJobs(ctx context.Context, runID int64, opts PageOptions) ([]WorkflowJob, ResponseMetadata, error) {
	if err := c.requireRepository("ListWorkflowJobs"); err != nil {
		return nil, ResponseMetadata{}, err
	}
	if runID <= 0 {
		return nil, ResponseMetadata{}, fmt.Errorf("workflow run id must be positive")
	}
	jobs, resp, err := c.gh.Actions.ListWorkflowJobs(ctx, c.owner, c.repo, runID, &gh.ListWorkflowJobsOptions{
		Filter: "all",
		ListOptions: gh.ListOptions{
			Page:    opts.Page,
			PerPage: opts.PerPage,
		},
	})
	meta := responseMetadata(resp)
	if err != nil {
		return nil, meta, fmt.Errorf("list workflow jobs for run %d: %w", runID, err)
	}
	return workflowJobModels(jobs), meta, nil
}

// ListWorkflowJobsAttempt returns one page of jobs for a specific run attempt.
func (c *Client) ListWorkflowJobsAttempt(ctx context.Context, runID, attempt int64, opts PageOptions) ([]WorkflowJob, ResponseMetadata, error) {
	if err := c.requireRepository("ListWorkflowJobsAttempt"); err != nil {
		return nil, ResponseMetadata{}, err
	}
	if runID <= 0 || attempt <= 0 {
		return nil, ResponseMetadata{}, fmt.Errorf("workflow run id and attempt must be positive")
	}
	jobs, resp, err := c.gh.Actions.ListWorkflowJobsAttempt(ctx, c.owner, c.repo, runID, attempt, &gh.ListOptions{
		Page:    opts.Page,
		PerPage: opts.PerPage,
	})
	meta := responseMetadata(resp)
	if err != nil {
		return nil, meta, fmt.Errorf("list workflow jobs for run %d attempt %d: %w", runID, attempt, err)
	}
	return workflowJobModels(jobs), meta, nil
}

// GetWorkflowJob gets one workflow job.
func (c *Client) GetWorkflowJob(ctx context.Context, jobID int64) (WorkflowJob, ResponseMetadata, error) {
	if err := c.requireRepository("GetWorkflowJob"); err != nil {
		return WorkflowJob{}, ResponseMetadata{}, err
	}
	if jobID <= 0 {
		return WorkflowJob{}, ResponseMetadata{}, fmt.Errorf("workflow job id must be positive")
	}
	job, resp, err := c.gh.Actions.GetWorkflowJobByID(ctx, c.owner, c.repo, jobID)
	meta := responseMetadata(resp)
	if err != nil {
		return WorkflowJob{}, meta, fmt.Errorf("get workflow job %d: %w", jobID, err)
	}
	if job == nil {
		return WorkflowJob{}, meta, fmt.Errorf("get workflow job %d returned no job", jobID)
	}
	return workflowJobModel(job), meta, nil
}

func (c *Client) WorkflowJobLog(
	ctx context.Context, jobID int64, maxBytes int64,
) ([]byte, ResponseMetadata, error) {
	if err := c.requireRepository("WorkflowJobLog"); err != nil {
		return nil, ResponseMetadata{}, err
	}
	if jobID <= 0 {
		return nil, ResponseMetadata{}, fmt.Errorf("workflow job id must be positive")
	}
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	if maxBytes > 16<<20 {
		maxBytes = 16 << 20
	}
	location, response, err := c.gh.Actions.GetWorkflowJobLogs(
		ctx, c.owner, c.repo, jobID, 0,
	)
	meta := responseMetadata(response)
	if err != nil {
		return nil, meta, fmt.Errorf("request workflow job log %d: %w", jobID, err)
	}
	if location == nil || location.Scheme != "https" || location.Host == "" ||
		location.User != nil {
		return nil, meta, errors.New("workflow job log redirect is not a safe HTTPS URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return nil, meta, fmt.Errorf("create workflow job log request: %w", err)
	}
	transport := http.DefaultTransport
	if c.downloadClient != nil && c.downloadClient.Transport != nil {
		transport = c.downloadClient.Transport
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	download, err := client.Do(request)
	if err != nil {
		return nil, meta, fmt.Errorf("download workflow job log %d: %w", jobID, err)
	}
	defer download.Body.Close()
	if download.StatusCode != http.StatusOK {
		return nil, meta, fmt.Errorf("download workflow job log %d: unexpected status %s",
			jobID, download.Status)
	}
	data, err := io.ReadAll(io.LimitReader(download.Body, maxBytes+1))
	if err != nil {
		return nil, meta, fmt.Errorf("read workflow job log %d: %w", jobID, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, meta, ErrLogTooLarge
	}
	return data, meta, nil
}

func (c *Client) requireRepository(method string) error {
	if c.scope != config.ScopeRepo || c.owner == "" || c.repo == "" {
		return fmt.Errorf("%s requires a repo-scoped client", method)
	}
	return nil
}

func createdRange(from, to time.Time) string {
	switch {
	case !from.IsZero() && !to.IsZero():
		return from.UTC().Format(time.RFC3339) + ".." + to.UTC().Format(time.RFC3339)
	case !from.IsZero():
		return ">=" + from.UTC().Format(time.RFC3339)
	case !to.IsZero():
		return "<=" + to.UTC().Format(time.RFC3339)
	default:
		return ""
	}
}

func responseMetadata(resp *gh.Response) ResponseMetadata {
	if resp == nil {
		return ResponseMetadata{}
	}
	meta := ResponseMetadata{
		NextPage:  resp.NextPage,
		PrevPage:  resp.PrevPage,
		FirstPage: resp.FirstPage,
		LastPage:  resp.LastPage,
		RateLimit: RateLimit{
			Limit:     resp.Rate.Limit,
			Remaining: resp.Rate.Remaining,
			Reset:     resp.Rate.Reset.Time,
		},
	}
	if resp.Response != nil {
		meta.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	if resp.Response != nil {
		meta.StatusCode = resp.StatusCode
		meta.RequestID = resp.Header.Get("X-GitHub-Request-Id")
	}
	return meta
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := when.Sub(now)
		if delay > 0 {
			return delay
		}
	}
	return 0
}

func workflowRunModel(run *gh.WorkflowRun) WorkflowRun {
	out := WorkflowRun{
		ID:           run.GetID(),
		Name:         run.GetName(),
		DisplayTitle: run.GetDisplayTitle(),
		RunNumber:    run.GetRunNumber(),
		RunAttempt:   run.GetRunAttempt(),
		Event:        run.GetEvent(),
		Status:       run.GetStatus(),
		Conclusion:   run.GetConclusion(),
		WorkflowID:   run.GetWorkflowID(),
		WorkflowPath: run.GetPath(),
		HeadBranch:   run.GetHeadBranch(),
		HeadSHA:      run.GetHeadSHA(),
		HTMLURL:      run.GetHTMLURL(),
	}
	if actor := run.GetActor(); actor != nil {
		out.Actor = actor.GetLogin()
	}
	if actor := run.GetTriggeringActor(); actor != nil {
		out.TriggeringActor = actor.GetLogin()
	}
	if run.CreatedAt != nil {
		out.CreatedAt = run.CreatedAt.Time
	}
	if run.UpdatedAt != nil {
		out.UpdatedAt = run.UpdatedAt.Time
	}
	if run.RunStartedAt != nil {
		out.RunStartedAt = run.RunStartedAt.Time
	}
	return out
}

func workflowJobModels(jobs *gh.Jobs) []WorkflowJob {
	if jobs == nil {
		return nil
	}
	out := make([]WorkflowJob, 0, len(jobs.Jobs))
	for _, job := range jobs.Jobs {
		if job != nil {
			out = append(out, workflowJobModel(job))
		}
	}
	return out
}

func workflowJobModel(job *gh.WorkflowJob) WorkflowJob {
	out := WorkflowJob{
		ID:              job.GetID(),
		RunID:           job.GetRunID(),
		RunAttempt:      job.GetRunAttempt(),
		Name:            job.GetName(),
		WorkflowName:    job.GetWorkflowName(),
		HeadBranch:      job.GetHeadBranch(),
		HeadSHA:         job.GetHeadSHA(),
		HTMLURL:         job.GetHTMLURL(),
		Status:          job.GetStatus(),
		Conclusion:      job.GetConclusion(),
		Labels:          append([]string(nil), job.Labels...),
		RunnerID:        job.GetRunnerID(),
		RunnerName:      job.GetRunnerName(),
		RunnerGroupID:   job.GetRunnerGroupID(),
		RunnerGroupName: job.GetRunnerGroupName(),
	}
	if job.CreatedAt != nil {
		out.CreatedAt = job.CreatedAt.Time
	}
	if job.StartedAt != nil {
		out.StartedAt = job.StartedAt.Time
	}
	if job.CompletedAt != nil {
		out.CompletedAt = job.CompletedAt.Time
	}
	out.Steps = make([]WorkflowStep, 0, len(job.Steps))
	for _, step := range job.Steps {
		if step == nil {
			continue
		}
		item := WorkflowStep{
			Name:       step.GetName(),
			Status:     step.GetStatus(),
			Conclusion: step.GetConclusion(),
			Number:     step.GetNumber(),
		}
		if step.StartedAt != nil {
			item.StartedAt = step.StartedAt.Time
		}
		if step.CompletedAt != nil {
			item.CompletedAt = step.CompletedAt.Time
		}
		out.Steps = append(out.Steps, item)
	}
	return out
}
