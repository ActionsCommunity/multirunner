package servicehost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	maxFailureExits = 3

	// FailureExitCode signals an unexpected process failure to service managers.
	FailureExitCode = 1
	// FailureWindow bounds the period used to detect a crash loop.
	FailureWindow = 10 * time.Minute
	// CrashLoopFailureCount is the first failure count that exhausts recovery.
	CrashLoopFailureCount = maxFailureExits + 1
)

type failureHistory struct {
	Failures  []time.Time `json:"failures"`
	LastError string      `json:"last_error,omitempty"`
}

// FailureStatus describes recent service recovery activity.
type FailureStatus struct {
	Count       int
	LastFailure time.Time
	LastError   string
}

// RecordFailure persists a fatal run for doctor diagnostics.
func RecordFailure(configPath string, now time.Time, summary string) error {
	history, err := readFailureHistory(configPath)
	if err != nil {
		return err
	}
	history.Failures = recentFailures(history.Failures, now)
	history.Failures = append(history.Failures, now.UTC())
	history.LastError = truncateSummary(summary)
	if err := writeFailureHistory(configPath, history); err != nil {
		return err
	}
	return nil
}

// FailureCount returns fatal runs still inside the recovery window.
func FailureCount(configPath string, now time.Time) (int, error) {
	status, err := ReadFailureStatus(configPath, now)
	return status.Count, err
}

// ReadFailureStatus returns recent retry count and the sanitized last failure.
func ReadFailureStatus(configPath string, now time.Time) (FailureStatus, error) {
	history, err := readFailureHistory(configPath)
	if err != nil {
		return FailureStatus{}, err
	}
	recent := recentFailures(history.Failures, now)
	status := FailureStatus{Count: len(recent)}
	if len(recent) > 0 {
		status.LastFailure = recent[len(recent)-1]
		status.LastError = history.LastError
	}
	return status, nil
}

// ClearFailures resets the crash-loop budget after an intentional stop.
func ClearFailures(configPath string) error {
	err := os.Remove(failurePath(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func recentFailures(failures []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-FailureWindow)
	recent := make([]time.Time, 0, len(failures))
	for _, failure := range failures {
		if failure.After(cutoff) && !failure.After(now) {
			recent = append(recent, failure)
		}
	}
	return recent
}

func readFailureHistory(configPath string) (failureHistory, error) {
	data, err := os.ReadFile(failurePath(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return failureHistory{}, nil
	}
	if err != nil {
		return failureHistory{}, fmt.Errorf("read service failure state: %w", err)
	}
	var history failureHistory
	if err := json.Unmarshal(data, &history); err != nil {
		return failureHistory{}, fmt.Errorf("parse service failure state: %w", err)
	}
	return history, nil
}

func writeFailureHistory(configPath string, history failureHistory) error {
	data, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("encode service failure state: %w", err)
	}
	path := failurePath(configPath)
	temp, err := os.CreateTemp(filepath.Dir(path), ".multirunner-service-state-*")
	if err != nil {
		return fmt.Errorf("create service failure state: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure service failure state: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write service failure state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync service failure state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close service failure state: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace service failure state: %w", err)
	}
	return nil
}

func failurePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ".multirunner-service-state.json")
}

func truncateSummary(summary string) string {
	const maxSummaryBytes = 2048
	if len(summary) <= maxSummaryBytes {
		return summary
	}
	return summary[:maxSummaryBytes]
}
