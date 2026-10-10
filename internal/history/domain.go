// Package history stores normalized runner and GitHub Actions history.
package history

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrNotFound = errors.New("history record not found")
var ErrAmbiguous = errors.New("history record is ambiguous")
var ErrSchemaTooNew = errors.New("history database schema is newer than this binary supports")
var ErrMigrationLedger = errors.New("history database migration ledger is invalid")
var ErrHistoryCompacted = errors.New("requested operational history has been compacted")

// RunnerSession is one local runner lifecycle.
type RunnerSession struct {
	ID                    string
	RunnerName            string
	PoolName              string
	Target                string
	Repository            string
	WorkflowRunID         int64
	RunAttempt            int
	WorkflowJobID         int64
	GitHubRegistrationID  int64
	BackendID             string
	Status                string
	Conclusion            string
	AttributionSource     string
	AttributionConfidence int
	PlannedAt             time.Time
	RegisteredAt          *time.Time
	LaunchedAt            *time.Time
	StartedAt             time.Time
	CompletedAt           *time.Time
	UpdatedAt             time.Time
	ExitCode              int
	Error                 string
}

// WorkflowRun is a GitHub Actions workflow run.
type WorkflowRun struct {
	ID              int64
	Repository      string
	Name            string
	WorkflowName    string
	WorkflowID      int64
	WorkflowPath    string
	DisplayTitle    string
	Event           string
	Status          string
	Conclusion      string
	HeadBranch      string
	HeadSHA         string
	Actor           string
	TriggeringActor string
	APIURL          string
	HTMLURL         string
	RunNumber       int64
	RunAttempt      int
	CreatedAt       time.Time
	UpdatedAt       time.Time
	StartedAt       *time.Time
	CompletedAt     *time.Time
}

// WorkflowJob is a GitHub Actions job and its ordered steps.
type WorkflowJob struct {
	ID                    int64
	RunID                 int64
	RunAttempt            int
	Repository            string
	Name                  string
	WorkflowName          string
	HeadBranch            string
	HeadSHA               string
	Status                string
	Conclusion            string
	Labels                []string
	RunnerID              int64
	RunnerName            string
	RunnerGroup           string
	PoolName              string
	LocalSessionID        string
	AttributionSource     string
	AttributionConfidence int
	APIURL                string
	HTMLURL               string
	CreatedAt             time.Time
	StartedAt             *time.Time
	CompletedAt           *time.Time
	UpdatedAt             time.Time
	Steps                 []WorkflowStep
}

// WorkflowStep is one step within a workflow job.
type WorkflowStep struct {
	Number      int
	Name        string
	Status      string
	Conclusion  string
	StartedAt   *time.Time
	CompletedAt *time.Time
}

// WebhookDelivery records a GitHub delivery ID for durable deduplication.
type WebhookDelivery struct {
	ID          string
	Event       string
	Action      string
	Repository  string
	ReceivedAt  time.Time
	ProcessedAt *time.Time
	Payload     []byte
}

// SyncState is a durable cursor or watermark for a named synchronization task.
type SyncState struct {
	Key              string
	Repository       string
	Phase            string
	Cursor           string
	LastSuccessAt    *time.Time
	LastError        string
	RetryAfter       *time.Time
	BackfillComplete bool
	RateRemaining    int
	RateResetAt      *time.Time
	UpdatedAt        time.Time
}

// ListOptions filters list queries. Limit defaults to 100 and is capped at 1000.
type ListOptions struct {
	Repository  string
	RunID       int64
	JobID       int64
	Workflow    string
	Job         string
	Pool        string
	Status      string
	Conclusion  string
	Branch      string
	Actor       string
	Attribution string
	Query       string
	Since       *time.Time
	Until       *time.Time
	Limit       int
	Offset      int
	AfterTime   *time.Time
	AfterID     string
}

type AccessAudit struct {
	ID            string
	OccurredAt    time.Time
	ActorKind     string
	ActorID       string
	Action        string
	TargetType    string
	TargetID      string
	CorrelationID string
	Outcome       string
	ByteCount     int64
	Duration      time.Duration
	ErrorCode     string
}

// Summary contains aggregate history counts for an optional repository.
type Summary struct {
	RunnerSessions         int64
	PendingSessions        int64
	WorkflowRuns           int64
	WorkflowJobs           int64
	WorkflowSteps          int64
	SuccessfulJobs         int64
	FailedJobs             int64
	CancelledJobs          int64
	AverageDurationSeconds float64
	WebhookDeliveries      int64
}

type AuditRecord struct {
	ID            string          `json:"id"`
	Source        string          `json:"source"`
	OccurredAt    time.Time       `json:"occurred_at"`
	ActorKind     string          `json:"actor_kind"`
	ActorID       string          `json:"actor_id"`
	Action        string          `json:"action"`
	TargetType    string          `json:"target_type"`
	TargetID      string          `json:"target_id"`
	CorrelationID string          `json:"correlation_id"`
	Outcome       string          `json:"outcome,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// PruneResult reports rows directly removed by retention cleanup. Jobs and
// steps removed by workflow-run cascading are included in WorkflowJobs and
// WorkflowSteps.
type PruneResult struct {
	RunnerSessions    int64
	WorkflowRuns      int64
	WorkflowJobs      int64
	WorkflowSteps     int64
	WebhookDeliveries int64
}
