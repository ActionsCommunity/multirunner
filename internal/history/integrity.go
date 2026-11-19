package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/GerardSmit/multirunner/internal/operations"
)

type operationalChainVerification struct {
	Epochs          int
	Events          int64
	CompactedEvents int64
}

type chainEpoch struct {
	id              string
	hostID          string
	lastSequence    int64
	throughSequence int64
	integrityHash   string
}

func rehashOperationalEventsTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, host_id, last_sequence
		FROM host_epochs ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list operational epochs for hash migration: %w", err)
	}
	var epochs []chainEpoch
	for rows.Next() {
		var epoch chainEpoch
		if err := rows.Scan(&epoch.id, &epoch.hostID, &epoch.lastSequence); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan operational epoch for hash migration: %w", err)
		}
		epochs = append(epochs, epoch)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close operational hash migration epochs: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read operational hash migration epochs: %w", err)
	}
	for _, epoch := range epochs {
		if err := rehashOperationalEpochTx(ctx, tx, epoch); err != nil {
			return err
		}
	}
	return nil
}

func rehashOperationalEpochTx(
	ctx context.Context, tx *sql.Tx, epoch chainEpoch,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT
		id, schema_version, host_id, host_epoch, sequence, event_type,
		entity_type, entity_id, timestamp, correlation_id, causation_id,
		actor_kind, actor_id, payload_json, command_id, previous_hash,
		integrity_hash
		FROM operational_events WHERE host_epoch=? ORDER BY sequence`, epoch.id)
	if err != nil {
		return fmt.Errorf("read operational chain %s for hash migration: %w", epoch.id, err)
	}
	var events []operations.Event
	for rows.Next() {
		var event operations.Event
		var timestamp int64
		var payload []byte
		if err := rows.Scan(
			&event.ID, &event.SchemaVersion, &event.HostID, &event.HostEpoch,
			&event.Sequence, &event.Type, &event.EntityType, &event.EntityID,
			&timestamp, &event.CorrelationID, &event.CausationID, &event.ActorKind,
			&event.ActorID, &payload, &event.CommandID, &event.PreviousHash,
			&event.IntegrityHash,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan operational chain %s for hash migration: %w",
				epoch.id, err)
		}
		event.Timestamp = millisTime(timestamp)
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close operational chain %s hash migration: %w", epoch.id, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read operational chain %s hash migration: %w", epoch.id, err)
	}
	if int64(len(events)) != epoch.lastSequence {
		return fmt.Errorf("operational chain %s cannot be rehashed: events=%d epoch=%d",
			epoch.id, len(events), epoch.lastSequence)
	}
	previousHash := ""
	legacyPreviousHash := ""
	for index := range events {
		event := &events[index]
		expectedSequence := int64(index + 1)
		if event.Sequence != expectedSequence ||
			event.ID != operations.EventID(epoch.id, expectedSequence) ||
			event.HostEpoch != epoch.id || event.HostID != epoch.hostID {
			return fmt.Errorf("operational chain %s cannot be rehashed at sequence %d",
				epoch.id, expectedSequence)
		}
		if event.PreviousHash != legacyPreviousHash || event.IntegrityHash == "" {
			return fmt.Errorf(
				"operational chain %s legacy linkage is invalid at sequence %d",
				epoch.id, expectedSequence)
		}
		legacyIntegrityHash := event.IntegrityHash
		event.PreviousHash = previousHash
		event.IntegrityHash = operations.IntegrityHash(previousHash, *event)
		if _, err := tx.ExecContext(ctx, `UPDATE operational_events
			SET previous_hash=?, integrity_hash=? WHERE id=?`,
			event.PreviousHash, event.IntegrityHash, event.ID); err != nil {
			return fmt.Errorf("rehash operational event %s: %w", event.ID, err)
		}
		previousHash = event.IntegrityHash
		legacyPreviousHash = legacyIntegrityHash
	}
	return nil
}

func verifyOperationalChains(
	ctx context.Context, db *sql.DB,
) (operationalChainVerification, error) {
	var verification operationalChainVerification
	var anchorsExist int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='operational_event_anchors'`).Scan(&anchorsExist); err != nil {
		return verification, fmt.Errorf("inspect operational chain anchors: %w", err)
	}
	query := `SELECT id, host_id, last_sequence, 0, '' FROM host_epochs ORDER BY id`
	if anchorsExist != 0 {
		query = `SELECT h.id, h.host_id, h.last_sequence,
			COALESCE(a.through_sequence, 0), COALESCE(a.integrity_hash, '')
			FROM host_epochs h
			LEFT JOIN operational_event_anchors a ON a.host_epoch=h.id
			ORDER BY h.id`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return verification, fmt.Errorf("list operational chain epochs: %w", err)
	}
	var epochs []chainEpoch
	for rows.Next() {
		var epoch chainEpoch
		if err := rows.Scan(
			&epoch.id, &epoch.hostID, &epoch.lastSequence,
			&epoch.throughSequence, &epoch.integrityHash,
		); err != nil {
			_ = rows.Close()
			return verification, fmt.Errorf("scan operational chain epoch: %w", err)
		}
		if epoch.throughSequence < 0 || epoch.throughSequence > epoch.lastSequence {
			_ = rows.Close()
			return verification, fmt.Errorf(
				"operational chain %s has invalid compacted sequence %d of %d",
				epoch.id, epoch.throughSequence, epoch.lastSequence)
		}
		if epoch.throughSequence > 0 && epoch.integrityHash == "" {
			_ = rows.Close()
			return verification, fmt.Errorf(
				"operational chain %s has an empty compaction hash", epoch.id)
		}
		epochs = append(epochs, epoch)
	}
	if err := rows.Close(); err != nil {
		return verification, fmt.Errorf("close operational chain epochs: %w", err)
	}
	if err := rows.Err(); err != nil {
		return verification, fmt.Errorf("read operational chain epochs: %w", err)
	}

	for _, epoch := range epochs {
		count, err := verifyOperationalEpoch(ctx, db, epoch)
		if err != nil {
			return verification, err
		}
		verification.Epochs++
		verification.Events += count
		verification.CompactedEvents += epoch.throughSequence
	}
	return verification, nil
}

