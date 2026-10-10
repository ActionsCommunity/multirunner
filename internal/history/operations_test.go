package history

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestOperationalHostEpochAndEventJournal(t *testing.T) {
	store := openTestStore(t)
	ctx := t.Context()
	host, err := store.EnsureOperationalHost(ctx, "install-1", "workstation")
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.EnsureOperationalHost(ctx, "install-1", "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != host.ID || again.DisplayName != "renamed" {
		t.Fatalf("EnsureOperationalHost = %+v, want stable ID %q and renamed display", again, host.ID)
	}
	started := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	epoch, err := store.StartHostEpoch(ctx, host.ID, started)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AppendOperationalEvent(ctx, epoch.ID, operations.EventInput{
		Type: "runner.planned", EntityType: "runner_session", EntityID: "session-1",
		Timestamp: started.Add(time.Second), CorrelationID: "corr-1",
		ActorKind: operations.ActorSystem, Payload: json.RawMessage(`{"pool":"linux"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendOperationalEvent(ctx, epoch.ID, operations.EventInput{
		Type: "runner.launched", EntityType: "runner_session", EntityID: "session-1",
		Timestamp: started.Add(2 * time.Second), CorrelationID: "corr-1",
		CausationID: first.ID, ActorKind: operations.ActorSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || second.Sequence != 2 ||
		first.ID != operations.EventID(epoch.ID, 1) ||
		second.PreviousHash != first.IntegrityHash {
		t.Fatalf("event chain = first %+v second %+v", first, second)
	}
	snapshot, err := store.OperationalSnapshot(ctx, epoch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LastSequence != 2 || len(snapshot.Runners) != 1 ||
		snapshot.Runners[0].ID != "session-1" || snapshot.Runners[0].Status != "launched" {
		t.Fatalf("OperationalSnapshot = %+v", snapshot)
	}
	historical, err := store.OperationalSnapshotAt(ctx, epoch.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if historical.LastSequence != 1 || len(historical.Runners) != 1 ||
		historical.Runners[0].Status != "planned" ||
		historical.Runners[0].Pool != "linux" {
		t.Fatalf("OperationalSnapshotAt = %+v", historical)
	}
	events, err := store.ListOperationalEvents(ctx, operations.EventQuery{
		HostEpoch: epoch.ID, AfterSequence: 0, MaxSequence: 1,
		EntityType: "runner_session", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != first.ID ||
		string(events[0].Payload) != `{"pool":"linux"}` {
		t.Fatalf("ListOperationalEvents = %+v", events)
	}
	events[0].Payload[0] = '['
	reloaded, err := store.ListOperationalEvents(ctx, operations.EventQuery{
		HostEpoch: epoch.ID, AfterSequence: 0, MaxSequence: 1, Limit: 10,
	})
	if err != nil || string(reloaded[0].Payload) != `{"pool":"linux"}` {
		t.Fatalf("payload alias or reload error: %q %v", reloaded[0].Payload, err)
	}
}

func TestOperationalProjectionFailureRollsBackEventAndSequence(t *testing.T) {
	store := openTestStore(t)
	host, err := store.EnsureOperationalHost(t.Context(), "install-1", "")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.StartHostEpoch(t.Context(), host.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{
		Type: "runner.planned", EntityType: "runner_session", EntityID: "session",
		ActorKind: operations.ActorSystem, Payload: json.RawMessage(`[]`),
	})
	if err == nil {
		t.Fatal("invalid projection payload was accepted")
	}
	event, err := store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{
		Type: "runner.planned", EntityType: "runner_session", EntityID: "session",
		ActorKind: operations.ActorSystem, Payload: json.RawMessage(`{"pool":"linux"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.Sequence != 1 {
		t.Fatalf("sequence after rollback = %d, want 1", event.Sequence)
	}
}

func TestOperationalJournalRejectsInvalidAndEndedEpoch(t *testing.T) {
	store := openTestStore(t)
	host, err := store.EnsureOperationalHost(t.Context(), "install-1", "")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.StartHostEpoch(t.Context(), host.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{}); err == nil {
		t.Fatal("invalid event was accepted")
	}
	if err := store.EndHostEpoch(t.Context(), epoch.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{
		Type: "runner.failed", EntityType: "runner_session", EntityID: "session",
		ActorKind: operations.ActorSystem,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("append to ended epoch error = %v, want ErrNotFound", err)
	}
}

func TestUnfinishedHostEpochsExcludesCurrentAndEndedEpochs(t *testing.T) {
	store := openTestStore(t)
	host, err := store.EnsureOperationalHost(t.Context(), "install-1", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.StartHostEpoch(
		t.Context(), host.ID, time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := store.StartHostEpoch(
		t.Context(), host.ID, time.Date(2026, 10, 9, 11, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndHostEpoch(t.Context(), ended.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	current, err := store.StartHostEpoch(
		t.Context(), host.ID, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	epochs, err := store.UnfinishedHostEpochs(t.Context(), host.ID, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(epochs) != 1 || epochs[0].ID != first.ID {
		t.Fatalf("unfinished epochs = %+v, want only %s", epochs, first.ID)
	}
}

func TestProjectionWatermarkDoesNotMoveBackward(t *testing.T) {
	store := openTestStore(t)
	host, err := store.EnsureOperationalHost(t.Context(), "install-1", "")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.StartHostEpoch(t.Context(), host.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var events []operations.Event
	for _, eventType := range []string{"runner.planned", "runner.launched"} {
		event, err := store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{
			Type: eventType, EntityType: "runner_session", EntityID: "session",
			ActorKind: operations.ActorSystem,
		})
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := store.SetProjectionWatermark(t.Context(), operations.ProjectionWatermark{
		Name: "runner_state", HostEpoch: epoch.ID, Sequence: 2, EventID: events[1].ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProjectionWatermark(t.Context(), operations.ProjectionWatermark{
		Name: "runner_state", HostEpoch: epoch.ID, Sequence: 1, EventID: events[0].ID,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ProjectionWatermark(t.Context(), "runner_state")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sequence != 2 || got.EventID != events[1].ID {
		t.Fatalf("ProjectionWatermark = %+v", got)
	}
}
