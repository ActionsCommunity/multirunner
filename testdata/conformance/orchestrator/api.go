package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	githubapi "github.com/google/go-github/v66/github"
)

type apiClient struct {
	github *githubapi.Client
	token  string
}

type dispatchResponse struct {
	WorkflowRunID int64 `json:"workflow_run_id"`
}

func newAPIClient(token string, serverRawURL string, apiRawURL string) (*apiClient, error) {
	apiBase, err := validatedAPIBase(serverRawURL, apiRawURL)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(request *http.Request, previous []*http.Request) error {
			if !sameOrigin(request.URL, apiBase) ||
				!strings.HasPrefix(request.URL.EscapedPath(), apiBase.EscapedPath()) {
				return errors.New("GitHub API redirect left the validated API base")
			}
			if len(previous) >= 3 {
				return errors.New("GitHub API redirect limit exceeded")
			}
			return nil
		},
	}
	client := githubapi.NewClient(httpClient).WithAuthToken(token)
	client.BaseURL = apiBase
	return &apiClient{
		github: client,
		token:  token,
	}, nil
}

func validatedAPIBase(serverRawURL string, apiRawURL string) (*url.URL, error) {
	serverURL, err := parseHTTPSBaseURL("GITHUB_SERVER_URL", serverRawURL)
	if err != nil {
		return nil, err
	}
	apiURL, err := parseHTTPSBaseURL("GITHUB_API_URL", apiRawURL)
	if err != nil {
		return nil, err
	}

	serverPath := strings.TrimRight(serverURL.EscapedPath(), "/")
	apiPath := strings.TrimRight(apiURL.EscapedPath(), "/")
	isDotCom := strings.EqualFold(serverURL.Host, "github.com") && serverPath == ""
	if isDotCom {
		if !strings.EqualFold(apiURL.Host, "api.github.com") || apiPath != "" {
			return nil, errors.New("GITHUB_API_URL does not match github.com")
		}
	} else if !sameOrigin(serverURL, apiURL) || apiPath != serverPath+"/api/v3" {
		return nil, errors.New("GITHUB_API_URL is not the API base for GITHUB_SERVER_URL")
	}

	apiURL.Path = strings.TrimRight(apiURL.Path, "/") + "/"
	apiURL.RawPath = ""
	return apiURL, nil
}

func parseHTTPSBaseURL(name string, raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Opaque != "" {
		return nil, fmt.Errorf("%s must be an HTTPS base URL without credentials, query, or fragment", name)
	}
	return parsed, nil
}

func sameOrigin(left *url.URL, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Host, right.Host)
}

func execute(ctx context.Context, client *apiClient, opts options, out io.Writer) ([]report, error) {
	type dispatchedRun struct {
		target target
		runID  int64
	}

	for _, target := range opts.Targets {
		if err := client.validateTarget(ctx, target); err != nil {
			return nil, err
		}
	}

	dispatched := make([]dispatchedRun, 0, len(opts.Targets))
	for _, target := range opts.Targets {
		runID, err := client.dispatch(ctx, target, opts)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "target=%s phase=dispatched run_id=%d\n", target.Repository, runID)
		dispatched = append(dispatched, dispatchedRun{target: target, runID: runID})
	}

	reports := make([]report, 0, len(dispatched))
	for _, item := range dispatched {
		result, err := waitForRun(ctx, client, item.target.Repository, item.runID, opts, out)
		if err != nil {
			return nil, err
		}
		reports = append(reports, result)
	}
	return reports, nil
}

func (c *apiClient) validateTarget(ctx context.Context, target target) error {
	owner, repo := splitRepository(target.Repository)
	branch, _, err := c.github.Repositories.GetBranch(ctx, owner, repo, target.Ref, 0)
	if err != nil {
		return c.apiError(fmt.Sprintf("get branch %s in %s", target.Ref, target.Repository), err)
	}
	if branch.GetName() != target.Ref || !branch.GetProtected() {
		return fmt.Errorf("target %s ref %s must resolve to the named protected branch", target.Repository, target.Ref)
	}
	return nil
}

