// Package github wraps the GitHub REST API calls multirunner needs:
// JIT runner config generation and registration tokens, across repo / org /
// enterprise scopes, authenticated by either a PAT or a GitHub App.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v66/github"

	"github.com/GerardSmit/multirunner/internal/config"
)

// Client talks to GitHub for a single configured scope.
type Client struct {
	gh    *github.Client
	scope config.Scope
	owner string // org name, repo owner, or enterprise slug
	repo  string // only for repo scope
}

// JITConfigRequest is the input for generate-jitconfig.
type JITConfigRequest struct {
	Name          string
	RunnerGroupID int64
	Labels        []string
	WorkFolder    string
}

// JITConfig is the relevant part of the generate-jitconfig response.
type JITConfig struct {
	EncodedJITConfig string `json:"encoded_jit_config"`
	Runner           struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"runner"`
}

// New builds a Client from config, selecting PAT or App auth and honoring a
// GHES base URL when github.url is not github.com.
func New(ctx context.Context, gh config.GitHub, auth config.Auth) (*Client, error) {
	httpClient, err := authHTTPClient(ctx, gh, auth)
	if err != nil {
		return nil, err
	}

	var ghc *github.Client
	if isDotCom(gh.URL) {
		ghc = github.NewClient(httpClient)
	} else {
		// GHES: REST API lives under <url>/api/v3/.
		ghc, err = github.NewClient(httpClient).WithEnterpriseURLs(gh.URL, gh.URL)
		if err != nil {
			return nil, fmt.Errorf("enterprise urls: %w", err)
		}
	}

	return &Client{gh: ghc, scope: gh.Scope, owner: gh.Owner, repo: gh.Repo}, nil
}

func authHTTPClient(ctx context.Context, gh config.GitHub, auth config.Auth) (*http.Client, error) {
	if auth.PAT != "" {
		return &http.Client{
			Timeout:   30 * time.Second,
			Transport: &patTransport{token: auth.PAT, base: http.DefaultTransport},
		}, nil
	}

	apiBase := "https://api.github.com/"
	if !isDotCom(gh.URL) {
		apiBase = strings.TrimRight(gh.URL, "/") + "/api/v3/"
	}
	itr, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, auth.AppID, auth.InstallationID, auth.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	itr.BaseURL = strings.TrimRight(apiBase, "/")
	return &http.Client{Timeout: 30 * time.Second, Transport: itr}, nil
}

// patTransport injects a bearer token on every request.
type patTransport struct {
	token string
	base  http.RoundTripper
}

func (t *patTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// GenerateJITConfig requests a single-use JIT config for the configured scope.
func (c *Client) GenerateJITConfig(ctx context.Context, in JITConfigRequest) (*JITConfig, error) {
	body := map[string]any{
		"name":            in.Name,
		"runner_group_id": in.RunnerGroupID,
		"labels":          in.Labels,
	}
	if in.WorkFolder != "" {
		body["work_folder"] = in.WorkFolder
	}

	path, err := c.runnersPath("generate-jitconfig")
	if err != nil {
		return nil, err
	}
	req, err := c.gh.NewRequest(http.MethodPost, path, body)
	if err != nil {
		return nil, fmt.Errorf("build jitconfig request: %w", err)
	}
	var out JITConfig
	resp, err := c.gh.Do(ctx, req, &out)
	if err != nil {
		return nil, fmt.Errorf("generate-jitconfig (%s): %w", c.scope, err)
	}
	if out.EncodedJITConfig == "" {
		return nil, fmt.Errorf("generate-jitconfig returned empty config (status %d)", resp.StatusCode)
	}
	return &out, nil
}

// CreateRegistrationToken returns a short-lived registration token (config.sh
// fallback path when JIT is unavailable).
func (c *Client) CreateRegistrationToken(ctx context.Context) (string, error) {
	path, err := c.runnersPath("registration-token")
	if err != nil {
		return "", err
	}
	req, err := c.gh.NewRequest(http.MethodPost, path, nil)
	if err != nil {
		return "", fmt.Errorf("build registration-token request: %w", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if _, err := c.gh.Do(ctx, req, &out); err != nil {
		return "", fmt.Errorf("registration-token (%s): %w", c.scope, err)
	}
	return out.Token, nil
}

// ErrRunnerNotFound reports that a runner registration is already gone. GitHub
// removes an ephemeral runner itself once it finishes its single job, so any
// best-effort cleanup races with that and should treat this as success.
var ErrRunnerNotFound = errors.New("runner registration not found")

// DeleteRunner removes a runner registration from GitHub by ID. Ephemeral
// runners self-remove after their one job, so this is the cleanup path for
// runners that exited without consuming their registration. Returns
// ErrRunnerNotFound if GitHub already removed it.
func (c *Client) DeleteRunner(ctx context.Context, runnerID int64) error {
	path, err := c.runnersPath(strconv.FormatInt(runnerID, 10))
	if err != nil {
		return err
	}
	req, err := c.gh.NewRequest(http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("build delete-runner request: %w", err)
	}
	resp, err := c.gh.Do(ctx, req, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("delete-runner %d: %w", runnerID, ErrRunnerNotFound)
		}
		return fmt.Errorf("delete-runner %d (%s): %w", runnerID, c.scope, err)
	}
	return nil
}

// QueuedJobLabels returns the requested labels for queued workflow jobs in repo
// scope. Org/enterprise scope returns nil (no cheap REST endpoint; use webhook
// mode there).
func (c *Client) QueuedJobLabels(ctx context.Context) ([][]string, error) {
	if c.scope != config.ScopeRepo {
		return nil, nil
	}
	path := fmt.Sprintf("repos/%s/%s/actions/runs?status=queued&per_page=20",
		url.PathEscape(c.owner), url.PathEscape(c.repo))
	req, err := c.gh.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		WorkflowRuns []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	if _, err := c.gh.Do(ctx, req, &out); err != nil {
		return nil, fmt.Errorf("list queued runs: %w", err)
	}
	var labels [][]string
	for _, run := range out.WorkflowRuns {
		jobs, err := c.queuedJobsForRun(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		labels = append(labels, jobs...)
	}
	return labels, nil
}

func (c *Client) queuedJobsForRun(ctx context.Context, runID int64) ([][]string, error) {
	path := fmt.Sprintf("repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=100",
		url.PathEscape(c.owner), url.PathEscape(c.repo), runID)
	req, err := c.gh.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Jobs []struct {
			Status string   `json:"status"`
			Labels []string `json:"labels"`
		} `json:"jobs"`
	}
	if _, err := c.gh.Do(ctx, req, &out); err != nil {
		return nil, fmt.Errorf("list workflow jobs: %w", err)
	}
	labels := make([][]string, 0, len(out.Jobs))
	for _, job := range out.Jobs {
		if job.Status == "queued" {
			labels = append(labels, job.Labels)
		}
	}
	return labels, nil
}

// Scope reports the configured scope.
func (c *Client) Scope() config.Scope { return c.scope }

// ActionsEnabled reports whether GitHub Actions is enabled on the repo. A repo
// with Actions switched off accepts runner registrations and reports no queued
// jobs, exactly like an idle repo, so it is otherwise indistinguishable from one
// that simply has no work. Requires a repo-scoped client.
func (c *Client) ActionsEnabled(ctx context.Context) (bool, error) {
	if c.repo == "" {
		return false, fmt.Errorf("ActionsEnabled requires a repo-scoped client")
	}
	perms, _, err := c.gh.Repositories.GetActionsPermissions(ctx, c.owner, c.repo)
	if err != nil {
		return false, fmt.Errorf("get actions permissions %s/%s: %w", c.owner, c.repo, err)
	}
	return perms.GetEnabled(), nil
}

// RepoFilePaths returns every blob path in the repo's default-branch tree. Used
// by `multirunner detect --repo` to find language markers without a checkout.
// Requires the client to be repo-scoped (owner + repo set).
func (c *Client) RepoFilePaths(ctx context.Context) ([]string, error) {
	if c.repo == "" {
		return nil, fmt.Errorf("RepoFilePaths requires a repo-scoped client")
	}
	repo, _, err := c.gh.Repositories.Get(ctx, c.owner, c.repo)
	if err != nil {
		return nil, fmt.Errorf("get repo %s/%s: %w", c.owner, c.repo, err)
	}
	tree, _, err := c.gh.Git.GetTree(ctx, c.owner, c.repo, repo.GetDefaultBranch(), true)
	if err != nil {
		return nil, fmt.Errorf("get tree (%s): %w", repo.GetDefaultBranch(), err)
	}
	var out []string
	for _, e := range tree.Entries {
		if e.GetType() == "blob" {
			out = append(out, e.GetPath())
		}
	}
	return out, nil
}

// RepoFile returns the contents of a repo-relative path on the default branch.
func (c *Client) RepoFile(ctx context.Context, p string) ([]byte, error) {
	fc, _, _, err := c.gh.Repositories.GetContents(ctx, c.owner, c.repo, p, nil)
	if err != nil {
		return nil, fmt.Errorf("get contents %s: %w", p, err)
	}
	if fc == nil {
		return nil, fmt.Errorf("%s is not a file", p)
	}
	s, err := fc.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return []byte(s), nil
}

// runnersPath builds the actions/runners sub-path for the configured scope.
func (c *Client) runnersPath(action string) (string, error) {
	switch c.scope {
	case config.ScopeRepo:
		return fmt.Sprintf("repos/%s/%s/actions/runners/%s",
			url.PathEscape(c.owner), url.PathEscape(c.repo), action), nil
	case config.ScopeOrg:
		return fmt.Sprintf("orgs/%s/actions/runners/%s", url.PathEscape(c.owner), action), nil
	case config.ScopeEnterprise:
		return fmt.Sprintf("enterprises/%s/actions/runners/%s", url.PathEscape(c.owner), action), nil
	default:
		return "", fmt.Errorf("unsupported scope %q", c.scope)
	}
}

func isDotCom(rawURL string) bool {
	if rawURL == "" {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	return host == "github.com" || host == "www.github.com" || host == ""
}
