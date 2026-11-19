package history

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenRejectsSchemaNewerThanBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	future := CurrentSchemaVersion() + 1
	if _, err := db.Exec(
		`INSERT INTO schema_migrations(version, applied_at) VALUES(?, 0)`,
		future,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(t.Context(), path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open future schema error = %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func TestOnlineBackupPreservesNewerSchemaWithoutOpeningStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future-complete.db")
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	host, err := store.EnsureOperationalHost(t.Context(), "installation-1", "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartHostEpoch(t.Context(), host.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	future := CurrentSchemaVersion() + 1
	if _, err := db.Exec(
		`INSERT INTO schema_migrations(version, applied_at) VALUES(?, 0)`,
		future,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(t.TempDir(), "future-backup.db")
	if err := OnlineBackupDatabase(t.Context(), path, backupPath); err != nil {
		t.Fatal(err)
	}
	validation, err := ValidateBackupDatabase(t.Context(), backupPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if validation.SchemaVersion != future || validation.HostID != host.ID {
		t.Fatalf("future backup validation = %+v", validation)
	}
}

func TestOpenConfiguresSQLiteAndMigratesIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "history.db")
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var journal string
	if err := store.db.QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	var busyTimeout, foreignKeys, migrations int
	if err := store.db.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 || foreignKeys != 1 || migrations != len(migrationsForTest()) {
		t.Errorf("SQLite setup: busy_timeout=%d foreign_keys=%d migrations=%d", busyTimeout, foreignKeys, migrations)
	}
	if stats := store.db.Stats(); stats.MaxOpenConnections != 1 {
		t.Errorf("max open connections = %d, want 1", stats.MaxOpenConnections)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if err := reopened.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != len(migrationsForTest()) {
		t.Errorf("migration rows after reopen = %d", migrations)
	}
}

func TestWorkflowStepBatchUsesOneInsertQuery(t *testing.T) {
	job := WorkflowJob{ID: 1, Repository: "o/r"}
	for number := 1; number <= 100; number++ {
		job.Steps = append(job.Steps, WorkflowStep{Number: number, Name: "step"})
	}
	query, args, err := workflowStepBatch(job)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(query, "INSERT INTO workflow_steps"); got != 1 {
		t.Fatalf("workflow step insert query count = %d, want 1", got)
	}
	if got := strings.Count(query, "(?, ?, ?, ?, ?, ?, ?, ?)"); got != 100 {
		t.Fatalf("workflow step value groups = %d, want 100", got)
	}
	if len(args) != 800 {
		t.Fatalf("workflow step batch args = %d, want 800", len(args))
	}
}

func TestWorkflowStepsBatchQueryUsesOneReadForAllJobs(t *testing.T) {
	jobs := make([]WorkflowJob, 1000)
	for index := range jobs {
		jobs[index] = WorkflowJob{Repository: "o/r", ID: int64(index + 1)}
	}
	query, args := workflowStepsBatchQuery(jobs)
	if got := strings.Count(query, "SELECT "); got != 1 {
		t.Fatalf("workflow step read query count = %d, want 1", got)
	}
	if got := strings.Count(query, "(?, ?)"); got != 1000 {
		t.Fatalf("workflow step requested rows = %d, want 1000", got)
	}
	if len(args) != 2000 {
		t.Fatalf("workflow step read args = %d, want 2000", len(args))
	}
}

func TestLifecycleUpsertsListsAndSummary(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
	run := WorkflowRun{
		ID: 101, Repository: "o/r", Name: "CI", WorkflowName: "CI",
		Status: "queued", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.UpsertWorkflowRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	run.Status = "completed"
	run.Conclusion = "success"
	run.CompletedAt = timePointer(now.Add(3 * time.Minute))
	if err := store.UpsertWorkflowRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}

	job := WorkflowJob{
		ID: 201, RunID: run.ID, Repository: run.Repository, Name: "test",
		Status: "completed", Conclusion: "success", CreatedAt: now,
		CompletedAt: timePointer(now.Add(2 * time.Minute)),
		Steps: []WorkflowStep{
			{Number: 1, Name: "checkout", Status: "completed", Conclusion: "success"},
			{Number: 2, Name: "test", Status: "completed", Conclusion: "success"},
		},
	}
	if err := store.UpsertWorkflowJob(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	secondJob := job
	secondJob.ID = 202
	secondJob.Name = "build"
	secondJob.Steps = []WorkflowStep{
		{Number: 1, Name: "build", Status: "completed", Conclusion: "success"},
		{Number: 2, Name: "package", Status: "completed", Conclusion: "success"},
	}
	if err := store.UpsertWorkflowJob(t.Context(), secondJob); err != nil {
		t.Fatal(err)
	}
	job.Steps = job.Steps[:1]
	if err := store.UpsertWorkflowJob(t.Context(), job); err != nil {
		t.Fatal(err)
	}

	session := RunnerSession{
		ID: "session-1", RunnerName: "runner-1", PoolName: "linux",
		Repository: "o/r", WorkflowRunID: run.ID, WorkflowJobID: job.ID,
		Status: "running", StartedAt: now,
	}
	if err := store.UpsertRunnerSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingRunnerSessions(t.Context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("PendingRunnerSessions = %d, %v", len(pending), err)
	}
	if err := store.CompleteRunnerSession(t.Context(), session.ID, "completed", "success", "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRunnerSession(t.Context(), "missing", "completed", "success", "", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing completion error = %v, want ErrNotFound", err)
	}

	sessions, err := store.ListRunnerSessions(t.Context(), ListOptions{Repository: "o/r"})
	if err != nil || len(sessions) != 1 || sessions[0].Conclusion != "success" {
		t.Fatalf("ListRunnerSessions = %+v, %v", sessions, err)
	}
	jobs, err := store.ListWorkflowJobs(t.Context(), ListOptions{Repository: "o/r"})
	if err != nil || len(jobs) != 2 ||
		len(jobs[0].Steps)+len(jobs[1].Steps) != 3 {
		t.Fatalf("ListWorkflowJobs = %+v, %v", jobs, err)
	}
	summary, err := store.Summary(t.Context(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if summary.RunnerSessions != 1 || summary.PendingSessions != 0 ||
		summary.WorkflowRuns != 1 || summary.WorkflowJobs != 2 || summary.WorkflowSteps != 3 ||
		summary.SuccessfulJobs != 2 {
		t.Errorf("summary = %+v", summary)
	}
}

func TestWebhookDedupeSyncStateAndPrune(t *testing.T) {
	store := openTestStore(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	delivery := WebhookDelivery{ID: "delivery-1", Event: "workflow_job", ReceivedAt: old}
	inserted, err := store.RecordWebhookDelivery(t.Context(), delivery)
	if err != nil || !inserted {
		t.Fatalf("first delivery: inserted=%v err=%v", inserted, err)
	}
	inserted, err = store.RecordWebhookDelivery(t.Context(), delivery)
	if err != nil || inserted {
		t.Fatalf("duplicate delivery: inserted=%v err=%v", inserted, err)
	}
	if err := store.MarkWebhookProcessed(t.Context(), delivery.ID, old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	state := SyncState{Key: "o/r", Cursor: "first", UpdatedAt: old}
	if err := store.SetSyncState(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	state.Cursor = "second"
	if err := store.SetSyncState(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	got, err := store.SyncState(t.Context(), state.Key)
	if err != nil || got.Cursor != "second" {
		t.Fatalf("SyncState = %+v, %v", got, err)
	}

	completed := old.Add(time.Hour)
	if err := store.UpsertRunnerSession(t.Context(), RunnerSession{
		ID: "old", Status: "completed", StartedAt: old, CompletedAt: &completed,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := store.PruneBefore(t.Context(), old.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.RunnerSessions != 1 || result.WebhookDeliveries != 1 {
		t.Errorf("PruneBefore = %+v", result)
	}
	summary, err := store.Summary(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if summary.RunnerSessions != 0 || summary.WebhookDeliveries != 0 {
		t.Errorf("summary after prune = %+v", summary)
	}
}

func TestStableRunAndSessionCursors(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for id := int64(1); id <= 3; id++ {
		if err := store.UpsertWorkflowRun(t.Context(), WorkflowRun{
			ID: id, Repository: "o/r", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		sessionID := strconv.FormatInt(id, 10)
		if err := store.UpsertRunnerSession(t.Context(), RunnerSession{
			ID: sessionID, Repository: "o/r", Status: "running",
			StartedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := store.ListWorkflowRuns(t.Context(), ListOptions{Limit: 2})
	if err != nil || len(runs) != 2 || runs[0].ID != 3 || runs[1].ID != 2 {
		t.Fatalf("first run page = %+v, %v", runs, err)
	}
	runs, err = store.ListWorkflowRuns(t.Context(), ListOptions{
		Limit: 2, AfterTime: &runs[1].CreatedAt,
		AfterID: strconv.FormatInt(runs[1].ID, 10),
	})
	if err != nil || len(runs) != 1 || runs[0].ID != 1 {
		t.Fatalf("second run page = %+v, %v", runs, err)
	}
	sessions, err := store.ListRunnerSessions(t.Context(), ListOptions{Limit: 2})
	if err != nil || len(sessions) != 2 || sessions[0].ID != "3" || sessions[1].ID != "2" {
		t.Fatalf("first session page = %+v, %v", sessions, err)
	}
	sessions, err = store.ListRunnerSessions(t.Context(), ListOptions{
		Limit: 2, AfterTime: &sessions[1].StartedAt, AfterID: sessions[1].ID,
	})
	if err != nil || len(sessions) != 1 || sessions[0].ID != "1" {
		t.Fatalf("second session page = %+v, %v", sessions, err)
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func migrationsForTest() []migration { return migrations }
