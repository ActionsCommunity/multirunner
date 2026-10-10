// Package restore stages verified history database restores for activation by
// the service supervisor while the authoritative database is closed.
package restore

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/GerardSmit/multirunner/internal/backup"
)

var (
	ErrNotFound     = errors.New("restore not found")
	ErrConflict     = errors.New("another restore is already staged")
	ErrIntegrity    = errors.New("restore integrity verification failed")
	ErrIncompatible = errors.New("restore is incompatible with this host")
)

type State string

const (
	StateStaging    State = "staging"
	StateStaged     State = "staged"
	StateActivating State = "activating"
	StateSucceeded  State = "succeeded"
	StateRolledBack State = "rolled_back"
	StateFailed     State = "failed"
)

type Request struct {
	BackupID string `json:"backup_id"`
}

type Metadata struct {
	ID             string    `json:"id"`
	CommandID      string    `json:"command_id"`
	BackupID       string    `json:"backup_id"`
	State          State     `json:"state"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ActivatedAt    time.Time `json:"activated_at,omitempty"`
	CompletedAt    time.Time `json:"completed_at,omitempty"`
	HostID         string    `json:"host_id"`
	SchemaVersion  int       `json:"schema_version"`
	SizeBytes      int64     `json:"size_bytes"`
	SHA256         string    `json:"sha256"`
	DatabasePath   string    `json:"-"`
	StagedPath     string    `json:"-"`
	RollbackPath   string    `json:"-"`
	HandoffPath    string    `json:"-"`
	QuickCheck     string    `json:"quick_check,omitempty"`
	RollbackReason string    `json:"rollback_reason,omitempty"`
	Error          string    `json:"error,omitempty"`
}

type BackupReader interface {
	Open(context.Context, string) (backup.Metadata, *os.File, error)
}

type Store interface {
	CreateRestore(context.Context, Metadata) error
	UpdateRestore(context.Context, Metadata) error
	Restore(context.Context, string) (Metadata, error)
	ListRestores(context.Context, int) ([]Metadata, error)
	ReconcileRestoredSnapshot(context.Context, string, time.Time) (SnapshotReconciliation, error)
}

type SnapshotReconciliation struct {
	CommandsInterrupted int
	BackupsFailed       int
}

type Validator func(context.Context, string, bool) (backup.Validation, error)
