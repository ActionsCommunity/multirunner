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
	"github.com/GerardSmit/multirunner/internal/ghapp"
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
	origin, err := apiOrigin(gh.URL)
	if err != nil {
		return nil, err
	}

	if auth.PAT != "" {
		return &http.Client{
			Timeout:   30 * time.Second,
			Transport: &patTransport{token: auth.PAT, base: http.DefaultTransport, origin: origin},
		}, nil
	}

	if auth.IsDeviceApp() {
		tr, err := newDeviceTransport(gh, auth, origin)
		if err != nil {
			return nil, err
		}
		return &http.Client{Timeout: 30 * time.Second, Transport: tr}, nil
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
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &originTransport{base: itr, origin: origin},
	}, nil
}

// originTransport stops a wrapped transport that adds credentials of its own -
// ghinstallation mints and attaches an installation token per request - from
// sending them anywhere but the configured API origin.
type originTransport struct {
	base   http.RoundTripper
	origin string
}

func (t *originTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if sameOrigin(req, t.origin) {
		return t.base.RoundTrip(req)
	}
	r := req.Clone(req.Context())
	r.Header.Del("Authorization")
	return http.DefaultTransport.RoundTrip(r)
}

// apiOrigin returns the single scheme+host that may receive our credentials.
// Go's http.Client drops the Authorization header when a redirect crosses
// origins, but a RoundTripper runs per hop and would put it straight back, so
// each transport - including the wrapper around ghinstallation, which mints an
// installation token per request - checks the destination itself.
func apiOrigin(ghURL string) (string, error) {
	if isDotCom(ghURL) {
		return "https://api.github.com", nil
	}
	u, err := url.Parse(strings.TrimRight(ghURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("github.url %q is not a valid URL", ghURL)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// sameOrigin reports whether req is going to the origin our credentials belong
// to. Anything else — a redirect to a raw-content host, an attacker-controlled
// Location — must be sent unauthenticated.
func sameOrigin(req *http.Request, origin string) bool {
	return strings.EqualFold(req.URL.Scheme+"://"+req.URL.Host, origin)
}

// patTransport injects a bearer token on requests to the configured API origin.
type patTransport struct {
	token  string
	base   http.RoundTripper
	origin string
}

func (t *patTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if !sameOrigin(req, t.origin) {
		r.Header.Del("Authorization")
		return t.base.RoundTrip(r)
	}
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// deviceTransport authenticates requests with a GitHub App device-flow user
// access token, refreshing it (via the shared ghapp.TokenRefresher) when expired
// and persisting the rotated token to its sidecar.
type deviceTransport struct {
	base      http.RoundTripper
	refresher *ghapp.TokenRefresher
	origin    string
}

func newDeviceTransport(gh config.GitHub, auth config.Auth, origin string) (*deviceTransport, error) {
	refresher, err := ghapp.NewTokenRefresher(auth.ClientID, gh.URL, auth.TokenPath)
	if err != nil {
		return nil, err
	}
	return &deviceTransport{base: http.DefaultTransport, refresher: refresher, origin: origin}, nil
}

func (t *deviceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if !sameOrigin(req, t.origin) {
		r.Header.Del("Authorization")
		return t.base.RoundTrip(r)
	}
	access, err := t.refresher.AccessToken(req.Context())
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+access)
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

// RunnerBusy reports whether GitHub currently shows the runner as running a
// job. Returns ErrRunnerNotFound when the registration is already gone.
func (c *Client) RunnerBusy(ctx context.Context, runnerID int64) (bool, error) {
	path, err := c.runnersPath(strconv.FormatInt(runnerID, 10))
	if err != nil {
		return false, err
	}
	req, err := c.gh.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return false, fmt.Errorf("build get-runner request: %w", err)
	}
	var out github.Runner
	resp, err := c.gh.Do(ctx, req, &out)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, fmt.Errorf("get-runner %d: %w", runnerID, ErrRunnerNotFound)
		}
		return false, fmt.Errorf("get-runner %d (%s): %w", runnerID, c.scope, err)
	}
	return out.GetBusy(), nil
}

// QueuedJobLabels returns the requested labels for queued workflow jobs in repo
// scope. Org/enterprise scope returns nil (no cheap REST endpoint; use webhook
// mode there).
func (c *Client) QueuedJobLabels(ctx context.Context) ([][]string, error) {
	if c.scope != config.ScopeRepo {
		return nil, nil
	}
	runIDs := make([]int64, 0)
	seen := make(map[int64]struct{})
	for _, status := range []string{"queued", "in_progress"} {
		opts := &github.ListWorkflowRunsOptions{
			Status:      status,
			ListOptions: github.ListOptions{PerPage: 100},
		}
		for {
			runs, resp, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, c.owner, c.repo, opts)
			if err != nil {
				return nil, fmt.Errorf("list %s workflow runs: %w", status, err)
			}
			for _, run := range runs.WorkflowRuns {
				id := run.GetID()
				if _, ok := seen[id]; !ok {
					seen[id] = struct{}{}
					runIDs = append(runIDs, id)
				}
			}
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}
	var labels [][]string
	for _, runID := range runIDs {
		jobs, err := c.queuedJobsForRun(ctx, runID)
		if err != nil {
			return nil, err
		}
		labels = append(labels, jobs...)
	}
	return labels, nil
}

func (c *Client) queuedJobsForRun(ctx context.Context, runID int64) ([][]string, error) {
	opts := &github.ListWorkflowJobsOptions{
		Filter:      "latest",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var labels [][]string
	for {
		jobs, resp, err := c.gh.Actions.ListWorkflowJobs(ctx, c.owner, c.repo, runID, opts)
		if err != nil {
			return nil, fmt.Errorf("list workflow jobs for run %d: %w", runID, err)
		}
		for _, job := range jobs.Jobs {
			if job.GetStatus() == "queued" {
				labels = append(labels, job.Labels)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return labels, nil
}

// CheckRunnerAccess lists one runner to prove the configured credential can
// reach the runner-admin API for this scope. Registration is the first thing
// that fails when the token is wrong, and it fails on the runner host rather
// than on the operator's terminal, so proving reachability up front turns a
// silent misconfiguration into an immediate error. 401/403 and 404 have
// different fixes (credential/scope versus owner or enterprise slug), so they
// are reported apart.
func (c *Client) CheckRunnerAccess(ctx context.Context) error {
	path, err := c.runnersBasePath()
	if err != nil {
		return err
	}
	req, err := c.gh.NewRequest(http.MethodGet, path+"?per_page=1", nil)
	if err != nil {
		return fmt.Errorf("build list-runners request: %w", err)
	}
	resp, err := c.gh.Do(ctx, req, nil)
	if err == nil {
		return nil
	}
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("credentials cannot manage %s runners for %q: %w", c.scope, c.owner, err)
		case http.StatusNotFound:
			return fmt.Errorf("no %s %q visible to these credentials: %w", c.scope, c.owner, err)
		}
	}
	return fmt.Errorf("list-runners (%s): %w", c.scope, err)
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
	if tree.GetTruncated() {
		return nil, fmt.Errorf("get tree (%s): recursive result was truncated", repo.GetDefaultBranch())
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

// runnersBasePath builds the actions/runners collection path for the configured
// scope. runnersPath appends an action to it; callers that need the collection
// itself must use this, since a trailing slash is a different endpoint.
func (c *Client) runnersBasePath() (string, error) {
	switch c.scope {
	case config.ScopeRepo:
		return fmt.Sprintf("repos/%s/%s/actions/runners",
			url.PathEscape(c.owner), url.PathEscape(c.repo)), nil
	case config.ScopeOrg:
		return fmt.Sprintf("orgs/%s/actions/runners", url.PathEscape(c.owner)), nil
	case config.ScopeEnterprise:
		return fmt.Sprintf("enterprises/%s/actions/runners", url.PathEscape(c.owner)), nil
	default:
		return "", fmt.Errorf("unsupported scope %q", c.scope)
	}
}

// runnersPath builds the actions/runners sub-path for the configured scope.
func (c *Client) runnersPath(action string) (string, error) {
	base, err := c.runnersBasePath()
	if err != nil {
		return "", err
	}
	return base + "/" + action, nil
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
