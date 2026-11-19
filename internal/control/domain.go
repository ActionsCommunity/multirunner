// Package control defines durable, fenced operator commands for the local
// Multirunner runtime.
package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

const CurrentCommandVersion = 1

var (
	ErrNotFound            = errors.New("command not found")
	ErrIdempotencyConflict = errors.New("idempotency key conflicts with an existing command")
	ErrConflictDomainBusy  = errors.New("command conflict domain is busy")
	ErrStateConflict       = errors.New("command state changed")
	ErrStaleFence          = errors.New("command fencing token is stale")
)

type State string

const (
	StateReceived             State = "received"
	StateValidated            State = "validated"
	StateAwaitingConfirmation State = "awaiting_confirmation"
	StateQueued               State = "queued"
	StateClaimed              State = "claimed"
	StateRunning              State = "running"
	StateReconciling          State = "reconciling"
	StateSucceeded            State = "succeeded"
	StateFailed               State = "failed"
	StateCancelled            State = "cancelled"
	StateInterrupted          State = "interrupted"
	StateUnknownOutcome       State = "unknown_outcome"
)

func (s State) Valid() bool {
	switch s {
	case StateReceived, StateValidated, StateAwaitingConfirmation, StateQueued,
		StateClaimed, StateRunning, StateReconciling, StateSucceeded, StateFailed,
		StateCancelled, StateInterrupted, StateUnknownOutcome:
		return true
	default:
		return false
	}
}

func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateInterrupted, StateUnknownOutcome:
		return true
	default:
		return false
	}
}

func (s State) BlocksConflictDomain() bool {
	return !s.Terminal() || s == StateUnknownOutcome
}

type ConfirmationState string

const (
	ConfirmationNotRequired ConfirmationState = "not_required"
	ConfirmationPending     ConfirmationState = "pending"
	ConfirmationConfirmed   ConfirmationState = "confirmed"
)

type Request struct {
	IdempotencyKey string
	Type           string
	Version        int
	HostID         string
	TargetType     string
	TargetID       string
	ConflictDomain string
	Parameters     json.RawMessage
	ActorKind      operations.ActorKind
	ActorID        string
	Reason         string
	Confirmation   ConfirmationState
	CorrelationID  string
	ClientMetadata json.RawMessage
	TimeoutAt      *time.Time
}

func (r Request) Validate() error {
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return errors.New("idempotency key is required")
	}
	if strings.TrimSpace(r.Type) == "" {
		return errors.New("command type is required")
	}
	if r.Version < 1 {
		return errors.New("command version must be positive")
	}
	if strings.TrimSpace(r.HostID) == "" {
		return errors.New("command host ID is required")
	}
	if strings.TrimSpace(r.TargetType) == "" || strings.TrimSpace(r.TargetID) == "" {
		return errors.New("command target type and ID are required")
	}
	if strings.TrimSpace(r.ConflictDomain) == "" {
		return errors.New("command conflict domain is required")
	}
	if !r.ActorKind.Valid() || strings.TrimSpace(r.ActorID) == "" {
		return errors.New("command actor kind and ID are required")
	}
	switch r.Confirmation {
	case ConfirmationNotRequired, ConfirmationPending, ConfirmationConfirmed:
	default:
		return fmt.Errorf("invalid command confirmation state %q", r.Confirmation)
	}
	if len(r.Parameters) > 0 && !json.Valid(r.Parameters) {
		return errors.New("command parameters must be valid JSON")
	}
	if len(r.ClientMetadata) > 0 && !json.Valid(r.ClientMetadata) {
		return errors.New("command client metadata must be valid JSON")
	}
	return nil
}

func (r Request) IdempotencyScope() string {
	return strings.Join([]string{
		string(r.ActorKind), r.ActorID, r.Type, r.TargetType, r.TargetID,
	}, "\x00")
}

func (r Request) CanonicalHash() (string, error) {
	parameters, err := canonicalJSON(r.Parameters)
	if err != nil {
		return "", fmt.Errorf("canonicalize command parameters: %w", err)
	}
	canonical := struct {
		Type           string            `json:"type"`
		Version        int               `json:"version"`
		HostID         string            `json:"host_id"`
		TargetType     string            `json:"target_type"`
		TargetID       string            `json:"target_id"`
		ConflictDomain string            `json:"conflict_domain"`
		Parameters     json.RawMessage   `json:"parameters"`
		Reason         string            `json:"reason"`
		Confirmation   ConfirmationState `json:"confirmation"`
	}{
		Type: r.Type, Version: r.Version, HostID: r.HostID,
		TargetType: r.TargetType, TargetID: r.TargetID,
		ConflictDomain: r.ConflictDomain, Parameters: parameters,
		Reason: r.Reason, Confirmation: r.Confirmation,
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type Command struct {
	ID               string               `json:"id"`
	IdempotencyKey   string               `json:"idempotency_key"`
	IdempotencyScope string               `json:"-"`
	RequestHash      string               `json:"-"`
	Type             string               `json:"type"`
	Version          int                  `json:"version"`
	HostID           string               `json:"host_id"`
	TargetType       string               `json:"target_type"`
	TargetID         string               `json:"target_id"`
	ConflictDomain   string               `json:"conflict_domain"`
	Parameters       json.RawMessage      `json:"parameters"`
	ActorKind        operations.ActorKind `json:"actor_kind"`
	ActorID          string               `json:"actor_id"`
	Reason           string               `json:"reason"`
	Confirmation     ConfirmationState    `json:"confirmation"`
	State            State                `json:"state"`
	CorrelationID    string               `json:"correlation_id"`
	ClientMetadata   json.RawMessage      `json:"client_metadata"`
	CreatedAt        time.Time            `json:"created_at"`
	ValidatedAt      *time.Time           `json:"validated_at,omitempty"`
	QueuedAt         *time.Time           `json:"queued_at,omitempty"`
	StartedAt        *time.Time           `json:"started_at,omitempty"`
	HeartbeatAt      *time.Time           `json:"heartbeat_at,omitempty"`
	CompletedAt      *time.Time           `json:"completed_at,omitempty"`
	TimeoutAt        *time.Time           `json:"timeout_at,omitempty"`
	BeforeState      json.RawMessage      `json:"before_state,omitempty"`
	Outcome          json.RawMessage      `json:"outcome,omitempty"`
	Error            string               `json:"error,omitempty"`
	LeaseOwner       string               `json:"lease_owner,omitempty"`
	LeaseExpiresAt   *time.Time           `json:"lease_expires_at,omitempty"`
	Attempt          int                  `json:"attempt"`
	FencingToken     int64                `json:"fencing_token"`
}

type Transition struct {
	ExpectedState State
	To            State
	ActorKind     operations.ActorKind
	ActorID       string
	Detail        json.RawMessage
	BeforeState   json.RawMessage
	Outcome       json.RawMessage
	Error         string
	LeaseOwner    string
	LeaseDuration time.Duration
	FencingToken  int64
}

func canonicalJSON(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}
