// Package operations defines durable operational events and projections for the
// local Multirunner host.
package operations

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const CurrentEventSchemaVersion = 1

type ActorKind string

const (
	ActorSystem   ActorKind = "system"
	ActorOperator ActorKind = "operator"
	ActorGitHub   ActorKind = "github"
)

func (k ActorKind) Valid() bool {
	switch k {
	case ActorSystem, ActorOperator, ActorGitHub:
		return true
	default:
		return false
	}
}

type Host struct {
	ID             string
	InstallationID string
	DisplayName    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type HostEpoch struct {
	ID           string
	HostID       string
	StartedAt    time.Time
	EndedAt      *time.Time
	LastSequence int64
}

type EventInput struct {
	Type          string
	EntityType    string
	EntityID      string
	Timestamp     time.Time
	CorrelationID string
	CausationID   string
	ActorKind     ActorKind
	ActorID       string
	Payload       json.RawMessage
	CommandID     string
}

func (in EventInput) Validate() error {
	if strings.TrimSpace(in.Type) == "" {
		return errors.New("operational event type is required")
	}
	if strings.TrimSpace(in.EntityType) == "" {
		return errors.New("operational event entity type is required")
	}
	if strings.TrimSpace(in.EntityID) == "" {
		return errors.New("operational event entity ID is required")
	}
	if !in.ActorKind.Valid() {
		return fmt.Errorf("invalid operational event actor kind %q", in.ActorKind)
	}
	if len(in.Payload) > 0 && !json.Valid(in.Payload) {
		return errors.New("operational event payload must be valid JSON")
	}
	return nil
}

type Event struct {
	ID            string          `json:"id"`
	SchemaVersion int             `json:"schema_version"`
	HostID        string          `json:"host_id"`
	HostEpoch     string          `json:"host_epoch"`
	Sequence      int64           `json:"sequence"`
	Type          string          `json:"type"`
	EntityType    string          `json:"entity_type"`
	EntityID      string          `json:"entity_id"`
	Timestamp     time.Time       `json:"timestamp"`
	CorrelationID string          `json:"correlation_id"`
	CausationID   string          `json:"causation_id"`
	ActorKind     ActorKind       `json:"actor_kind"`
	ActorID       string          `json:"actor_id"`
	Payload       json.RawMessage `json:"payload"`
	CommandID     string          `json:"command_id"`
	PreviousHash  string          `json:"previous_hash"`
	IntegrityHash string          `json:"integrity_hash"`
}

type EventQuery struct {
	HostEpoch     string
	AfterSequence int64
	MaxSequence   int64
	Since         time.Time
	Until         time.Time
	Type          string
	EntityType    string
	EntityID      string
	Limit         int
}

type ProjectionWatermark struct {
	Name      string
	HostEpoch string
	Sequence  int64
	EventID   string
	UpdatedAt time.Time
}

type RunnerState struct {
	ID          string    `json:"id"`
	HostID      string    `json:"host_id"`
	HostEpoch   string    `json:"host_epoch"`
	Pool        string    `json:"pool"`
	Target      string    `json:"target"`
	Repository  string    `json:"repository"`
	RunnerName  string    `json:"runner_name"`
	Status      string    `json:"status"`
	BackendID   string    `json:"backend_id"`
	Error       string    `json:"error"`
	LastEventID string    `json:"last_event_id"`
	Sequence    int64     `json:"sequence"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Snapshot struct {
	HostEpoch    string        `json:"host_epoch"`
	LastSequence int64         `json:"last_sequence"`
	GeneratedAt  time.Time     `json:"generated_at"`
	Runners      []RunnerState `json:"runners"`
}

func NewOpaqueID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate opaque ID: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return hex.EncodeToString(id[:]), nil
}

func EventID(hostEpoch string, sequence int64) string {
	return fmt.Sprintf("%s:%d", hostEpoch, sequence)
}

func IntegrityHash(previous string, event Event) string {
	sum := sha256.New()
	writeHashPart(sum, previous)
	writeHashPart(sum, event.ID)
	writeHashPart(sum, fmt.Sprintf("%d", event.SchemaVersion))
	writeHashPart(sum, event.HostID)
	writeHashPart(sum, event.HostEpoch)
	writeHashPart(sum, fmt.Sprintf("%d", event.Sequence))
	writeHashPart(sum, event.Type)
	writeHashPart(sum, event.EntityType)
	writeHashPart(sum, event.EntityID)
	writeHashPart(sum, event.Timestamp.UTC().Format(time.RFC3339Nano))
	writeHashPart(sum, event.CorrelationID)
	writeHashPart(sum, event.CausationID)
	writeHashPart(sum, string(event.ActorKind))
	writeHashPart(sum, event.ActorID)
	writeHashPart(sum, string(event.Payload))
	writeHashPart(sum, event.CommandID)
	return hex.EncodeToString(sum.Sum(nil))
}

type hashWriter interface {
	Write([]byte) (int, error)
}

func writeHashPart(writer hashWriter, value string) {
	_, _ = writer.Write([]byte(fmt.Sprintf("%d:", len(value))))
	_, _ = writer.Write([]byte(value))
}
