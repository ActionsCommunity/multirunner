// Package update verifies trusted release metadata and immutable update
// artifacts. It does not download or activate executables.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

var (
	ErrIntegrity    = errors.New("update metadata integrity verification failed")
	ErrExpired      = errors.New("update metadata is expired")
	ErrRollback     = errors.New("update metadata rollback detected")
	ErrUntrusted    = errors.New("update metadata is not trusted")
	ErrIncompatible = errors.New("update is incompatible with this installation")
	ErrNotFound     = errors.New("update target not found")
	ErrConflict     = errors.New("another update is already staged")
	ErrTrustMissing = errors.New("trusted update root is not configured")
)

const SpecVersion = "1.0.31"

type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

type Envelope[T any] struct {
	Signed     T           `json:"signed"`
	Signatures []Signature `json:"signatures"`
}

type Key struct {
	KeyType string   `json:"keytype"`
	Scheme  string   `json:"scheme"`
	KeyVal  KeyValue `json:"keyval"`
}

type KeyValue struct {
	Public string `json:"public"`
}

type Role struct {
	KeyIDs    []string `json:"keyids"`
	Threshold int      `json:"threshold"`
}

type Root struct {
	Type               string          `json:"_type"`
	SpecVersion        string          `json:"spec_version"`
	Version            int             `json:"version"`
	Expires            time.Time       `json:"expires"`
	ConsistentSnapshot bool            `json:"consistent_snapshot"`
	Keys               map[string]Key  `json:"keys"`
	Roles              map[string]Role `json:"roles"`
}

type Timestamp struct {
	Type        string              `json:"_type"`
	SpecVersion string              `json:"spec_version"`
	Version     int                 `json:"version"`
	Expires     time.Time           `json:"expires"`
	Meta        map[string]FileMeta `json:"meta"`
}

type Snapshot struct {
	Type        string              `json:"_type"`
	SpecVersion string              `json:"spec_version"`
	Version     int                 `json:"version"`
	Expires     time.Time           `json:"expires"`
	Meta        map[string]FileMeta `json:"meta"`
}

type Targets struct {
	Type        string                `json:"_type"`
	SpecVersion string                `json:"spec_version"`
	Version     int                   `json:"version"`
	Expires     time.Time             `json:"expires"`
	Targets     map[string]TargetFile `json:"targets"`
}

type FileMeta struct {
	Version int               `json:"version"`
	Length  int64             `json:"length"`
	Hashes  map[string]string `json:"hashes"`
}

type TargetFile struct {
	Length int64             `json:"length"`
	Hashes map[string]string `json:"hashes"`
	Custom TargetMetadata    `json:"custom"`
}

type TargetMetadata struct {
	Version               string     `json:"version"`
	Commit                string     `json:"commit"`
	OS                    string     `json:"os"`
	Arch                  string     `json:"arch"`
	APIVersion            string     `json:"api_version"`
	SchemaMin             int        `json:"schema_min"`
	SchemaMax             int        `json:"schema_max"`
	EmbeddedConsoleSHA256 string     `json:"embedded_console_sha256"`
	SBOMSHA256            string     `json:"sbom_sha256"`
	ProvenanceSHA256      string     `json:"provenance_sha256"`
	Provenance            Provenance `json:"provenance"`
}

type Provenance struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Workflow   string `json:"workflow"`
	BuilderID  string `json:"builder_id"`
}

type MetadataBundle struct {
	RootUpdates [][]byte `json:"root_updates,omitempty"`
	Timestamp   []byte   `json:"timestamp"`
	Snapshot    []byte   `json:"snapshot"`
	Targets     []byte   `json:"targets"`
}

type TrustedState struct {
	RootVersion      int `json:"root_version"`
	TimestampVersion int `json:"timestamp_version"`
	SnapshotVersion  int `json:"snapshot_version"`
	TargetsVersion   int `json:"targets_version"`
}

type Installed struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	APIVersion    string `json:"api_version"`
	SchemaVersion int    `json:"schema_version"`
}

type Policy struct {
	AllowedTargetKeyIDs []string
	RevokedKeyIDs       []string
	Repository          string
	Workflow            string
	BuilderID           string
}

type Verified struct {
	Root        Envelope[Root]
	Timestamp   Envelope[Timestamp]
	Snapshot    Envelope[Snapshot]
	Targets     Envelope[Targets]
	TargetPath  string
	Target      TargetFile
	NextState   TrustedState
	MetadataRaw map[string]json.RawMessage
}

type State string

const (
	StateInspecting State = "inspecting"
	StateStaging    State = "staging"
	StateStaged     State = "staged"
	StateActivating State = "activating"
	StateSucceeded  State = "succeeded"
	StateRolledBack State = "rolled_back"
	StateFailed     State = "failed"
)

type Request struct {
	Version string `json:"version,omitempty"`
}

type Metadata struct {
	ID             string    `json:"id"`
	CommandID      string    `json:"command_id"`
	State          State     `json:"state"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ActivatedAt    time.Time `json:"activated_at,omitempty"`
	CompletedAt    time.Time `json:"completed_at,omitempty"`
	Version        string    `json:"version"`
	Commit         string    `json:"commit"`
	HostID         string    `json:"host_id"`
	TargetPath     string    `json:"target_path"`
	SizeBytes      int64     `json:"size_bytes"`
	SHA256         string    `json:"sha256"`
	SchemaMin      int       `json:"schema_min"`
	SchemaMax      int       `json:"schema_max"`
	APIVersion     string    `json:"api_version"`
	ArtifactPath   string    `json:"-"`
	HandoffPath    string    `json:"-"`
	RollbackReason string    `json:"rollback_reason,omitempty"`
	Error          string    `json:"error,omitempty"`
}

type Source interface {
	Bundle(context.Context, int) (MetadataBundle, error)
	OpenTarget(context.Context, string, TargetFile) (io.ReadCloser, error)
}

type Store interface {
	CreateUpdate(context.Context, Metadata) error
	UpdateUpdate(context.Context, Metadata) error
	Update(context.Context, string) (Metadata, error)
	ListUpdates(context.Context, int) ([]Metadata, error)
}