func verifyOperationalEpoch(
	ctx context.Context, db *sql.DB, epoch chainEpoch,
) (int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT
		id, schema_version, host_id, host_epoch, sequence, event_type,
		entity_type, entity_id, timestamp, correlation_id, causation_id,
		actor_kind, actor_id, payload_json, command_id, previous_hash,
		integrity_hash
		FROM operational_events WHERE host_epoch=? AND sequence>?
		ORDER BY sequence`, epoch.id, epoch.throughSequence)
	if err != nil {
		return 0, fmt.Errorf("read operational chain %s: %w", epoch.id, err)
	}
	expectedSequence := epoch.throughSequence + 1
	previousHash := epoch.integrityHash
	var count int64
	for rows.Next() {
		var event operations.Event
		var timestamp int64
		var payload []byte
		if err := rows.Scan(
			&event.ID, &event.SchemaVersion, &event.HostID, &event.HostEpoch,
			&event.Sequence, &event.Type, &event.EntityType, &event.EntityID,
			&timestamp, &event.CorrelationID, &event.CausationID, &event.ActorKind,
			&event.ActorID, &payload, &event.CommandID, &event.PreviousHash,
			&event.IntegrityHash,
		); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan operational chain %s: %w", epoch.id, err)
		}
		event.Timestamp = millisTime(timestamp)
		event.Payload = json.RawMessage(payload)
		if event.Sequence != expectedSequence {
			_ = rows.Close()
			return 0, fmt.Errorf(
				"operational chain %s expected sequence %d, found %d",
				epoch.id, expectedSequence, event.Sequence)
		}
		if event.ID != operations.EventID(epoch.id, event.Sequence) ||
			event.HostEpoch != epoch.id || event.HostID != epoch.hostID {
			_ = rows.Close()
			return 0, fmt.Errorf(
				"operational chain %s event identity mismatch at sequence %d",
				epoch.id, event.Sequence)
		}
		if event.PreviousHash != previousHash {
			_ = rows.Close()
			return 0, fmt.Errorf(
				"operational chain %s previous hash mismatch at sequence %d",
				epoch.id, event.Sequence)
		}
		if operations.IntegrityHash(previousHash, event) != event.IntegrityHash {
			_ = rows.Close()
			return 0, fmt.Errorf(
				"operational chain %s integrity hash mismatch at sequence %d",
				epoch.id, event.Sequence)
		}
		previousHash = event.IntegrityHash
		expectedSequence++
		count++
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close operational chain %s: %w", epoch.id, err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read operational chain %s: %w", epoch.id, err)
	}
	if expectedSequence-1 != epoch.lastSequence {
		return 0, fmt.Errorf(
			"operational chain %s last sequence mismatch: events=%d epoch=%d",
			epoch.id, expectedSequence-1, epoch.lastSequence)
	}
	return count, nil
}
