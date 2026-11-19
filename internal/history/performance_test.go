package history

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/operations"
)

const benchmarkHistoryRows = 10_000

func benchmarkStore(b *testing.B) *Store {
	b.Helper()
	store, err := Open(context.Background(), filepath.Join(b.TempDir(), "history.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		b.Fatal(err)
	}
	runStatement, err := tx.Prepare(`INSERT INTO workflow_runs (
		repository, id, name, workflow_name, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	jobStatement, err := tx.Prepare(`INSERT INTO workflow_jobs (
		repository, id, run_id, name, status, conclusion, created_at, updated_at,
		workflow_name, attribution_source, started_at, completed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	sessionStatement, err := tx.Prepare(`INSERT INTO runner_sessions (
		id, runner_name, pool_name, repository, status, started_at, updated_at, planned_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index := 1; index <= benchmarkHistoryRows; index++ {
		repository := fmt.Sprintf("owner/repo-%02d", index%20)
		created := start.Add(time.Duration(index) * time.Second).UnixMilli()
		if _, err := runStatement.Exec(repository, index, "CI", "CI", created, created); err != nil {
			b.Fatal(err)
		}
		conclusion := "success"
		if index%10 == 0 {
			conclusion = "failure"
		}
		if _, err := jobStatement.Exec(
			repository, index, index, "test", "completed", conclusion,
			created, created, "CI", "exact", created, created+30_000,
		); err != nil {
			b.Fatal(err)
		}
		if _, err := sessionStatement.Exec(
			strconv.Itoa(index), "runner", "linux", repository,
			"completed", created, created, created,
		); err != nil {
			b.Fatal(err)
		}
	}
	_ = runStatement.Close()
	_ = jobStatement.Close()
	_ = sessionStatement.Close()
	if _, err := tx.Exec(`UPDATE analytics_state SET source_version=1 WHERE id=1`); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return store
}

func BenchmarkAnalyticsSnapshotHit(b *testing.B) {
	store := benchmarkStore(b)
	if _, err := store.Analytics(context.Background(), AnalyticsOptions{
		GroupBy: "repository",
	}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.Analytics(context.Background(), AnalyticsOptions{
			GroupBy: "repository",
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSummaryCacheHit(b *testing.B) {
	store := benchmarkStore(b)
	if _, err := store.Summary(context.Background(), ""); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.Summary(context.Background(), ""); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWorkflowRunCursorPage(b *testing.B) {
	store := benchmarkStore(b)
	after := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).
		Add(5_000 * time.Second)
	options := ListOptions{
		Limit: 100, AfterTime: &after, AfterID: "5000",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.ListWorkflowRuns(context.Background(), options); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCertifiedHundredThousandRunQueryAndCommandBurstCapacity(t *testing.T) {
	store := openTestStore(t)
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.Prepare(`INSERT INTO workflow_runs (
			repository, id, name, workflow_name, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index := 1; index <= 100_000; index++ {
		created := start.Add(time.Duration(index) * time.Second).UnixMilli()
		if _, err := statement.Exec(
			fmt.Sprintf("owner/repo-%02d", index%20), index,
			"CI", "CI", created, created,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	queryStarted := time.Now()
	after := start.Add(50_000 * time.Second)
	runs, err := store.ListWorkflowRuns(t.Context(), ListOptions{
		Limit: 100, AfterTime: &after, AfterID: "50000",
	})
	queryDuration := time.Since(queryStarted)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 100 {
		t.Fatalf("run page length = %d, want 100", len(runs))
	}
	if queryDuration > 2*time.Second {
		t.Fatalf("100,000-run cursor query took %s, SLO is 2s", queryDuration)
	}

	hostID := bindCommandEpoch(t, store)
	burstStarted := time.Now()
	for index := 0; index < 10; index++ {
		request := control.Request{
			IdempotencyKey: fmt.Sprintf("capacity-%d", index),
			Type:           "pool.pause", Version: control.CurrentCommandVersion,
			HostID: hostID, TargetType: "pool", TargetID: fmt.Sprintf("pool-%d", index),
			ConflictDomain: fmt.Sprintf("pool:pool-%d", index),
			Parameters:     json.RawMessage(`{}`),
			ActorKind:      operations.ActorOperator, ActorID: "capacity-test",
			Confirmation: control.ConfirmationNotRequired,
		}
		if _, created, err := store.CreateCommand(t.Context(), request); err != nil || !created {
			t.Fatalf("command %d: created=%v err=%v", index, created, err)
		}
	}
	burstDuration := time.Since(burstStarted)
	if burstDuration > 2*time.Second {
		t.Fatalf("10-command burst took %s, SLO is 2s", burstDuration)
	}
}
