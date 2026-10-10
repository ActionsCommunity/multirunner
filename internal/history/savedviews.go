package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

var (
	ErrSavedViewNotFound            = errors.New("saved view not found")
	ErrSavedViewConflict            = errors.New("saved view version conflict")
	ErrSavedViewIdempotencyConflict = errors.New("saved view idempotency conflict")
	ErrSavedViewNameConflict        = errors.New("saved view name conflict")
)

type SavedViewInput struct {
	Name       string `json:"name"`
	Query      string `json:"query"`
	EntityType string `json:"entity_type,omitempty"`
	Repository string `json:"repository,omitempty"`
	State      string `json:"state,omitempty"`
}

type SavedView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Query      string    `json:"query"`
	EntityType string    `json:"entity_type,omitempty"`
	Repository string    `json:"repository,omitempty"`
	State      string    `json:"state,omitempty"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Version    int64     `json:"version"`
}

func (s *Store) ListSavedViews(ctx context.Context) ([]SavedView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, query, entity_type,
		repository, state, created_by, created_at, updated_at, version
		FROM saved_views ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("list saved views: %w", err)
	}
	defer rows.Close()
	result := make([]SavedView, 0)
	for rows.Next() {
		view, err := scanSavedView(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list saved views: %w", err)
	}
	return result, nil
}

func (s *Store) CreateSavedView(
	ctx context.Context, idempotencyKey, actorID string, input SavedViewInput,
) (SavedView, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	actorID = strings.TrimSpace(actorID)
	input.Name = strings.TrimSpace(input.Name)
	input.Query = strings.TrimSpace(input.Query)
	input.EntityType = strings.TrimSpace(input.EntityType)
	input.Repository = strings.TrimSpace(input.Repository)
	input.State = strings.TrimSpace(input.State)
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return SavedView{}, false, errors.New("idempotency key must contain 1 to 128 characters")
	}
	if actorID == "" {
		return SavedView{}, false, errors.New("saved view actor is required")
	}
	if input.Name == "" || len(input.Name) > 100 {
		return SavedView{}, false, errors.New("saved view name must contain 1 to 100 characters")
	}
	if _, err := searchExpression(input.Query); err != nil {
		return SavedView{}, false, fmt.Errorf("%w: %v", ErrInvalidSearch, err)
	}
	if input.EntityType != "" && !validSearchEntityType(input.EntityType) {
		return SavedView{}, false, fmt.Errorf("%w: unsupported search entity type %q",
			ErrInvalidSearch, input.EntityType)
	}
	requestJSON, _ := json.Marshal(input)
	sum := sha256.Sum256(requestJSON)
	requestHash := hex.EncodeToString(sum[:])
	var existingHash string
	existing, err := s.savedViewByIdempotencyKey(ctx, idempotencyKey, &existingHash)
	if err == nil {
		if existingHash != requestHash {
			return SavedView{}, false, ErrSavedViewIdempotencyConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrSavedViewNotFound) {
		return SavedView{}, false, err
	}
	id, err := operations.NewOpaqueID()
	if err != nil {
		return SavedView{}, false, err
	}
	now := nowUTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO saved_views (
		id, idempotency_key, request_hash, name, query, entity_type, repository,
		state, created_by, created_at, updated_at, version
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		id, idempotencyKey, requestHash, input.Name, input.Query, input.EntityType,
		input.Repository, input.State, actorID, timeMillis(now), timeMillis(now))
	if err != nil {
		if isSQLiteConstraint(err) {
			existing, lookupErr := s.savedViewByIdempotencyKey(
				ctx, idempotencyKey, &existingHash,
			)
			if lookupErr == nil {
				if existingHash != requestHash {
					return SavedView{}, false, ErrSavedViewIdempotencyConflict
				}
				return existing, false, nil
			}
			if !errors.Is(lookupErr, ErrSavedViewNotFound) {
				return SavedView{}, false, lookupErr
			}
			return SavedView{}, false, ErrSavedViewNameConflict
		}
		return SavedView{}, false, fmt.Errorf("create saved view: %w", err)
	}
	return SavedView{
		ID: id, Name: input.Name, Query: input.Query, EntityType: input.EntityType,
		Repository: input.Repository, State: input.State, CreatedBy: actorID,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}, true, nil
}

func (s *Store) DeleteSavedView(
	ctx context.Context, id, idempotencyKey string, version int64,
) (bool, error) {
	id = strings.TrimSpace(id)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if id == "" || idempotencyKey == "" || version < 1 {
		return false, errors.New("saved view ID, idempotency key, and version are required")
	}
	var deletedID string
	var deletedVersion int64
	err := s.db.QueryRowContext(ctx, `SELECT saved_view_id, version
		FROM saved_view_deletions WHERE idempotency_key=?`, idempotencyKey).
		Scan(&deletedID, &deletedVersion)
	if err == nil {
		if deletedID != id || deletedVersion != version {
			return false, ErrSavedViewIdempotencyConflict
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read saved view deletion: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin saved view deletion: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM saved_views WHERE id=? AND version=?`, id, version)
	if err != nil {
		return false, fmt.Errorf("delete saved view: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read saved view deletion result: %w", err)
	}
	if affected == 0 {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM saved_views WHERE id=?`, id).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrSavedViewNotFound
		}
		if err != nil {
			return false, fmt.Errorf("read saved view version: %w", err)
		}
		return false, ErrSavedViewConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO saved_view_deletions (
		idempotency_key, saved_view_id, version, deleted_at
	) VALUES (?, ?, ?, ?)`, idempotencyKey, id, version, timeMillis(nowUTC())); err != nil {
		return false, fmt.Errorf("record saved view deletion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit saved view deletion: %w", err)
	}
	return true, nil
}

func (s *Store) savedViewByIdempotencyKey(
	ctx context.Context, key string, requestHash *string,
) (SavedView, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, query, entity_type,
		repository, state, created_by, created_at, updated_at, version, request_hash
		FROM saved_views WHERE idempotency_key=?`, key)
	var view SavedView
	var createdAt, updatedAt int64
	err := row.Scan(
		&view.ID, &view.Name, &view.Query, &view.EntityType, &view.Repository,
		&view.State, &view.CreatedBy, &createdAt, &updatedAt, &view.Version, requestHash,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SavedView{}, ErrSavedViewNotFound
	}
	if err != nil {
		return SavedView{}, fmt.Errorf("read saved view: %w", err)
	}
	view.CreatedAt = millisTime(createdAt)
	view.UpdatedAt = millisTime(updatedAt)
	return view, nil
}

type savedViewScanner interface {
	Scan(...any) error
}

func scanSavedView(scanner savedViewScanner) (SavedView, error) {
	var view SavedView
	var createdAt, updatedAt int64
	if err := scanner.Scan(
		&view.ID, &view.Name, &view.Query, &view.EntityType, &view.Repository,
		&view.State, &view.CreatedBy, &createdAt, &updatedAt, &view.Version,
	); err != nil {
		return SavedView{}, fmt.Errorf("scan saved view: %w", err)
	}
	view.CreatedAt = millisTime(createdAt)
	view.UpdatedAt = millisTime(updatedAt)
	return view, nil
}
