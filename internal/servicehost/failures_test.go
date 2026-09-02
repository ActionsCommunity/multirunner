package servicehost

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordFailureTracksCrashLoopThreshold(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	for attempt := 1; attempt <= 4; attempt++ {
		if err := RecordFailure(configPath, now.Add(time.Duration(attempt)*time.Second), "orchestrator failed"); err != nil {
			t.Fatal(err)
		}
	}
	count, err := FailureCount(configPath, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("failure count = %d, want 4", count)
	}
}

func TestRecordFailureResetsAfterWindow(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	if err := RecordFailure(configPath, now, "first failure"); err != nil {
		t.Fatal(err)
	}
	if err := RecordFailure(configPath, now.Add(FailureWindow+time.Second), "second failure"); err != nil {
		t.Fatal(err)
	}
	count, err := FailureCount(configPath, now.Add(FailureWindow+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("failure count after reset = %d, want 1", count)
	}
}

func TestClearFailuresRemovesCrashLoopState(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := RecordFailure(configPath, time.Now(), "failure"); err != nil {
		t.Fatal(err)
	}
	if err := ClearFailures(configPath); err != nil {
		t.Fatal(err)
	}
	count, err := FailureCount(configPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failure count = %d, want 0", count)
	}
}

func TestReadFailureStatusIncludesLastSanitizedSummary(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	now := time.Now().UTC()
	if err := RecordFailure(configPath, now, "orchestrator failed safely"); err != nil {
		t.Fatal(err)
	}
	status, err := ReadFailureStatus(configPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if status.Count != 1 || status.LastError != "orchestrator failed safely" || !status.LastFailure.Equal(now) {
		t.Fatalf("failure status = %+v", status)
	}
}

func TestRecordFailureBoundsSummarySize(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := RecordFailure(configPath, time.Now(), strings.Repeat("x", 4096)); err != nil {
		t.Fatal(err)
	}
	status, err := ReadFailureStatus(configPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.LastError) != 2048 {
		t.Fatalf("last error bytes = %d, want 2048", len(status.LastError))
	}
}
