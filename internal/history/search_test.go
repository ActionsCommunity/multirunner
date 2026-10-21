package history

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestSearchIndexesCanonicalHistoryAndAppliesFilters(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
	run := WorkflowRun{
		ID: 42, Repository: "actionscommunity/multirunner",
		Name: "CI", WorkflowName: "Release verification",
		DisplayTitle: "Verify search projection", HeadBranch: "feature/search",
		HeadSHA: "abc123", Actor: "octocat", Status: "completed",
		Conclusion: "success", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.UpsertWorkflowRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkflowJob(t.Context(), WorkflowJob{
		ID: 84, RunID: 42, Repository: run.Repository,
		Name: "integration linux", WorkflowName: run.WorkflowName,
		HeadBranch: run.HeadBranch, HeadSHA: run.HeadSHA,
		Status: "completed", Conclusion: "success", RunnerName: "runner-alpha",
		PoolName: "linux", CreatedAt: now, UpdatedAt: now,
		Steps: []WorkflowStep{{
			Number: 1, Name: "Run searchable tests", Status: "completed",
			Conclusion: "success",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRunnerSession(t.Context(), RunnerSession{
		ID: "session-1", RunnerName: "runner-alpha", PoolName: "linux",
		Repository: run.Repository, WorkflowRunID: 42, WorkflowJobID: 84,
		Status: "stopped", Conclusion: "success",
		StartedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	loadedRun, err := store.WorkflowRun(t.Context(), run.Repository, run.ID)
	if err != nil || loadedRun.DisplayTitle != run.DisplayTitle {
		t.Fatalf("WorkflowRun = %+v, %v", loadedRun, err)
	}
	jobs, err := store.ListWorkflowJobs(t.Context(), ListOptions{
		Repository: run.Repository, RunID: run.ID,
	})
	if err != nil || len(jobs) != 1 || jobs[0].ID != 84 {
		t.Fatalf("run jobs = %+v, %v", jobs, err)
	}
	loadedJob, err := store.WorkflowJob(t.Context(), 84)
	if err != nil || loadedJob.Name != "integration linux" {
		t.Fatalf("WorkflowJob = %+v, %v", loadedJob, err)
	}
	sessions, err := store.ListRunnerSessions(t.Context(), ListOptions{
		Repository: run.Repository, RunID: run.ID,
	})
	if err != nil || len(sessions) != 1 || sessions[0].ID != "session-1" {
		t.Fatalf("run sessions = %+v, %v", sessions, err)
	}
	if err := store.RecordAccessAudit(t.Context(), AccessAudit{
		OccurredAt: now, ActorKind: "operator", ActorID: "local",
		Action: "job.log.read", TargetType: "workflow_job", TargetID: "84",
		Outcome: "succeeded", ByteCount: 123, Duration: 25 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	var auditBytes int64
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT byte_count FROM access_audit_events WHERE target_id='84'`).Scan(&auditBytes); err != nil ||
		auditBytes != 123 {
		t.Fatalf("access audit bytes = %d, %v", auditBytes, err)
	}

	results, err := store.Search(t.Context(), SearchOptions{
		Query: "release ver", EntityType: "run",
	})
	if err != nil || len(results) != 1 || results[0].EntityType != "run" ||
		results[0].Route != "/runs?repository=actionscommunity%2Fmultirunner&run_id=42" {
		t.Fatalf("run search = %+v, %v", results, err)
	}
	results, err = store.Search(t.Context(), SearchOptions{
		Query: "linux", EntityType: "job", Repository: run.Repository,
	})
	if err != nil || len(results) != 1 || results[0].Title != "Release verification integration linux" ||
		results[0].Route != "/runs?repository=actionscommunity%2Fmultirunner&run_id=42" {
		t.Fatalf("job search = %+v, %v", results, err)
	}
	results, err = store.Search(t.Context(), SearchOptions{
		Query: "searchable", EntityType: "step",
	})
	if err != nil || len(results) != 1 ||
		results[0].Route != "/runs?repository=actionscommunity%2Fmultirunner&run_id=42" {
		t.Fatalf("step search = %+v, %v", results, err)
	}

	run.DisplayTitle = "Emergency hotfix"
	run.WorkflowName = "Hotfix"
	run.Name = "Hotfix"
	if err := store.UpsertWorkflowRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	results, err = store.Search(t.Context(), SearchOptions{Query: "emergency"})
	if err != nil || len(results) != 1 || results[0].Title != "Hotfix Emergency hotfix Hotfix" {
		t.Fatalf("updated search = %+v, %v", results, err)
	}
}

func TestSearchRejectsUnboundedInput(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, options := range []SearchOptions{
		{Query: "x"},
		{Query: "one two three four five six seven eight nine"},
		{Query: "valid", EntityType: "unknown"},
	} {
		if _, err := store.Search(t.Context(), options); !errors.Is(err, ErrInvalidSearch) {
			t.Fatalf("Search(%+v) error = %v", options, err)
		}
	}
}

func TestCommandSearchRowsTrackUpdatesAndDeletion(t *testing.T) {
	store := openTestStore(t)
	request := control.Request{
		IdempotencyKey: "search-command", Type: "pool.pause",
		HostID:     bindCommandEpoch(t, store),
		TargetType: "pool", TargetID: "linux", ConflictDomain: "pool:linux",
		Parameters: json.RawMessage(`{}`),
		ActorKind:  operations.ActorOperator, ActorID: "search-operator",
	}
	command, _, err := store.CreateCommand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"first-reason", "second-reason"} {
		if _, err := store.db.ExecContext(t.Context(),
			`UPDATE commands SET reason=? WHERE id=?`, reason, command.ID); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*)
			FROM search_index JOIN command_search_rows
			ON search_index.rowid=command_search_rows.search_rowid
			WHERE entity_key=? AND context=?`, command.ID, reason+" search-operator",
		).Scan(&count); err != nil || count != 1 {
			t.Fatalf("command search mapping count=%d err=%v", count, err)
		}
	}
	for _, table := range []string{"command_transitions", "audit_events"} {
		if _, err := store.db.ExecContext(t.Context(),
			`DELETE FROM `+table+` WHERE command_id=?`, command.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(t.Context(),
		`DELETE FROM commands WHERE id=?`, command.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"search_index", "command_search_rows"} {
		var count int
		if err := store.db.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count after deletion=%d err=%v", table, count, err)
		}
	}
}
