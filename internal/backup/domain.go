// Package backup creates and verifies durable online history database backups.
package backup

import (
	"context"
	"errors"
	"os"
	"time"
)

var (
	ErrNotFound          = errors.New("backup not found")
	ErrInsufficientSpace = errors.New("insufficient disk space for backup")
	ErrIntegrity         = errors.New("backup integrity verification failed")
	ErrIncompatible      = errors.New("backup is incompatible with this host")
)

type State string

const (
	StateCreating  State = "creating"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
)

type Purpose string

const (
	PurposeManual     Purpose = "manual"
	PurposePreRestore Purpose = "pre_restore"
	PurposePreUpdate  Purpose = "pre_update"
)

type Request struct {
	Purpose Purpose `json:"purpose"`
}

type Snapshot struct {
	SchemaVersion      int
	HostID             string
	AuditEventID       string
	AuditIntegrityHash string
	DatabaseBytes      int64
	WALBytes           int64
}

type Validation struct {
	QuickCheck      string `json:"quick_check"`
	ForeignKeyCount int    `json:"foreign_key_count"`
	SchemaVersion   int    `json:"schema_version"`
	HostID          string `json:"host_id"`
	VerifiedEpochs  int    `json:"verified_epochs"`
	VerifiedEvents  int64  `json:"verified_events"`
	CompactedEvents int64  `json:"compacted_events"`
}

type Metadata struct {
	ID                 string    `json:"id"`
	CommandID          string    `json:"command_id"`
	Purpose            Purpose   `json:"purpose"`
	State              State     `json:"state"`
	CreatedAt          time.Time `json:"created_at"`
	CompletedAt        time.Time `json:"completed_at,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
	FileName           string    `json:"file_name,omitempty"`
	FilePath           string    `json:"-"`
	ManifestPath       string    `json:"-"`
	SizeBytes          int64     `json:"size_bytes,omitempty"`
	SHA256             string    `json:"sha256,omitempty"`
	SchemaVersion      int       `json:"schema_version,omitempty"`
	AppVersion         string    `json:"app_version,omitempty"`
	HostID             string    `json:"host_id,omitempty"`
	AuditEventID       string    `json:"audit_event_id,omitempty"`
	AuditIntegrityHash string    `json:"audit_integrity_hash,omitempty"`
	QuickCheck         string    `json:"quick_check,omitempty"`
	Error              string    `json:"error,omitempty"`
	DownloadURL        string    `json:"download_url,omitempty"`
}

type Source interface {
	OnlineBackup(context.Context, string) error
	BackupSnapshot(context.Context, string) (Snapshot, error)
}

type Store interface {
	CreateBackup(context.Context, Metadata) error
	CompleteBackup(context.Context, Metadata) error
	FailBackup(context.Context, string, string, time.Time) error
	Backup(context.Context, string) (Metadata, error)
	ListBackups(context.Context, int) ([]Metadata, error)
	PruneBackups(context.Context, time.Time) ([]Metadata, error)
}

type Validator func(context.Context, string, bool) (Validation, error)
type FreeSpace func(string) (available, total uint64, err error)

type Reader interface {
	List(context.Context, int) ([]Metadata, error)
	Open(context.Context, string) (Metadata, *os.File, error)
}
