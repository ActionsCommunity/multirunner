package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const apiVersion = "2026-03-10"

var (
	repositoryPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	workflowPattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.ya?ml$`)
	labelPattern          = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	refPattern            = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

type target struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
}

type options struct {
	Targets           []target
	Workflow          string
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
}

type report struct {
	Repository  string `json:"repository"`
	RunID       int64  `json:"run_id"`
	RunURL      string `json:"run_url"`
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
	if len(args) == 0 {
		return errors.New("expected run or cleanup command")
	}
	token := os.Getenv("MR_CONFORMANCE_PAT")
	if token == "" {
		return errors.New("MR_CONFORMANCE_PAT is required")
	}
	baseURL := os.Getenv("GITHUB_API_URL")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	client := &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	switch args[0] {
	case "run":
		opts, err := parseRunOptions(args[1:])
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
		targets, prefix, timeout, poll, err := parseCleanupOptions(args[1:])
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return waitForCleanup(ctx, client, targets, prefix, poll, out)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func parseRunOptions(args []string) (options, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var targetsJSON string
	var opts options
	fs.StringVar(&targetsJSON, "targets-json", "", "JSON target array")
	fs.StringVar(&opts.Workflow, "workflow", "runner-conformance-dispatch.yml", "target workflow file")
	fs.StringVar(&opts.RunnerLabel, "runner-label", "", "unique runner label")
	fs.StringVar(&opts.RunnerPrefix, "runner-prefix", "", "owned runner name prefix")
	fs.StringVar(&opts.FixtureRepository, "fixture-repository", "", "repository containing fixtures")
	fs.StringVar(&opts.FixtureRef, "fixture-ref", "", "fixture repository ref")
	fs.StringVar(&opts.Platform, "platform", "", "linux or windows")
	fs.StringVar(&opts.CacheMode, "cache-mode", "", "enabled or disabled")
	fs.DurationVar(&opts.QueueLimit, "queue-limit", 3*time.Minute, "maximum queue latency")
	fs.DurationVar(&opts.Timeout, "timeout", 30*time.Minute, "overall timeout")
	fs.DurationVar(&opts.PollInterval, "poll", 5*time.Second, "poll interval")
	fs.StringVar(&opts.ReportPath, "report", "runner-conformance-report.json", "report path")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	targets, err := parseTargets(targetsJSON)
	if err != nil {
		return options{}, err
	}
	opts.Targets = targets
	if err := validateOptions(opts); err != nil {
		return options{}, err
	}
	return opts, nil
}

func parseCleanupOptions(args []string) ([]target, string, time.Duration, time.Duration, error) {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	var targetsJSON, prefix string
	var timeout, poll time.Duration
	fs.StringVar(&targetsJSON, "targets-json", "", "JSON target array")
	fs.StringVar(&prefix, "runner-prefix", "", "owned runner name prefix")
	fs.DurationVar(&timeout, "timeout", 2*time.Minute, "cleanup timeout")
	fs.DurationVar(&poll, "poll", 5*time.Second, "poll interval")
	if err := fs.Parse(args); err != nil {
		return nil, "", 0, 0, err
	}
	targets, err := parseTargets(targetsJSON)
	if err != nil {
		return nil, "", 0, 0, err
	}
	if !labelPattern.MatchString(prefix) {
		return nil, "", 0, 0, fmt.Errorf("invalid runner prefix %q", prefix)
	}
	if timeout <= 0 || poll <= 0 {
		return nil, "", 0, 0, errors.New("timeout and poll must be positive")
	}
	return targets, prefix, timeout, poll, nil
}

func parseTargets(raw string) ([]target, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var targets []target
	if err := decoder.Decode(&targets); err != nil {
		return nil, fmt.Errorf("decode targets: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("targets JSON must contain one value")
	}
	if len(targets) < 2 {
		return nil, errors.New("at least two conformance targets are required")
	}
	seen := make(map[string]struct{}, len(targets))
	for i := range targets {
		parts := strings.Split(targets[i].Repository, "/")
		if len(parts) != 2 || !repositoryPartPattern.MatchString(parts[0]) || !repositoryPartPattern.MatchString(parts[1]) {
			return nil, fmt.Errorf("invalid target repository %q", targets[i].Repository)
		}
		if !validRef(targets[i].Ref) {
			return nil, fmt.Errorf("invalid target ref %q", targets[i].Ref)
		}
		key := strings.ToLower(targets[i].Repository)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate target repository %q", targets[i].Repository)
		}
		seen[key] = struct{}{}
	}
	return targets, nil
}

func validRef(value string) bool {
	return value != "" &&
		len(value) <= 255 &&
		refPattern.MatchString(value) &&
		!strings.Contains(value, "..") &&
		!strings.Contains(value, "//") &&
		!strings.HasPrefix(value, "/") &&
		!strings.HasSuffix(value, "/") &&
		!strings.HasSuffix(value, ".")
}

func validateOptions(opts options) error {
	if !workflowPattern.MatchString(opts.Workflow) {
		return fmt.Errorf("invalid workflow file %q", opts.Workflow)
	}
	if !labelPattern.MatchString(opts.RunnerLabel) || !labelPattern.MatchString(opts.RunnerPrefix) {
		return errors.New("runner label and prefix must use letters, digits, dots, underscores, or hyphens")
	}
	parts := strings.Split(opts.FixtureRepository, "/")
	if len(parts) != 2 || !repositoryPartPattern.MatchString(parts[0]) || !repositoryPartPattern.MatchString(parts[1]) {
		return fmt.Errorf("invalid fixture repository %q", opts.FixtureRepository)
	}
	if !validRef(opts.FixtureRef) {
		return fmt.Errorf("invalid fixture ref %q", opts.FixtureRef)
	}
	if opts.Platform != "linux" && opts.Platform != "windows" {
		return fmt.Errorf("invalid platform %q", opts.Platform)
	}
	if opts.CacheMode != "enabled" && opts.CacheMode != "disabled" {
		return fmt.Errorf("invalid cache mode %q", opts.CacheMode)
	}
	if opts.QueueLimit <= 0 || opts.Timeout <= 0 || opts.PollInterval <= 0 {
		return errors.New("queue limit, timeout, and poll must be positive")
	}
	if opts.ReportPath == "" {
		return errors.New("report path is required")
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
