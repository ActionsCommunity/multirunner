package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

type apiClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

type runnerList struct {
	Runners []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"runners"`
}

type dispatchResponse struct {
	WorkflowRunID int64  `json:"workflow_run_id"`
	HTMLURL       string `json:"html_url"`
}

type workflowRun struct {
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
	HTMLURL    string    `json:"html_url"`
}

type workflowJobs struct {
	Jobs []struct {
		Name      string     `json:"name"`
		Status    string     `json:"status"`
		StartedAt *time.Time `json:"started_at"`
	} `json:"jobs"`
}

func execute(ctx context.Context, client *apiClient, opts options, out io.Writer) ([]report, error) {
	type dispatchedRun struct {
		target target
		run    dispatchResponse
	}
	dispatched := make([]dispatchedRun, 0, len(opts.Targets))
	for _, target := range opts.Targets {
		run, err := client.dispatch(ctx, target, opts)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "target=%s phase=dispatched run_id=%d url=%s\n", target.Repository, run.WorkflowRunID, run.HTMLURL)
		dispatched = append(dispatched, dispatchedRun{target: target, run: run})
	}
	if err := waitForRunners(ctx, client, opts.Targets, opts.RunnerPrefix, opts.RunnerLabel, opts.PollInterval, out); err != nil {
		return nil, err
	}

	reports := make([]report, 0, len(dispatched))
	for _, item := range dispatched {
		result, err := waitForRun(ctx, client, item.target.Repository, item.run, opts, out)
		if err != nil {
			return nil, err
		}
		reports = append(reports, result)
	}
	return reports, nil
}

func waitForRunners(
	ctx context.Context,
	client *apiClient,
	targets []target,
	prefix string,
	label string,
	poll time.Duration,
	out io.Writer,
) error {
	for {
		pending := make([]string, 0, len(targets))
		for _, target := range targets {
			runners, err := client.runners(ctx, target.Repository)
			if err != nil {
				return err
			}
			if !hasOnlineRunner(runners, prefix, label) {
				pending = append(pending, target.Repository)
			}
		}
		if len(pending) == 0 {
			fmt.Fprintf(out, "phase=provisioned targets=%d label=%s\n", len(targets), label)
			return nil
		}
		fmt.Fprintf(out, "phase=provisioning pending=%s\n", strings.Join(pending, ","))
		if err := sleep(ctx, poll); err != nil {
			return fmt.Errorf("wait for runners: %w", err)
		}
	}
}

func hasOnlineRunner(runners runnerList, prefix string, label string) bool {
	for _, runner := range runners.Runners {
		if runner.Status != "online" || !strings.HasPrefix(runner.Name, prefix) {
			continue
		}
		for _, runnerLabel := range runner.Labels {
			if strings.EqualFold(runnerLabel.Name, label) {
				return true
			}
		}
	}
	return false
}

func waitForRun(
	ctx context.Context,
	client *apiClient,
	repository string,
	dispatched dispatchResponse,
	opts options,
	out io.Writer,
) (report, error) {
	for {
		run, err := client.workflowRun(ctx, repository, dispatched.WorkflowRunID)
		if err != nil {
			return report{}, err
		}
		fmt.Fprintf(out, "target=%s run_id=%d phase=workload status=%s conclusion=%s\n",
			repository, dispatched.WorkflowRunID, run.Status, run.Conclusion)
		if run.Status == "completed" {
			if run.Conclusion != "success" {
				return report{}, fmt.Errorf("target %s run %d concluded %s", repository, dispatched.WorkflowRunID, run.Conclusion)
			}
			jobs, err := client.workflowJobs(ctx, repository, dispatched.WorkflowRunID)
			if err != nil {
				return report{}, err
			}
			queue, err := queueLatency(run.CreatedAt, jobs)
			if err != nil {
				return report{}, fmt.Errorf("target %s run %d: %w", repository, dispatched.WorkflowRunID, err)
			}
			fmt.Fprintf(out, "target=%s run_id=%d queue_to_start_ms=%d limit_ms=%d\n",
				repository, dispatched.WorkflowRunID, queue.Milliseconds(), opts.QueueLimit.Milliseconds())
			if queue > opts.QueueLimit {
				return report{}, fmt.Errorf("target %s queue latency %s exceeded %s", repository, queue, opts.QueueLimit)
			}
			runURL := run.HTMLURL
			if runURL == "" {
				runURL = dispatched.HTMLURL
			}
			return report{
				Repository:  repository,
				RunID:       dispatched.WorkflowRunID,
				RunURL:      runURL,
				Platform:    opts.Platform,
				CacheMode:   opts.CacheMode,
				QueueMillis: queue.Milliseconds(),
				Conclusion:  run.Conclusion,
			}, nil
		}
		if err := sleep(ctx, opts.PollInterval); err != nil {
			return report{}, fmt.Errorf("wait for run %d: %w", dispatched.WorkflowRunID, err)
		}
	}
}

func queueLatency(createdAt time.Time, jobs workflowJobs) (time.Duration, error) {
	var first time.Time
	for _, job := range jobs.Jobs {
		if job.StartedAt != nil && (first.IsZero() || job.StartedAt.Before(first)) {
			first = *job.StartedAt
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
			for _, runner := range runners.Runners {
				if strings.HasPrefix(runner.Name, prefix) {
					remaining = append(remaining, target.Repository+"/"+runner.Name)
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

func (c *apiClient) runners(ctx context.Context, repository string) (runnerList, error) {
	var response runnerList
	err := c.request(ctx, http.MethodGet, repositoryPath(repository, "actions/runners?per_page=100"), nil, &response)
	if err != nil {
		return runnerList{}, fmt.Errorf("list runners for %s: %w", repository, err)
	}
	return response, nil
}

func (c *apiClient) dispatch(ctx context.Context, target target, opts options) (dispatchResponse, error) {
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
	var response dispatchResponse
	endpoint := repositoryPath(target.Repository, "actions/workflows/"+url.PathEscape(opts.Workflow)+"/dispatches")
	if err := c.request(ctx, http.MethodPost, endpoint, body, &response); err != nil {
		return dispatchResponse{}, fmt.Errorf("dispatch %s in %s: %w", opts.Workflow, target.Repository, err)
	}
	if response.WorkflowRunID == 0 {
		return dispatchResponse{}, fmt.Errorf("dispatch %s in %s returned no run ID", opts.Workflow, target.Repository)
	}
	return response, nil
}

func (c *apiClient) workflowRun(ctx context.Context, repository string, runID int64) (workflowRun, error) {
	var response workflowRun
	endpoint := repositoryPath(repository, fmt.Sprintf("actions/runs/%d", runID))
	if err := c.request(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return workflowRun{}, fmt.Errorf("get run %d in %s: %w", runID, repository, err)
	}
	return response, nil
}

func (c *apiClient) workflowJobs(ctx context.Context, repository string, runID int64) (workflowJobs, error) {
	var response workflowJobs
	endpoint := repositoryPath(repository, fmt.Sprintf("actions/runs/%d/jobs?filter=latest&per_page=100", runID))
	if err := c.request(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return workflowJobs{}, fmt.Errorf("get jobs for run %d in %s: %w", runID, repository, err)
	}
	return response, nil
}

func (c *apiClient) request(ctx context.Context, method string, endpoint string, body any, response any) error {
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/"+endpoint, requestBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("GitHub API returned %s: %s", res.Status, strings.TrimSpace(string(message)))
	}
	if response == nil {
		_, err = io.Copy(io.Discard, res.Body)
		return err
	}
	if err := json.NewDecoder(res.Body).Decode(response); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func repositoryPath(repository string, suffix string) string {
	parts := strings.Split(repository, "/")
	return path.Join("repos", url.PathEscape(parts[0]), url.PathEscape(parts[1])) + "/" + suffix
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