func waitForRun(
	ctx context.Context,
	client *apiClient,
	repository string,
	runID int64,
	opts options,
	out io.Writer,
) (report, error) {
	for {
		run, err := client.workflowRun(ctx, repository, runID)
		if err != nil {
			return report{}, err
		}
		fmt.Fprintf(out, "target=%s run_id=%d phase=workload status=%s conclusion=%s\n",
			repository, runID, run.GetStatus(), run.GetConclusion())
		if run.GetStatus() == "completed" {
			jobs, err := client.workflowJobs(ctx, repository, runID)
			if err != nil {
				return report{}, err
			}
			startedJobs := 0
			for _, job := range jobs {
				if job == nil || job.StartedAt == nil {
					continue
				}
				startedJobs++
				if !strings.HasPrefix(job.GetRunnerName(), opts.RunnerPrefix) {
					return report{}, fmt.Errorf(
						"target %s job %s ran on unowned runner %s",
						repository, job.GetName(), job.GetRunnerName(),
					)
				}
				fmt.Fprintf(
					out,
					"target=%s pool=conformance runner=%s phase=workload job=%s conclusion=%s\n",
					repository, job.GetRunnerName(), job.GetName(), job.GetConclusion(),
				)
			}
			if run.GetConclusion() != "success" {
				return report{}, fmt.Errorf("target %s run %d concluded %s", repository, runID, run.GetConclusion())
			}
			if startedJobs == 0 {
				return report{}, fmt.Errorf("target %s run %d exposed no started jobs", repository, runID)
			}
			queue, err := queueLatency(run.GetCreatedAt().Time, jobs)
			if err != nil {
				return report{}, fmt.Errorf("target %s run %d: %w", repository, runID, err)
			}
			fmt.Fprintf(out, "target=%s run_id=%d queue_to_start_ms=%d limit_ms=%d\n",
				repository, runID, queue.Milliseconds(), opts.QueueLimit.Milliseconds())
			if queue > opts.QueueLimit {
				return report{}, fmt.Errorf("target %s queue latency %s exceeded %s", repository, queue, opts.QueueLimit)
			}
			return report{
				Repository:  repository,
				RunID:       runID,
				Platform:    opts.Platform,
				CacheMode:   opts.CacheMode,
				QueueMillis: queue.Milliseconds(),
				Conclusion:  run.GetConclusion(),
			}, nil
		}
		if err := sleep(ctx, opts.PollInterval); err != nil {
			return report{}, fmt.Errorf("wait for run %d: %w", runID, err)
		}
	}
}

func queueLatency(createdAt time.Time, jobs []*githubapi.WorkflowJob) (time.Duration, error) {
	var first time.Time
	for _, job := range jobs {
		if job == nil || job.StartedAt == nil {
			continue
		}
		startedAt := job.GetStartedAt().Time
		if first.IsZero() || startedAt.Before(first) {
			first = startedAt
		}
	}
	if createdAt.IsZero() || first.IsZero() {
		return 0, errors.New("workflow run did not expose queue timestamps")
	}
	if first.Before(createdAt) {
		return 0, errors.New("first job started before the workflow run was created")
	}
	return first.Sub(createdAt), nil
}

func waitForCleanup(
	ctx context.Context,
	client *apiClient,
	targets []target,
	prefix string,
	poll time.Duration,
	out io.Writer,
) error {
	for {
		var remaining []string
		for _, target := range targets {
			runners, err := client.runners(ctx, target.Repository)
			if err != nil {
				return err
			}
			for _, runner := range runners {
				if strings.HasPrefix(runner.GetName(), prefix) {
					remaining = append(remaining, target.Repository+"/"+runner.GetName())
				}
			}
		}
		if len(remaining) == 0 {
			fmt.Fprintf(out, "phase=cleanup registrations=0 prefix=%s\n", prefix)
			return nil
		}
		slices.Sort(remaining)
		fmt.Fprintf(out, "phase=cleanup waiting=%s\n", strings.Join(remaining, ","))
		if err := sleep(ctx, poll); err != nil {
			return fmt.Errorf("runner registrations remain for prefix %s: %s", prefix, strings.Join(remaining, ","))
		}
	}
}

