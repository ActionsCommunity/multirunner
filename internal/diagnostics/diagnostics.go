// Package diagnostics runs isolated, structured operational checks.
package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Observation struct {
	Status      Status         `json:"status"`
	Observed    string         `json:"observed"`
	Expected    string         `json:"expected"`
	Remediation string         `json:"remediation,omitempty"`
	Evidence    map[string]any `json:"evidence,omitempty"`
}

type Check struct {
	ID        string
	Version   int
	Category  string
	Severity  string
	DocsRoute string
	Timeout   time.Duration
	Run       func(context.Context) (Observation, error)
}

type Result struct {
	ID          string         `json:"id"`
	Version     int            `json:"version"`
	Category    string         `json:"category"`
	Severity    string         `json:"severity"`
	Status      Status         `json:"status"`
	ObservedAt  time.Time      `json:"observed_at"`
	DurationMS  int64          `json:"duration_ms"`
	Observed    string         `json:"observed"`
	Expected    string         `json:"expected"`
	Remediation string         `json:"remediation,omitempty"`
	DocsRoute   string         `json:"docs_route,omitempty"`
	Evidence    map[string]any `json:"evidence,omitempty"`
	Error       string         `json:"error,omitempty"`
}

type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Status      Status    `json:"status"`
	Results     []Result  `json:"results"`
}

type Service struct {
	mu     sync.RWMutex
	checks []Check
	now    func() time.Time
}

func NewService() *Service {
	return &Service{now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) SetChecks(checks []Check) error {
	seen := make(map[string]struct{}, len(checks))
	for index := range checks {
		check := checks[index]
		if strings.TrimSpace(check.ID) == "" || check.Version < 1 ||
			strings.TrimSpace(check.Category) == "" || check.Run == nil {
			return fmt.Errorf("diagnostic check %d is incomplete", index)
		}
		if _, exists := seen[check.ID]; exists {
			return fmt.Errorf("diagnostic check ID %q is duplicated", check.ID)
		}
		seen[check.ID] = struct{}{}
	}
	s.mu.Lock()
	s.checks = append([]Check(nil), checks...)
	s.mu.Unlock()
	return nil
}

func (s *Service) Run(ctx context.Context) Report {
	s.mu.RLock()
	checks := append([]Check(nil), s.checks...)
	s.mu.RUnlock()
	results := make(chan Result, len(checks))
	var wait sync.WaitGroup
	for _, check := range checks {
		check := check
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- s.runCheck(ctx, check)
		}()
	}
	wait.Wait()
	close(results)
	report := Report{
		GeneratedAt: s.now(), Status: StatusPass,
		Results: make([]Result, 0, len(checks)),
	}
	for result := range results {
		report.Results = append(report.Results, result)
		switch result.Status {
		case StatusFail:
			report.Status = StatusFail
		case StatusWarn:
			if report.Status == StatusPass {
				report.Status = StatusWarn
			}
		}
	}
	sort.Slice(report.Results, func(i, j int) bool {
		return report.Results[i].ID < report.Results[j].ID
	})
	return report
}

func (s *Service) runCheck(parent context.Context, check Check) (result Result) {
	timeout := check.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := s.now()
	result = Result{
		ID: check.ID, Version: check.Version, Category: check.Category,
		Severity: check.Severity, DocsRoute: check.DocsRoute,
		ObservedAt: started,
	}
	defer func() {
		result.DurationMS = max(0, s.now().Sub(started).Milliseconds())
		if recovered := recover(); recovered != nil {
			result.Status = StatusFail
			result.Observed = "check panicked"
			result.Expected = "check completes without panic"
			result.Error = boundedText(fmt.Sprintf("%v", recovered), 512)
			result.Evidence = map[string]any{
				"stack": boundedText(string(debug.Stack()), 2048),
			}
		}
	}()
	observation, err := check.Run(ctx)
	result.Status = observation.Status
	result.Observed = boundedText(observation.Observed, 2048)
	result.Expected = boundedText(observation.Expected, 2048)
	result.Remediation = boundedText(observation.Remediation, 2048)
	result.Evidence = observation.Evidence
	if err != nil {
		result.Error = boundedText(err.Error(), 2048)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Error = "diagnostic check timed out"
		}
		if result.Status == "" {
			result.Status = StatusFail
		}
	}
	if !result.Status.Valid() {
		result.Status = StatusFail
		if result.Error == "" {
			result.Error = "diagnostic check returned an invalid status"
		}
	}
	return result
}

func (s Status) Valid() bool {
	return s == StatusPass || s == StatusWarn || s == StatusFail
}

func boundedText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
