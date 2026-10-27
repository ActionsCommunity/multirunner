package history

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAnalyticsAggregatesRepositoryAndWorkflowReliability(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	runs := []WorkflowRun{
		{ID: 1, Repository: "o/a", WorkflowName: "CI", Name: "CI", CreatedAt: now, UpdatedAt: now},
		{ID: 2, Repository: "o/a", WorkflowName: "Release", Name: "Release", CreatedAt: now, UpdatedAt: now},
		{ID: 3, Repository: "o/b", WorkflowName: "CI", Name: "CI", CreatedAt: now, UpdatedAt: now},
	}
	for _, run := range runs {
		if err := store.UpsertWorkflowRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}
	job := func(id, runID int64, repository, workflow, conclusion, attribution string, duration time.Duration) {
		started := now.Add(-duration)
		completed := now
		if err := store.UpsertWorkflowJob(t.Context(), WorkflowJob{
			ID: id, RunID: runID, Repository: repository,
			Name: "job", WorkflowName: workflow, Status: "completed",
			Conclusion: conclusion, AttributionSource: attribution,
			CreatedAt: started, StartedAt: &started, CompletedAt: &completed,
			UpdatedAt: completed,
		}); err != nil {
			t.Fatal(err)
		}
	}
	job(11, 1, "o/a", "CI", "success", "exact", 10*time.Second)
	job(12, 1, "o/a", "CI", "failure", "inferred", 20*time.Second)
	job(13, 2, "o/a", "Release", "cancelled", "exact", 30*time.Second)
	job(14, 3, "o/b", "CI", "success", "exact", 40*time.Second)

	repository, err := store.Analytics(t.Context(), AnalyticsOptions{
		GroupBy: "repository", Since: now.Add(-time.Hour), Until: now,
	})
	if err != nil || len(repository.Rows) != 2 {
		t.Fatalf("repository analytics = %+v, %v", repository, err)
	}
	first := repository.Rows[0]
	if first.Repository != "o/a" || first.TotalJobs != 3 ||
		first.SuccessfulJobs != 1 || first.FailedJobs != 1 ||
		first.CancelledJobs != 1 || first.InfrastructureFailures != 1 ||
		first.ExactAttributions != 2 || first.P50DurationSeconds != 20 ||
		first.P95DurationSeconds != 20 {
		t.Fatalf("repository row = %+v", first)
	}

	workflow, err := store.Analytics(t.Context(), AnalyticsOptions{
		GroupBy: "workflow", Since: now.Add(-time.Hour), Until: now,
	})
	if err != nil || len(workflow.Rows) != 3 ||
		workflow.Rows[0].Repository != "o/a" || workflow.Rows[0].Workflow != "CI" {
		t.Fatalf("workflow analytics = %+v, %v", workflow, err)
	}
}

func TestAnalyticsRejectsUnsafeWindows(t *testing.T) {
	now := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
	for _, options := range []AnalyticsOptions{
		{GroupBy: "unknown", Since: now.Add(-time.Hour), Until: now},
		{GroupBy: "repository", Since: now, Until: now.Add(-time.Hour)},
		{GroupBy: "repository", Since: now.AddDate(-2, 0, 0), Until: now},
		{GroupBy: "repository", Since: now, Until: now.Add(6 * time.Minute)},
	} {
		if _, err := normalizeAnalyticsOptions(options, now); !errors.Is(err, ErrInvalidAnalytics) {
			t.Fatalf("normalizeAnalyticsOptions(%+v) error = %v", options, err)
		}
	}
}

func TestAnalyticsPersistsAndInvalidatesDefaultSnapshot(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	if err := store.UpsertWorkflowRun(t.Context(), WorkflowRun{
		ID: 1, Repository: "o/r", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkflowJob(t.Context(), WorkflowJob{
		ID: 1, RunID: 1, Repository: "o/r", Status: "completed",
		Conclusion: "success", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	first, err := store.Analytics(t.Context(), AnalyticsOptions{GroupBy: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Analytics(t.Context(), AnalyticsOptions{GroupBy: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	if !first.GeneratedAt.Equal(second.GeneratedAt) {
		t.Fatalf("snapshot generated_at changed: %s != %s", first.GeneratedAt, second.GeneratedAt)
	}
	var snapshots int
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM analytics_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 {
		t.Fatalf("analytics snapshots = %d, want 1", snapshots)
	}
	if err := store.UpsertWorkflowJob(t.Context(), WorkflowJob{
		ID: 2, RunID: 1, Repository: "o/r", Status: "completed",
		Conclusion: "failure", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Analytics(t.Context(), AnalyticsOptions{GroupBy: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Rows) != 1 || updated.Rows[0].TotalJobs != 2 {
		t.Fatalf("updated analytics = %+v", updated)
	}
}
