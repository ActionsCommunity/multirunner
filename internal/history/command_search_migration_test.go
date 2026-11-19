package history

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestCommandSearchMigrationBackfillsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	createDatabaseAtVersion(t, path, 15)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &Store{db: db}
	command, _, err := legacy.CreateCommand(t.Context(), control.Request{
		IdempotencyKey: "legacy-search", Type: "pool.pause", HostID: "host",
		TargetType: "pool", TargetID: "linux", ConflictDomain: "pool:linux",
		Parameters: json.RawMessage(`{}`),
		ActorKind:  operations.ActorOperator, ActorID: "operator",
	})
	if closeErr := db.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(t.Context(),
		`UPDATE commands SET reason='migrated command' WHERE id=?`, command.ID,
	); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(t.Context(), SearchOptions{
		Query: "migrated", EntityType: "command",
	})
	if err != nil || len(results) != 1 || results[0].EntityKey != command.ID {
		t.Fatalf("migrated command search=%+v err=%v", results, err)
	}
	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*)
		FROM search_index WHERE entity_type='command' AND entity_key=?`,
		command.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migrated command row count=%d err=%v", count, err)
	}
}
