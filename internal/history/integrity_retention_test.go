package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestTieredRetentionCompactsOperationalPrefixAndPreservesVerification(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	host, err := store.EnsureOperationalHost(t.Context(), "retention-install", "host")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.StartHostEpoch(t.Context(), host.ID, now.Add(-300*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for index, timestamp := range []time.Time{
		now.Add(-200 * 24 * time.Hour),
		now.Add(-150 * 24 * time.Hour),
		now.Add(-10 * 24 * time.Hour),
	} {
		if _, err := store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{
			Type: "test.event", EntityType: "test", EntityID: "item",
			Timestamp: timestamp, ActorKind: operations.ActorSystem,
			Payload: json.RawMessage(fmt.Sprintf(`{"index":%d}`, index+1)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(t.Context(), `INSERT INTO health_snapshots (
		id, host_id, host_epoch, observed_at, status, payload_json
	) VALUES ('old-health', ?, ?, ?, 'ok', '{}')`,
		host.ID, epoch.ID, timeMillis(now.Add(-40*24*time.Hour))); err != nil {
		t.Fatal(err)
	}

	result, err := store.PruneTiered(t.Context(), now, DefaultTieredRetentionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if result.OperationalEvents != 2 || result.HealthSnapshots != 1 {
		t.Fatalf("tiered prune result = %+v", result)
	}
	health, err := store.DatabaseHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.VerifiedEvents != 1 || health.CompactedEvents != 2 {
		t.Fatalf("database health after compaction = %+v", health)
	}
	var through int64
	if err := store.db.QueryRowContext(t.Context(), `SELECT through_sequence
		FROM operational_event_anchors WHERE host_epoch=?`, epoch.ID).Scan(&through); err != nil {
		t.Fatal(err)
	}
	if through != 2 {
		t.Fatalf("compaction anchor = %d, want 2", through)
	}
}

func TestTieredRetentionProtectsOneYearOfAuditData(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, item := range []struct {
		id string
		at time.Time
	}{
		{"protected", now.Add(-200 * 24 * time.Hour)},
		{"expired", now.Add(-400 * 24 * time.Hour)},
	} {
		if _, err := store.db.ExecContext(t.Context(), `INSERT INTO access_audit_events (
			id, occurred_at, actor_kind, actor_id, action, target_type, target_id,
			outcome
		) VALUES (?, ?, 'system', '', 'read', 'test', 'item', 'allowed')`,
			item.id, timeMillis(item.at)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.PruneTiered(t.Context(), now, TieredRetentionPolicy{
		Audit: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AccessAuditEvents != 1 {
		t.Fatalf("pruned access audit events = %d, want 1", result.AccessAuditEvents)
	}
	var remaining string
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT id FROM access_audit_events`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != "protected" {
		t.Fatalf("remaining audit event = %q", remaining)
	}
}

func TestOperationalAppendContinuesAfterFullPrefixCompaction(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	host, err := store.EnsureOperationalHost(t.Context(), "append-install", "host")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.StartHostEpoch(t.Context(), host.ID, now.Add(-200*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 2; sequence++ {
		if _, err := store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{
			Type: "test.event", EntityType: "test", EntityID: "item",
			Timestamp: now.Add(-150 * 24 * time.Hour),
			ActorKind: operations.ActorSystem,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.PruneTiered(
		t.Context(), now, DefaultTieredRetentionPolicy(),
	); err != nil {
		t.Fatal(err)
	}
	event, err := store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{
		Type: "test.event", EntityType: "test", EntityID: "item",
		Timestamp: now, ActorKind: operations.ActorSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.Sequence != 3 || event.PreviousHash == "" {
		t.Fatalf("event after compaction = %+v", event)
	}
	if _, err := store.OperationalSnapshotAt(t.Context(), epoch.ID, 2); !errors.Is(
		err, ErrHistoryCompacted,
	) {
		t.Fatalf("OperationalSnapshotAt compacted error = %v", err)
	}
	if _, err := store.DatabaseHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOperationalCorruptionFailsReadinessAndBackupValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	host, err := store.EnsureOperationalHost(t.Context(), "corrupt-install", "host")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.StartHostEpoch(t.Context(), host.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{
		Type: "test.event", EntityType: "test", EntityID: "item",
		ActorKind: operations.ActorSystem, Payload: json.RawMessage(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE operational_events
		SET payload_json='{"ok":false}' WHERE id=?`, event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DatabaseHealth(t.Context()); err == nil {
		t.Fatal("DatabaseHealth accepted a rewritten operational event")
	}
	backupPath := filepath.Join(t.TempDir(), "corrupt-backup.db")
	if err := store.OnlineBackup(t.Context(), backupPath); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBackupDatabase(t.Context(), backupPath, false); err == nil {
		t.Fatal("ValidateBackupDatabase accepted a rewritten operational event")
	}
}

func TestMigrationAddsForeignKeyIndexesAndRemovesRedundantIndexes(t *testing.T) {
	store := openTestStore(t)
	required := map[string]string{
		"idx_operational_events_host":            "host_id",
		"idx_projection_watermarks_epoch":        "host_epoch",
		"idx_projection_watermarks_event":        "event_id",
		"idx_health_snapshots_epoch":             "host_epoch",
		"idx_runner_state_host":                  "host_id",
		"idx_runner_state_last_event":            "last_event_id",
		"idx_alert_evaluated_rule":               "rule_id",
		"idx_notification_deliveries_transition": "transition_id",
		"idx_notification_deliveries_endpoint":   "endpoint_id",
		"idx_backups_host":                       "host_id",
		"idx_restores_host":                      "host_id",
		"idx_updates_host":                       "host_id",
	}
	for index, column := range required {
		var got string
		if err := store.db.QueryRowContext(t.Context(),
			`SELECT name FROM pragma_index_info(?) WHERE seqno=0`,
			index).Scan(&got); err != nil {
			t.Fatalf("read index %s: %v", index, err)
		}
		if got != column {
			t.Errorf("index %s first column = %q, want %q", index, got, column)
		}
	}
	for _, index := range []string{
		"idx_operational_events_epoch_sequence",
		"idx_saved_views_name",
	} {
		var count int
		if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*)
			FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("redundant index %s still exists", index)
		}
	}
}
