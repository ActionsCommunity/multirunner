package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var restoreIDPattern = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9_-]{0,127}\z`)

type ActivationOptions struct {
	DatabasePath         string
	CurrentSchemaVersion int
	Validate             Validator
	HandoffKey           []byte
	Now                  func() time.Time
}

func ActivatePending(ctx context.Context, options ActivationOptions) (Handoff, bool, error) {
	return activatePendingWithHooks(ctx, options, activationHooks{})
}

type activationHooks struct {
	afterPhase        func(ActivationPhase) error
	afterRollbackMove func(string) error
	afterDatabaseMove func() error
	rename            func(string, string) error
	rollback          func(context.Context, string, Handoff, ActivationOptions) error
}

func activatePendingWithHooks(
	ctx context.Context,
	options ActivationOptions,
	hooks activationHooks,
) (Handoff, bool, error) {
	root, databasePath, handoffPath, err := activationPaths(options.DatabasePath)
	if err != nil {
		return Handoff{}, false, err
	}
	handoff, err := readHandoff(handoffPath, options.HandoffKey)
	if errors.Is(err, os.ErrNotExist) {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, err
	}
	if handoff.State != StateStaged {
		return handoff, false, nil
	}
	if options.Validate == nil || options.CurrentSchemaVersion < 1 {
		return Handoff{}, false, errors.New("restore activation validator and schema version are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	expectedStagedPath, expectedRollbackPath, err := derivedArtifactPaths(root, handoff.ID)
	if err != nil {
		return Handoff{}, false, err
	}
	if !samePath(handoff.DatabasePath, databasePath) ||
		!samePath(handoff.StagedPath, expectedStagedPath) ||
		!samePath(handoff.RollbackPath, expectedRollbackPath) {
		return Handoff{}, false, ErrIntegrity
	}
	handoff.DatabasePath = databasePath
	handoff.StagedPath = expectedStagedPath
	handoff.RollbackPath = expectedRollbackPath
	staged, err := openVerifiedRegularFile(handoff.StagedPath)
	if err != nil {
		return Handoff{}, false, err
	}
	defer staged.Close()
	size, checksum, err := digestOpenFile(ctx, staged)
	if err != nil {
		return Handoff{}, false, err
	}
	if size != handoff.SizeBytes || !strings.EqualFold(checksum, handoff.SHA256) {
		return Handoff{}, false, ErrIntegrity
	}
	validation, err := options.Validate(ctx, handoff.StagedPath, false)
	if err != nil {
		return Handoff{}, false, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if validation.QuickCheck != "ok" || validation.ForeignKeyCount != 0 ||
		validation.HostID != handoff.HostID ||
		validation.SchemaVersion != handoff.SchemaVersion ||
		validation.SchemaVersion > options.CurrentSchemaVersion {
		return Handoff{}, false, ErrIncompatible
	}
	if err := confirmPathIdentity(staged, handoff.StagedPath); err != nil {
		return Handoff{}, false, err
	}
	handoff.RollbackFiles, err = snapshotRollbackFiles(ctx, databasePath)
	if err != nil {
		return Handoff{}, false, err
	}
	rollbackValidation, err := options.Validate(ctx, databasePath, false)
	if err != nil || rollbackValidation.QuickCheck != "ok" ||
		rollbackValidation.ForeignKeyCount != 0 {
		return Handoff{}, false, fmt.Errorf("%w: current database is not safe to roll back", ErrIntegrity)
	}
	handoff.RollbackHostID = rollbackValidation.HostID
	handoff.RollbackSchemaVersion = rollbackValidation.SchemaVersion
	handoff.State = StateActivating
	if err := persistActivationPhase(
		handoffPath, &handoff, ActivationPhasePrepared, options,
	); err != nil {
		return Handoff{}, false, err
	}
	if err := runActivationPhaseHook(hooks, handoff.ActivationPhase); err != nil {
		return Handoff{}, false, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := moveIfExists(databasePath+suffix, handoff.RollbackPath+suffix); err != nil {
			return activationFailure(ctx, databasePath, handoff, options, hooks, err)
		}
		if hooks.afterRollbackMove != nil {
			if err := hooks.afterRollbackMove(suffix); err != nil {
				return Handoff{}, false, err
			}
		}
	}
	if err := persistActivationPhase(
		handoffPath, &handoff, ActivationPhaseRollbackStaged, options,
	); err != nil {
		return activationFailure(ctx, databasePath, handoff, options, hooks, err)
	}
	if err := runActivationPhaseHook(hooks, handoff.ActivationPhase); err != nil {
		return Handoff{}, false, err
	}
	rename := hooks.rename
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(handoff.StagedPath, databasePath); err != nil {
		return activationFailure(
			ctx, databasePath, handoff, options, hooks,
			fmt.Errorf("activate restore database: %w", err),
		)
	}
	if hooks.afterDatabaseMove != nil {
		if err := hooks.afterDatabaseMove(); err != nil {
			return Handoff{}, false, err
		}
	}
	if err := persistActivationPhase(
		handoffPath, &handoff, ActivationPhaseDatabaseActivated, options,
	); err != nil {
		return activationFailure(ctx, databasePath, handoff, options, hooks, err)
	}
	if err := runActivationPhaseHook(hooks, handoff.ActivationPhase); err != nil {
		return Handoff{}, false, err
	}
	if err := verifyActivatedDatabase(ctx, databasePath, handoff, options); err != nil {
		return activationFailure(ctx, databasePath, handoff, options, hooks, err)
	}
	handoff.ActivatedAt = options.Now().UTC()
	if err := persistActivationPhase(
		handoffPath, &handoff, ActivationPhaseReady, options,
	); err != nil {
		return activationFailure(ctx, databasePath, handoff, options, hooks, err)
	}
	if err := runActivationPhaseHook(hooks, handoff.ActivationPhase); err != nil {
		return Handoff{}, false, err
	}
	return handoff, true, nil
}

func CommitHealthy(
	ctx context.Context, databasePath string, handoffKey []byte,
	store Store, now time.Time,
) (Handoff, bool, error) {
	_, _, handoffPath, err := activationPaths(databasePath)
	if err != nil {
		return Handoff{}, false, err
	}
	handoff, err := readHandoff(handoffPath, handoffKey)
	if errors.Is(err, os.ErrNotExist) {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, err
	}
	if handoff.State != StateActivating &&
		handoff.State != StateRolledBack &&
		handoff.State != StateFailed {
		return handoff, false, nil
	}
	if handoff.State == StateActivating {
		if !activationReady(handoff) {
			return Handoff{}, false, ErrIntegrity
		}
		if _, err := store.ReconcileRestoredSnapshot(ctx, handoff.ID, now); err != nil {
			return Handoff{}, false, fmt.Errorf("reconcile restored snapshot: %w", err)
		}
		handoff.State = StateSucceeded
		handoff.CompletedAt = now.UTC()
		handoff.UpdatedAt = handoff.CompletedAt
	}
	metadata := handoff.Metadata()
	metadata.HandoffPath = handoffPath
	if err := store.UpdateRestore(ctx, metadata); errors.Is(err, ErrNotFound) {
		if err := store.CreateRestore(ctx, metadata); err != nil {
			return Handoff{}, false, err
		}
	} else if err != nil {
		return Handoff{}, false, err
	}
	if err := writeHandoff(handoffPath, handoff, handoffKey, false); err != nil {
		return Handoff{}, false, err
	}
	_ = os.Rename(
		handoffPath,
		filepath.Join(filepath.Dir(handoffPath), handoff.ID+"."+string(handoff.State)+".json"),
	)
	return handoff, true, nil
}

func RollbackPending(
	ctx context.Context, options ActivationOptions, reason string, now time.Time,
) (Handoff, bool, error) {
	root, resolvedDatabase, handoffPath, err := activationPaths(options.DatabasePath)
	if err != nil {
		return Handoff{}, false, err
	}
	handoff, err := readHandoff(handoffPath, options.HandoffKey)
	if errors.Is(err, os.ErrNotExist) {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, err
	}
	if handoff.State != StateActivating {
		return handoff, false, nil
	}
	if !validActivationPhase(handoff.ActivationPhase) {
		return Handoff{}, false, ErrIntegrity
	}
	_, expectedRollbackPath, err := derivedArtifactPaths(root, handoff.ID)
	if err != nil || !samePath(handoff.DatabasePath, resolvedDatabase) ||
		!samePath(handoff.RollbackPath, expectedRollbackPath) {
		return Handoff{}, false, ErrIntegrity
	}
	handoff.DatabasePath = resolvedDatabase
	handoff.RollbackPath = expectedRollbackPath
	handles, err := openRollbackFiles(ctx, resolvedDatabase, handoff)
	if err != nil {
		return Handoff{}, false, err
	}
	defer closeRollbackHandles(handles)
	if err := restoreRollbackFiles(
		resolvedDatabase, handoff.RollbackPath, handles,
	); err != nil {
		return Handoff{}, false, err
	}
	if err := verifyRestoredRollback(ctx, resolvedDatabase, handoff, options); err != nil {
		return Handoff{}, false, err
	}
	handoff.State = StateRolledBack
	handoff.CompletedAt = now.UTC()
	handoff.UpdatedAt = handoff.CompletedAt
	handoff.RollbackReason = boundedError(errors.New(reason))
	if err := writeHandoff(handoffPath, handoff, options.HandoffKey, false); err != nil {
		return Handoff{}, false, err
	}
	return handoff, true, nil
}

func activationPaths(databasePath string) (string, string, string, error) {
	if strings.TrimSpace(databasePath) == "" {
		return "", "", "", errors.New("restore database path is required")
	}
	resolved, err := filepath.Abs(databasePath)
	if err != nil {
		return "", "", "", err
	}
	resolved = filepath.Clean(resolved)
	root := resolved + ".restore"
	return root, resolved, activePath(root), nil
}

func moveIfExists(source, destination string) error {
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale rollback file: %w", err)
	}
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("stage rollback file: %w", err)
	}
	return nil
}

func persistActivationPhase(
	handoffPath string,
	handoff *Handoff,
	phase ActivationPhase,
	options ActivationOptions,
) error {
	handoff.ActivationPhase = phase
	handoff.UpdatedAt = options.Now().UTC()
	return writeHandoff(handoffPath, *handoff, options.HandoffKey, false)
}

func runActivationPhaseHook(hooks activationHooks, phase ActivationPhase) error {
	if hooks.afterPhase == nil {
		return nil
	}
	return hooks.afterPhase(phase)
}

func activationFailure(
	ctx context.Context,
	databasePath string,
	handoff Handoff,
	options ActivationOptions,
	hooks activationHooks,
	cause error,
) (Handoff, bool, error) {
	rollback := hooks.rollback
	if rollback == nil {
		rollback = rollbackActivationFailure
	}
	if rollbackErr := rollback(ctx, databasePath, handoff, options); rollbackErr != nil {
		cause = errors.Join(
			cause,
			fmt.Errorf("roll back failed restore activation: %w", rollbackErr),
		)
	}
	return Handoff{}, false, cause
}

func validActivationPhase(phase ActivationPhase) bool {
	switch phase {
	case "", ActivationPhasePrepared, ActivationPhaseRollbackStaged,
		ActivationPhaseDatabaseActivated, ActivationPhaseReady:
		return true
	default:
		return false
	}
}

func activationReady(handoff Handoff) bool {
	return handoff.ActivationPhase == ActivationPhaseReady ||
		(handoff.ActivationPhase == "" && !handoff.ActivatedAt.IsZero())
}

func restoreRollbackFiles(
	databasePath, rollbackPath string, handles map[string]*os.File,
) error {
	var result error
	for _, suffix := range []string{"", "-wal", "-shm"} {
		handle, expected := handles[suffix]
		if !expected {
			continue
		}

		source := rollbackPath + suffix
		if err := confirmPathIdentity(handle, source); err != nil {
			result = errors.Join(result, err)
			continue
		}
		if err := os.Remove(databasePath + suffix); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
			continue
		}
		if err := os.Rename(source, databasePath+suffix); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func rollbackActivationFailure(
	ctx context.Context, databasePath string, handoff Handoff,
	options ActivationOptions,
) error {
	handles, err := openRollbackFiles(ctx, databasePath, handoff)
	if err != nil {
		return err
	}
	defer closeRollbackHandles(handles)
	if err := restoreRollbackFiles(databasePath, handoff.RollbackPath, handles); err != nil {
		return err
	}
	return verifyRestoredRollback(ctx, databasePath, handoff, options)
}

func derivedArtifactPaths(root, id string) (string, string, error) {
	if !restoreIDPattern.MatchString(id) {
		return "", "", ErrIntegrity
	}
	return filepath.Join(root, id+".staged.db"),
		filepath.Join(root, id+".rollback.db"), nil
}

func openVerifiedRegularFile(path string) (*os.File, error) {
	file, err := openSecureRegularFile(path)
	if err != nil {
		return nil, err
	}
	if err := confirmPathIdentity(file, path); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func confirmPathIdentity(file *os.File, path string) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, current) {
		return ErrIntegrity
	}
	return nil
}

func digestOpenFile(ctx context.Context, file *os.File) (int64, string, error) {
	if _, err := file.Seek(0, 0); err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	size, err := copyContext(ctx, hash, file)
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyActivatedDatabase(
	ctx context.Context, databasePath string, handoff Handoff,
	options ActivationOptions,
) error {
	file, err := openVerifiedRegularFile(databasePath)
	if err != nil {
		return err
	}
	defer file.Close()
	size, checksum, err := digestOpenFile(ctx, file)
	if err != nil {
		return err
	}
	if size != handoff.SizeBytes || !strings.EqualFold(checksum, handoff.SHA256) {
		return ErrIntegrity
	}
	validation, err := options.Validate(ctx, databasePath, false)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if validation.QuickCheck != "ok" || validation.ForeignKeyCount != 0 ||
		validation.HostID != handoff.HostID ||
		validation.SchemaVersion != handoff.SchemaVersion {
		return ErrIncompatible
	}
	return confirmPathIdentity(file, databasePath)
}

func snapshotRollbackFiles(
	ctx context.Context, databasePath string,
) ([]ArtifactDigest, error) {
	var artifacts []ArtifactDigest
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := databasePath + suffix
		file, err := openVerifiedRegularFile(path)
		if errors.Is(err, os.ErrNotExist) {
			if suffix == "" {
				return nil, fmt.Errorf("current history database is missing")
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		size, checksum, digestErr := digestOpenFile(ctx, file)
		closeErr := file.Close()
		if digestErr != nil || closeErr != nil {
			return nil, errors.Join(digestErr, closeErr)
		}
		artifacts = append(artifacts, ArtifactDigest{
			Suffix: suffix, SizeBytes: size, SHA256: checksum,
		})
	}
	return artifacts, nil
}

func openRollbackFiles(
	ctx context.Context, databasePath string, handoff Handoff,
) (map[string]*os.File, error) {
	expected := make(map[string]ArtifactDigest, len(handoff.RollbackFiles))
	for _, artifact := range handoff.RollbackFiles {
		switch artifact.Suffix {
		case "", "-wal", "-shm":
			expected[artifact.Suffix] = artifact
		default:
			return nil, ErrIntegrity
		}
	}
	if _, ok := expected[""]; !ok {
		return nil, ErrIntegrity
	}
	handles := make(map[string]*os.File, len(expected))
	for _, suffix := range []string{"", "-wal", "-shm"} {
		artifact, signed := expected[suffix]
		rollbackPath := handoff.RollbackPath + suffix
		_, statErr := os.Lstat(rollbackPath)
		if !signed {
			if statErr == nil {
				closeRollbackHandles(handles)
				return nil, ErrIntegrity
			}
			if !errors.Is(statErr, os.ErrNotExist) {
				closeRollbackHandles(handles)
				return nil, statErr
			}
			if err := removeUnsignedTargetSidecar(databasePath + suffix); err != nil {
				closeRollbackHandles(handles)
				return nil, err
			}
			continue
		}
		path := rollbackPath
		fromRollback := statErr == nil
		if errors.Is(statErr, os.ErrNotExist) {
			path = databasePath + suffix
		} else if statErr != nil {
			closeRollbackHandles(handles)
			return nil, statErr
		}
		file, err := openVerifiedRegularFile(path)
		if err != nil {
			closeRollbackHandles(handles)
			return nil, err
		}
		size, checksum, digestErr := digestOpenFile(ctx, file)
		if digestErr != nil {
			_ = file.Close()
			closeRollbackHandles(handles)
			return nil, digestErr
		}
		if size != artifact.SizeBytes ||
			!strings.EqualFold(checksum, artifact.SHA256) {
			_ = file.Close()
			closeRollbackHandles(handles)
			return nil, ErrIntegrity
		}
		if fromRollback {
			handles[suffix] = file
		} else if err := file.Close(); err != nil {
			closeRollbackHandles(handles)
			return nil, err
		}
	}
	return handles, nil
}

func removeUnsignedTargetSidecar(path string) error {
	file, err := openVerifiedRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := confirmPathIdentity(file, path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return ErrIntegrity
		}
		return err
	}
	return nil
}

func closeRollbackHandles(handles map[string]*os.File) {
	for _, file := range handles {
		_ = file.Close()
	}
}

func verifyRestoredRollback(
	ctx context.Context, databasePath string, handoff Handoff,
	options ActivationOptions,
) error {
	for _, artifact := range handoff.RollbackFiles {
		file, err := openVerifiedRegularFile(databasePath + artifact.Suffix)
		if err != nil {
			return err
		}
		size, checksum, digestErr := digestOpenFile(ctx, file)
		closeErr := file.Close()
		if digestErr != nil || closeErr != nil {
			return errors.Join(digestErr, closeErr)
		}
		if size != artifact.SizeBytes ||
			!strings.EqualFold(checksum, artifact.SHA256) {
			return ErrIntegrity
		}
	}
	if options.Validate == nil {
		return errors.New("restore rollback validator is required")
	}
	validation, err := options.Validate(ctx, databasePath, false)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if validation.QuickCheck != "ok" || validation.ForeignKeyCount != 0 ||
		validation.HostID != handoff.RollbackHostID ||
		validation.SchemaVersion != handoff.RollbackSchemaVersion {
		return ErrIncompatible
	}
	return nil
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil &&
		strings.EqualFold(filepath.Clean(leftAbs), filepath.Clean(rightAbs))
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative)
}