func (c *apiClient) runners(ctx context.Context, repository string) ([]*githubapi.Runner, error) {
	owner, repo := splitRepository(repository)
	opts := &githubapi.ListRunnersOptions{ListOptions: githubapi.ListOptions{PerPage: 100}}
	var runners []*githubapi.Runner
	for {
		page, response, err := c.github.Actions.ListRunners(ctx, owner, repo, opts)
		if err != nil {
			return nil, c.apiError(fmt.Sprintf("list runners for %s", repository), err)
		}
		runners = append(runners, page.Runners...)
		if response.NextPage == 0 {
			return runners, nil
		}
		opts.Page = response.NextPage
	}
}

func (c *apiClient) dispatch(ctx context.Context, target target, opts options) (int64, error) {
	body := map[string]any{
		"ref":                target.Ref,
		"return_run_details": true,
		"inputs": map[string]string{
			"platform":           opts.Platform,
			"runner_label":       opts.RunnerLabel,
			"runner_prefix":      opts.RunnerPrefix,
			"fixture_repository": opts.FixtureRepository,
			"fixture_ref":        opts.FixtureRef,
			"cache_namespace":    opts.CacheMode,
		},
	}
	endpoint := repositoryPath(target.Repository, "actions/workflows/"+url.PathEscape(targetWorkflow)+"/dispatches")
	request, err := c.github.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		return 0, c.apiError("build workflow dispatch request", err)
	}
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	var response dispatchResponse
	if _, err := c.github.Do(ctx, request, &response); err != nil {
		return 0, c.apiError(fmt.Sprintf("dispatch %s in %s", targetWorkflow, target.Repository), err)
	}
	if response.WorkflowRunID <= 0 {
		return 0, fmt.Errorf("dispatch %s in %s returned no run ID", targetWorkflow, target.Repository)
	}
	return response.WorkflowRunID, nil
}

func (c *apiClient) workflowRun(
	ctx context.Context,
	repository string,
	runID int64,
) (*githubapi.WorkflowRun, error) {
	owner, repo := splitRepository(repository)
	run, _, err := c.github.Actions.GetWorkflowRunByID(ctx, owner, repo, runID)
	if err != nil {
		return nil, c.apiError(fmt.Sprintf("get run %d in %s", runID, repository), err)
	}
	return run, nil
}

func (c *apiClient) workflowJobs(
	ctx context.Context,
	repository string,
	runID int64,
) ([]*githubapi.WorkflowJob, error) {
	owner, repo := splitRepository(repository)
	opts := &githubapi.ListWorkflowJobsOptions{
		Filter:      "latest",
		ListOptions: githubapi.ListOptions{PerPage: 100},
	}
	var jobs []*githubapi.WorkflowJob
	for {
		page, response, err := c.github.Actions.ListWorkflowJobs(ctx, owner, repo, runID, opts)
		if err != nil {
			return nil, c.apiError(fmt.Sprintf("get jobs for run %d in %s", runID, repository), err)
		}
		jobs = append(jobs, page.Jobs...)
		if response.NextPage == 0 {
			return jobs, nil
		}
		opts.Page = response.NextPage
	}
}

func (c *apiClient) apiError(operation string, err error) error {
	message := err.Error()
	if c.token != "" {
		message = strings.ReplaceAll(message, c.token, "[REDACTED]")
	}
	return fmt.Errorf("%s: %s", operation, message)
}

func splitRepository(repository string) (string, string) {
	parts := strings.SplitN(repository, "/", 2)
	return parts[0], parts[1]
}

func repositoryPath(repository string, suffix string) string {
	owner, repo := splitRepository(repository)
	return "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/" + suffix
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
