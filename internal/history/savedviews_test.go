package history

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestEmptySavedViewsSerializeAsArray(t *testing.T) {
	store := openTestStore(t)
	views, err := store.ListSavedViews(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("empty saved views = %s, want []", encoded)
	}
}

func TestSavedViewsAreIdempotentVersionedAndDeletable(t *testing.T) {
	store := openTestStore(t)
	input := SavedViewInput{
		Name: "Failed CI", Query: "failed ci", EntityType: "run",
		Repository: "actionscommunity/multirunner", State: "failure",
	}
	created, fresh, err := store.CreateSavedView(t.Context(), "create-1", "operator", input)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh || created.Version != 1 || created.Name != "Failed CI" {
		t.Fatalf("created saved view = %+v, fresh=%v", created, fresh)
	}
	replayed, fresh, err := store.CreateSavedView(t.Context(), "create-1", "operator", input)
	if err != nil {
		t.Fatal(err)
	}
	if fresh || replayed.ID != created.ID {
		t.Fatalf("replayed saved view = %+v, fresh=%v", replayed, fresh)
	}
	_, _, err = store.CreateSavedView(t.Context(), "create-1", "operator", SavedViewInput{
		Name: "Different", Query: "failed ci",
	})
	if !errors.Is(err, ErrSavedViewIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	views, err := store.ListSavedViews(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].ID != created.ID {
		t.Fatalf("saved views = %+v", views)
	}
	if _, err := store.DeleteSavedView(t.Context(), created.ID, "delete-1", 2); !errors.Is(err, ErrSavedViewConflict) {
		t.Fatalf("version conflict = %v", err)
	}
	deleted, err := store.DeleteSavedView(t.Context(), created.ID, "delete-1", 1)
	if err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	deleted, err = store.DeleteSavedView(t.Context(), created.ID, "delete-1", 1)
	if err != nil || deleted {
		t.Fatalf("idempotent delete = %v, %v", deleted, err)
	}
}
