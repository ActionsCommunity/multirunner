package operations

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventInputValidation(t *testing.T) {
	valid := EventInput{
		Type: "runner.planned", EntityType: "runner_session", EntityID: "session-1",
		ActorKind: ActorSystem, Payload: json.RawMessage(`{"pool":"linux"}`),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	tests := []struct {
		name  string
		input EventInput
		want  string
	}{
		{name: "type", input: EventInput{EntityType: "runner", EntityID: "1", ActorKind: ActorSystem}, want: "type"},
		{name: "entity type", input: EventInput{Type: "runner.test", EntityID: "1", ActorKind: ActorSystem}, want: "entity type"},
		{name: "entity ID", input: EventInput{Type: "runner.test", EntityType: "runner", ActorKind: ActorSystem}, want: "entity ID"},
		{name: "actor", input: EventInput{Type: "runner.test", EntityType: "runner", EntityID: "1", ActorKind: "root"}, want: "actor kind"},
		{name: "payload", input: EventInput{Type: "runner.test", EntityType: "runner", EntityID: "1", ActorKind: ActorSystem, Payload: json.RawMessage(`{`)}, want: "valid JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.input.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestOpaqueIDAndIntegrityHash(t *testing.T) {
	left, err := NewOpaqueID()
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewOpaqueID()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 32 || left == right {
		t.Fatalf("opaque IDs = %q and %q", left, right)
	}

	event := Event{
		ID: EventID("epoch", 1), SchemaVersion: CurrentEventSchemaVersion,
		HostID: "host", HostEpoch: "epoch", Sequence: 1,
		Type: "runner.planned", EntityType: "runner_session", EntityID: "session",
		Timestamp: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
		ActorKind: ActorSystem, Payload: json.RawMessage(`{"pool":"linux"}`),
	}
	first := IntegrityHash("", event)
	if len(first) != 64 || first != IntegrityHash("", event) {
		t.Fatalf("integrity hash = %q", first)
	}
	event.Payload = json.RawMessage(`{"pool":"windows"}`)
	if IntegrityHash("", event) == first {
		t.Fatal("integrity hash did not change with payload")
	}
}
