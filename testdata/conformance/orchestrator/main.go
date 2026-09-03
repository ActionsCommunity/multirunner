package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	apiVersion        = "2026-03-10"
	trustedRepository = "ActionsCommunity/multirunner"
	targetWorkflow    = "e2e-target.yml"
	workflowName      = "e2e-linux"
	workflowPath      = ".github/workflows/e2e-linux.yml"
	maxTargets        = 20
	maxQueueLimit     = 30 * time.Minute
	maxWorkflowInput  = 255
	runTimeout        = 30 * time.Minute
	pollInterval      = 5 * time.Second
)

var (
	repositoryPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	labelPattern          = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	refPattern            = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	shaPattern            = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	windowsPipePattern    = regexp.MustCompile(`^npipe:////\./pipe/[A-Za-z0-9_.-]+$`)
)

type target struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
}

type options struct {
	Targets           []target
	RunnerLabel       string
	RunnerPrefix      string
	FixtureRepository string
	FixtureRef        string
	Platform          string
	CacheMode         string
	QueueLimit        time.Duration
	Timeout           time.Duration
	PollInterval      time.Duration
	ReportPath        string
	ServerURL         string
	APIURL            string
	RepositoryOwner   string
	Repository        string
	Ref               string
	Workflow          string
	WorkflowRef       string
	EventName         string
	TrustedRef        string
	RunID             int64
	Backend           string
	DockerHost        string
	Image             string
}

type report struct {
	Repository  string `json:"repository"`
	RunID       int64  `json:"run_id"`
	Platform    string `json:"platform"`
	CacheMode   string `json:"cache_mode"`
	QueueMillis int64  `json:"queue_millis"`
	Conclusion  string `json:"conclusion"`
}

func main() {
	if err := runCLI(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "conformance orchestration failed: %v\n", err)
		os.Exit(1)
	}
}

func runCLI(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("expected exactly one validate, run, or cleanup command")
	}
	opts, err := loadOptions(os.Getenv)
	if err != nil {
		return err
	}

	switch args[0] {
	case "validate":
		return nil
	case "run":
		client, err := authenticatedAPIClient(opts)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
		defer cancel()
		reports, err := execute(ctx, client, opts, out)
		if err != nil {
			return err
		}
		return writeReport(opts.ReportPath, reports)
	case "cleanup":
		client, err := authenticatedAPIClient(opts)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return waitForCleanup(ctx, client, opts.Targets, opts.RunnerPrefix, opts.PollInterval, out)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func authenticatedAPIClient(opts options) (*apiClient, error) {
	token := os.Getenv("MR_CONFORMANCE_PAT")
	if token == "" {
		return nil, errors.New("MR_CONFORMANCE_PAT is required")
	}
	return newAPIClient(token, opts.ServerURL, opts.APIURL)
}

func loadOptions(getenv func(string) string) (options, error) {
	targets, err := parseTargets(getenv("MR_TARGETS"))
	if err != nil {
		return options{}, err
	}
	queueLimit, err := time.ParseDuration(getenv("MR_QUEUE_LIMIT"))
	if err != nil {
		return options{}, fmt.Errorf("invalid MR_QUEUE_LIMIT: %w", err)
	}
	runID, err := strconv.ParseInt(getenv("GITHUB_RUN_ID"), 10, 64)
	if err != nil || runID <= 0 {
		return options{}, errors.New("GITHUB_RUN_ID must be a positive integer")
	}
	opts := options{
		Targets:           targets,
		RunnerLabel:       getenv("MR_RUNNER_LABEL"),
		RunnerPrefix:      getenv("MR_RUNNER_PREFIX"),
		FixtureRepository: getenv("GITHUB_REPOSITORY"),
		FixtureRef:        getenv("GITHUB_SHA"),
		Platform:          getenv("MR_PLATFORM"),
		CacheMode:         getenv("MR_CACHE_MODE"),
		QueueLimit:        queueLimit,
		Timeout:           runTimeout,
		PollInterval:      pollInterval,
		ServerURL:         getenv("GITHUB_SERVER_URL"),
		APIURL:            getenv("GITHUB_API_URL"),
		RepositoryOwner:   getenv("GITHUB_REPOSITORY_OWNER"),
		Repository:        getenv("GITHUB_REPOSITORY"),
		Ref:               getenv("GITHUB_REF"),
		Workflow:          getenv("GITHUB_WORKFLOW"),
		WorkflowRef:       getenv("GITHUB_WORKFLOW_REF"),
		EventName:         getenv("GITHUB_EVENT_NAME"),
		TrustedRef:        getenv("MR_TRUSTED_REF"),
		RunID:             runID,
		Backend:           getenv("MR_BACKEND"),
		DockerHost:        getenv("MR_DOCKER_HOST"),
		Image:             getenv("MR_IMAGE"),
	}
	opts.ReportPath = fmt.Sprintf("runner-conformance-%s-%s.json", opts.Platform, opts.CacheMode)
	if err := validateOptions(opts); err != nil {
		return options{}, err
	}
	return opts, nil
}

func parseTargets(raw string) ([]target, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var targets []target
	if err := decoder.Decode(&targets); err != nil {
		return nil, fmt.Errorf("decode MR_TARGETS: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("MR_TARGETS must contain one JSON value")
	}
	if len(targets) < 2 || len(targets) > maxTargets {
		return nil, fmt.Errorf("MR_TARGETS must contain between 2 and %d repositories", maxTargets)
	}
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if !validRepository(target.Repository) {
			return nil, fmt.Errorf("invalid target repository %q", target.Repository)
		}
		if !validRef(target.Ref) {
			return nil, fmt.Errorf("invalid target ref %q", target.Ref)
		}
		key := strings.ToLower(target.Repository)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate target repository %q", target.Repository)
		}
		seen[key] = struct{}{}
	}
	return targets, nil
}

