// Package alerts defines durable alert and notification state.
package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

var (
	ErrNotFound            = errors.New("alert not found")
	ErrStateConflict       = errors.New("alert state changed")
	ErrIdempotencyConflict = errors.New("alert idempotency key conflicts with an existing mutation")
)

type Endpoint struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	ConfigRef string `json:"config_ref"`
	Enabled   bool   `json:"enabled"`
}

func EndpointID(configRef string) string {
	sum := sha256.Sum256([]byte(configRef))
	return hex.EncodeToString(sum[:16])
}

type State string

const (
	StatePending  State = "pending"
	StateOpen     State = "open"
	StateResolved State = "resolved"
)

type Rule struct {
	ID        string        `json:"id"`
	Version   int           `json:"version"`
	Name      string        `json:"name"`
	Family    string        `json:"family"`
	Severity  string        `json:"severity"`
	EventType string        `json:"event_type"`
	Hold      time.Duration `json:"hold"`
	Cooldown  time.Duration `json:"cooldown"`
	Enabled   bool          `json:"enabled"`
}

type Instance struct {
	ID              string          `json:"id"`
	RuleID          string          `json:"rule_id"`
	DedupKey        string          `json:"dedup_key"`
	State           State           `json:"state"`
	Severity        string          `json:"severity"`
	Summary         string          `json:"summary"`
	Details         json.RawMessage `json:"details"`
	SourceEventID   string          `json:"source_event_id"`
	FirstObservedAt time.Time       `json:"first_observed_at"`
	LastObservedAt  time.Time       `json:"last_observed_at"`
	DueAt           time.Time       `json:"due_at"`
	OpenedAt        *time.Time      `json:"opened_at,omitempty"`
	ResolvedAt      *time.Time      `json:"resolved_at,omitempty"`
	AcknowledgedAt  *time.Time      `json:"acknowledged_at,omitempty"`
	AcknowledgedBy  string          `json:"acknowledged_by,omitempty"`
	SilencedUntil   *time.Time      `json:"silenced_until,omitempty"`
	LastNotifiedAt  *time.Time      `json:"last_notified_at,omitempty"`
	OccurrenceCount int             `json:"occurrence_count"`
	Version         int             `json:"version"`
}

type ListOptions struct {
	State    State
	Severity string
	Limit    int
	Offset   int
}

type Silence struct {
	ID            string    `json:"id"`
	AlertID       string    `json:"alert_id"`
	StartsAt      time.Time `json:"starts_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by"`
	Reason        string    `json:"reason"`
	CorrelationID string    `json:"correlation_id"`
}

type Annotation struct {
	ID            string    `json:"id"`
	AlertID       string    `json:"alert_id"`
	Body          string    `json:"body"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by"`
	CorrelationID string    `json:"correlation_id"`
}

type Observation struct {
	Rule          Rule
	DedupKey      string
	Summary       string
	Details       json.RawMessage
	SourceEventID string
	ObservedAt    time.Time
	CorrelationID string
}

func (o Observation) Validate() error {
	if strings.TrimSpace(o.Rule.ID) == "" || strings.TrimSpace(o.DedupKey) == "" {
		return errors.New("alert rule and dedup key are required")
	}
	if strings.TrimSpace(o.Summary) == "" {
		return errors.New("alert summary is required")
	}
	if len(o.Details) > 0 && !json.Valid(o.Details) {
		return errors.New("alert details must be valid JSON")
	}
	return nil
}

type TransitionRequest struct {
	AlertID         string
	IdempotencyKey  string
	ExpectedState   State
	ExpectedVersion int
	To              State
	ActorKind       operations.ActorKind
	ActorID         string
	Reason          string
	Detail          json.RawMessage
	CorrelationID   string
	OccurredAt      time.Time
}

type SilenceRequest struct {
	AlertID         string
	IdempotencyKey  string
	ExpectedVersion int
	Until           time.Time
	ActorID         string
	Reason          string
	CorrelationID   string
	OccurredAt      time.Time
}

type AnnotationRequest struct {
	AlertID        string
	IdempotencyKey string
	Body           string
	ActorID        string
	CorrelationID  string
	OccurredAt     time.Time
}

type AcknowledgeRequest struct {
	AlertID         string
	IdempotencyKey  string
	ExpectedVersion int
	ActorID         string
	Reason          string
	CorrelationID   string
	OccurredAt      time.Time
}

type DeliveryState string

const (
	DeliveryPending    DeliveryState = "pending"
	DeliveryClaimed    DeliveryState = "claimed"
	DeliveryRetry      DeliveryState = "retry"
	DeliverySucceeded  DeliveryState = "succeeded"
	DeliveryDeadLetter DeliveryState = "dead_letter"
)

type Delivery struct {
	ID             string          `json:"id"`
	AlertID        string          `json:"alert_id"`
	TransitionID   string          `json:"transition_id"`
	EndpointID     string          `json:"endpoint_id"`
	DedupKey       string          `json:"dedup_key"`
	Payload        json.RawMessage `json:"payload"`
	State          DeliveryState   `json:"state"`
	Attempt        int             `json:"attempt"`
	NextAttemptAt  time.Time       `json:"next_attempt_at"`
	LeaseOwner     string          `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	DeliveredAt    *time.Time      `json:"delivered_at,omitempty"`
	DeadLetteredAt *time.Time      `json:"dead_lettered_at,omitempty"`
}