func validRef(value string) bool {
	return value != "" &&
		len(value) <= maxWorkflowInput &&
		refPattern.MatchString(value) &&
		!strings.Contains(value, "..") &&
		!strings.Contains(value, "//") &&
		!strings.HasPrefix(value, "/") &&
		!strings.HasSuffix(value, "/") &&
		!strings.HasSuffix(value, ".")
}

func validRepository(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 &&
		validRepositoryPart(parts[0], 39) &&
		validRepositoryPart(parts[1], 100)
}

func validRepositoryPart(value string, maxLength int) bool {
	return value != "" &&
		len(value) <= maxLength &&
		value != "." &&
		value != ".." &&
		repositoryPartPattern.MatchString(value)
}

func validLabel(value string) bool {
	return value != "" && len(value) <= maxWorkflowInput && labelPattern.MatchString(value)
}

func validateOptions(opts options) error {
	if !validLabel(opts.RunnerLabel) || !validLabel(opts.RunnerPrefix) {
		return errors.New("runner label and prefix must use letters, digits, dots, underscores, or hyphens")
	}
	if !validRepository(opts.FixtureRepository) {
		return fmt.Errorf("invalid GITHUB_REPOSITORY %q", opts.FixtureRepository)
	}
	if !validRef(opts.FixtureRef) {
		return fmt.Errorf("invalid GITHUB_SHA %q", opts.FixtureRef)
	}
	if opts.Platform != "linux" && opts.Platform != "windows" {
		return fmt.Errorf("invalid MR_PLATFORM %q", opts.Platform)
	}
	if opts.CacheMode != "enabled" && opts.CacheMode != "disabled" {
		return fmt.Errorf("invalid MR_CACHE_MODE %q", opts.CacheMode)
	}
	if opts.QueueLimit <= 0 || opts.QueueLimit > maxQueueLimit {
		return errors.New("MR_QUEUE_LIMIT is outside the supported range")
	}
	if _, err := validatedAPIBase(opts.ServerURL, opts.APIURL); err != nil {
		return err
	}
	if !repositoryPartPattern.MatchString(opts.RepositoryOwner) {
		return errors.New("invalid GITHUB_REPOSITORY_OWNER")
	}
	if opts.Repository != trustedRepository ||
		!validRepository(opts.Repository) ||
		!strings.EqualFold(strings.SplitN(opts.Repository, "/", 2)[0], opts.RepositoryOwner) {
		return errors.New("GITHUB_REPOSITORY is not the trusted repository or owner")
	}
	if opts.Repository != opts.FixtureRepository {
		return errors.New("fixture repository must be the trusted workflow repository")
	}
	if !strings.HasPrefix(opts.TrustedRef, "refs/heads/") ||
		!validRef(strings.TrimPrefix(opts.TrustedRef, "refs/heads/")) ||
		opts.Ref != opts.TrustedRef {
		return errors.New("GITHUB_REF is not the configured trusted branch")
	}
	if !shaPattern.MatchString(opts.FixtureRef) {
		return errors.New("GITHUB_SHA must be a 40 or 64 character hexadecimal commit identifier")
	}
	if opts.Workflow != workflowName {
		return fmt.Errorf("unexpected GITHUB_WORKFLOW %q", opts.Workflow)
	}
	expectedWorkflowRef := opts.Repository + "/" + workflowPath + "@" + opts.Ref
	if opts.WorkflowRef != expectedWorkflowRef {
		return errors.New("GITHUB_WORKFLOW_REF does not identify the trusted workflow and ref")
	}
	if opts.EventName != "schedule" && opts.EventName != "workflow_dispatch" {
		return fmt.Errorf("untrusted GITHUB_EVENT_NAME %q", opts.EventName)
	}
	if opts.Platform == "linux" {
		if opts.Backend != "docker" ||
			opts.DockerHost != "unix:///var/run/docker.sock" ||
			opts.Image != "gerardsmit/multirunner-runner-linux:node" {
			return errors.New("Linux backend, host, or image is outside the conformance allowlist")
		}
	} else if opts.Backend != "containerd" ||
		!windowsPipePattern.MatchString(opts.DockerHost) ||
		opts.Image != "gerardsmit/multirunner-runner-windows:dotnet" {
		return errors.New("Windows backend, host, or image is outside the conformance allowlist")
	}
	return nil
}

func writeReport(reportPath string, reports []report) error {
	encoded, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(reportPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
