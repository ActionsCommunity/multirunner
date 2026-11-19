package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/backup"
)

type ActivationOptions struct {
	Root             string
	FallbackPath     string
	HandoffKey       []byte
	TrustedRoot      []byte
	Installed        Installed
	Policy           Policy
	Now              func() time.Time
	DatabasePath     string
	BackupDatabase   func(context.Context, string) (backup.Validation, error)
	RestoreDatabase  func(context.Context, *os.File, string) error
	ValidateDatabase backup.Validator
}

func ResolveWorker(options ActivationOptions) (string, error) {
	identity, err := resolveWorker(options)
	return identity.path, err
}

func OpenWorker(options ActivationOptions, selectedPath string) (*os.File, bool, error) {
	root, fallback, err := activationPaths(options.Root, options.FallbackPath)
	if err != nil {
		return nil, false, err
	}
	if samePath(selectedPath, fallback) {
		return nil, false, nil
	}
	if handoff, err := readHandoff(activePath(root), options.HandoffKey); err == nil {
		switch {
		case handoff.State == StateActivating && samePath(selectedPath, handoff.ArtifactPath):
			if _, err := verifyHandoffEvidence(
				options, handoff, options.Installed, handoff.ActivatedAt,
			); err != nil {
				return nil, false, err
			}
			file, err := OpenVerifiedArtifact(
				handoff.ArtifactPath, handoff.SizeBytes, handoff.SHA256,
			)
			return file, true, err
		case handoff.State == StateRolledBack && handoff.RollbackCurrent != nil &&
			samePath(selectedPath, handoff.RollbackCurrent.ArtifactPath):
			if err := verifyCurrentEvidence(
				options, *handoff.RollbackCurrent, time.Now().UTC(),
			); err != nil {
				return nil, false, err
			}
			file, err := OpenVerifiedArtifact(
				handoff.RollbackCurrent.ArtifactPath,
				handoff.RollbackCurrent.SizeBytes,
				handoff.RollbackCurrent.SHA256,
			)
			return file, true, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	current, ok, err := readCurrent(currentPath(root), options.HandoffKey)
	if err != nil {
		return nil, false, err
	}
	if !ok || !samePath(selectedPath, current.ArtifactPath) {
		return nil, false, ErrIntegrity
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if err := verifyCurrentEvidence(options, current, options.Now().UTC()); err != nil {
		return nil, false, err
	}
	file, err := OpenVerifiedArtifact(
		current.ArtifactPath, current.SizeBytes, current.SHA256,
	)
	return file, true, err
}

type workerIdentity struct {
	path      string
	updateID  string
	sizeBytes int64
	sha256    string
	managed   bool
	current   Current
}

func resolveWorker(options ActivationOptions) (workerIdentity, error) {
	root, fallback, err := activationPaths(options.Root, options.FallbackPath)
	if err != nil {
		return workerIdentity{}, err
	}
	current, ok, err := readCurrent(currentPath(root), options.HandoffKey)
	if errors.Is(err, os.ErrNotExist) {
		return workerIdentity{path: fallback}, nil
	}
	if err != nil {
		return workerIdentity{}, err
	}
	if !ok {
		return workerIdentity{path: fallback}, nil
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if err := verifyCurrentEvidence(options, current, options.Now().UTC()); err != nil {
		return workerIdentity{}, err
	}
	expected, err := derivedArtifactPath(root, current.UpdateID, platformForPath(current.ArtifactPath))
	if err != nil || !samePath(expected, current.ArtifactPath) {
		return workerIdentity{}, ErrIntegrity
	}
	if err := verifyArtifactFile(current.ArtifactPath, current.SizeBytes, current.SHA256); err != nil {
		return workerIdentity{}, err
	}
	return workerIdentity{
		path: current.ArtifactPath, updateID: current.UpdateID,
		sizeBytes: current.SizeBytes, sha256: current.SHA256, managed: true,
		current: current,
	}, nil
}

func ActivatePending(ctx context.Context, options ActivationOptions) (Handoff, string, bool, error) {
	root, fallback, err := activationPaths(options.Root, options.FallbackPath)
	if err != nil {
		return Handoff{}, "", false, err
	}
	handoff, err := readHandoff(activePath(root), options.HandoffKey)
	if errors.Is(err, os.ErrNotExist) {
		worker, resolveErr := ResolveWorker(options)
		return Handoff{}, worker, false, resolveErr
	}
	if err != nil {
		return Handoff{}, "", false, err
	}
	if handoff.State != StateStaged {
		worker, resolveErr := ResolveWorker(options)
		return handoff, worker, false, resolveErr
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	rollback, err := resolveWorker(options)
	if err != nil {
		return Handoff{}, "", false, err
	}
	installed := options.Installed
	if rollback.managed {
		installed.Version = rollback.current.VersionName
		installed.Commit = rollback.current.Commit
	}
	var databaseState backup.Validation
	if options.DatabasePath != "" {
		if options.ValidateDatabase == nil {
			return Handoff{}, "", false, errors.New("update database validator is required")
		}
		databaseState, err = options.ValidateDatabase(ctx, options.DatabasePath, false)
		if err != nil {
			return Handoff{}, "", false, fmt.Errorf("validate pre-update database: %w", err)
		}
		if databaseState.QuickCheck != "ok" ||
			databaseState.ForeignKeyCount != 0 ||
			databaseState.SchemaVersion < 1 ||
			databaseState.HostID == "" {
			return Handoff{}, "", false, ErrIntegrity
		}
		installed.SchemaVersion = databaseState.SchemaVersion
	}
	if _, err := verifyHandoffEvidence(options, handoff, installed, options.Now().UTC()); err != nil {
		return Handoff{}, "", false, err
	}
	expected, err := derivedArtifactPath(root, handoff.ID, platformForPath(handoff.ArtifactPath))
	if err != nil || !samePath(expected, handoff.ArtifactPath) {
		return Handoff{}, "", false, ErrIntegrity
	}
	if err := verifyArtifactFile(handoff.ArtifactPath, handoff.SizeBytes, handoff.SHA256); err != nil {
		return Handoff{}, "", false, err
	}
	if rollback.path == "" {
		rollback.path = fallback
	}
	handoff.RollbackPath = rollback.path
	handoff.RollbackUpdateID = rollback.updateID
	handoff.RollbackSizeBytes = rollback.sizeBytes
	handoff.RollbackSHA256 = rollback.sha256
	handoff.PreUpdateInstalled = installed
	if rollback.managed {
		current := rollback.current
		handoff.RollbackCurrent = &current
	}
	if options.DatabasePath != "" {
		if err := prepareDatabaseBackup(
			ctx, root, &handoff, options, installed, databaseState,
		); err != nil {
			return Handoff{}, "", false, err
		}
	}
	handoff.State = StateActivating
	handoff.ActivatedAt = options.Now().UTC()
	handoff.UpdatedAt = handoff.ActivatedAt
	if err := writeSignedJSON(activePath(root), &handoff, options.HandoffKey, false); err != nil {
		return Handoff{}, "", false, err
	}
	return handoff, handoff.ArtifactPath, true, nil
}

func RollbackPending(
	options ActivationOptions,
	reason string,
	now time.Time,
) (Handoff, string, bool, error) {
	root, _, err := activationPaths(options.Root, options.FallbackPath)
	if err != nil {
		return Handoff{}, "", false, err
	}
	handoff, err := readHandoff(activePath(root), options.HandoffKey)
	if errors.Is(err, os.ErrNotExist) {
		worker, resolveErr := ResolveWorker(options)
		return Handoff{}, worker, false, resolveErr
	}
	if err != nil {
		return Handoff{}, "", false, err
	}
	if handoff.State != StateActivating {
		worker, resolveErr := ResolveWorker(options)
		return handoff, worker, false, resolveErr
	}
	if handoff.RollbackCurrent != nil {
		if err := verifyCurrentEvidence(options, *handoff.RollbackCurrent, now.UTC()); err != nil {
			return Handoff{}, "", false, err
		}
		if handoff.RollbackUpdateID != handoff.RollbackCurrent.UpdateID ||
			handoff.RollbackSizeBytes != handoff.RollbackCurrent.SizeBytes ||
			!strings.EqualFold(handoff.RollbackSHA256, handoff.RollbackCurrent.SHA256) ||
			!samePath(handoff.RollbackPath, handoff.RollbackCurrent.ArtifactPath) {
			return Handoff{}, "", false, ErrIntegrity
		}
	} else if !samePath(handoff.RollbackPath, options.FallbackPath) {
		return Handoff{}, "", false, ErrIntegrity
	} else if err := verifyExecutablePath(handoff.RollbackPath); err != nil {
		return Handoff{}, "", false, err
	}
	if err := verifyArtifactFile(
		handoff.RollbackPath, handoff.RollbackSizeBytes, handoff.RollbackSHA256,
	); err != nil && handoff.RollbackCurrent != nil {
		return Handoff{}, "", false, err
	}
	if err := restoreDatabaseForRollback(context.Background(), root, handoff, options); err != nil {
		return Handoff{}, "", false, err
	}
	handoff.State = StateRolledBack
	handoff.CompletedAt = now.UTC()
	handoff.UpdatedAt = handoff.CompletedAt
	handoff.RollbackReason = boundedError(errors.New(reason))
	if err := writeSignedJSON(activePath(root), &handoff, options.HandoffKey, false); err != nil {
		return Handoff{}, "", false, err
	}
	return handoff, handoff.RollbackPath, true, nil
}

func CommitHealthy(
	ctx context.Context,
	options ActivationOptions,
	store Store,
	now time.Time,
) (Handoff, bool, error) {
	resolvedRoot, _, err := activationPaths(options.Root, options.FallbackPath)
	if err != nil {
		return Handoff{}, false, err
	}
	handoff, err := readHandoff(activePath(resolvedRoot), options.HandoffKey)
	if errors.Is(err, os.ErrNotExist) {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, err
	}
	switch handoff.State {
	case StateActivating:
		if _, err := verifyHandoffEvidence(
			options, handoff, handoff.PreUpdateInstalled, handoff.ActivatedAt,
		); err != nil {
			return Handoff{}, false, err
		}
		if err := verifyRunningTarget(options, handoff); err != nil {
			return Handoff{}, false, err
		}
		current := Current{
			Version: handoffVersion, UpdateID: handoff.ID,
			ArtifactPath: handoff.ArtifactPath, TargetPath: handoff.TargetPath,
			VersionName: handoff.VersionName, Commit: handoff.Commit,
			SizeBytes: handoff.SizeBytes, SHA256: handoff.SHA256,
			SchemaMin: handoff.SchemaMin, SchemaMax: handoff.SchemaMax,
			APIVersion: handoff.APIVersion, Bundle: handoff.Bundle,
			VerifiedAt: handoff.ActivatedAt, ActivatedAt: now.UTC(),
		}
		if err := writeSignedJSON(currentPath(resolvedRoot), &current, options.HandoffKey, false); err != nil {
			return Handoff{}, false, err
		}
		handoff.State = StateSucceeded
		handoff.CompletedAt = now.UTC()
		handoff.UpdatedAt = handoff.CompletedAt
	case StateRolledBack, StateFailed:
	default:
		return handoff, false, nil
	}
	metadata := handoff.Metadata()
	if err := store.UpdateUpdate(ctx, metadata); errors.Is(err, ErrNotFound) {
		if err := store.CreateUpdate(ctx, metadata); err != nil {
			return Handoff{}, false, err
		}
	} else if err != nil {
		return Handoff{}, false, err
	}
	if err := writeSignedJSON(activePath(resolvedRoot), &handoff, options.HandoffKey, false); err != nil {
		return Handoff{}, false, err
	}
	if err := archiveHandoff(resolvedRoot, handoff); err != nil {
		return Handoff{}, false, err
	}
	return handoff, true, nil
}

func verifyRunningTarget(options ActivationOptions, handoff Handoff) error {
	running := options.Installed
	if running.Version != handoff.VersionName ||
		running.Commit != handoff.Commit ||
		running.OS != handoff.PreUpdateInstalled.OS ||
		running.Arch != handoff.PreUpdateInstalled.Arch ||
		running.APIVersion != handoff.APIVersion ||
		running.SchemaVersion < handoff.SchemaMin ||
		running.SchemaVersion > handoff.SchemaMax ||
		!samePath(options.FallbackPath, handoff.ArtifactPath) {
		return ErrIntegrity
	}
	return verifyArtifactFile(handoff.ArtifactPath, handoff.SizeBytes, handoff.SHA256)
}

func prepareDatabaseBackup(
	ctx context.Context,
	root string,
	handoff *Handoff,
	options ActivationOptions,
	preUpdate Installed,
	databaseState backup.Validation,
) error {
	if options.BackupDatabase == nil || options.ValidateDatabase == nil {
		return errors.New("update database backup and validator are required")
	}
	databasePath, err := filepath.Abs(options.DatabasePath)
	if err != nil {
		return err
	}
	backupPath, err := derivedDatabaseBackupPath(root, handoff.ID)
	if err != nil {
		return err
	}
	tempPath := backupPath + ".tmp"
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	created, err := options.BackupDatabase(ctx, tempPath)
	if err != nil {
		return fmt.Errorf("create pre-update database backup: %w", err)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	verified, err := options.ValidateDatabase(ctx, tempPath, false)
	if err != nil {
		return fmt.Errorf("validate pre-update database backup: %w", err)
	}
	if created.HostID != databaseState.HostID {
		return ErrIntegrity
	}
	if err := validateDatabaseEvidence(created, verified, preUpdate.SchemaVersion); err != nil {
		return err
	}
	size, checksum, err := digestRegularFile(tempPath)
	if err != nil {
		return err
	}
	if err := replaceFile(tempPath, backupPath); err != nil {
		return fmt.Errorf("activate pre-update database backup: %w", err)
	}
	removeTemp = false
	if err := syncDirectory(filepath.Dir(backupPath)); err != nil {
		return err
	}
	if err := verifyArtifactFile(backupPath, size, checksum); err != nil {
		return err
	}
	final, err := options.ValidateDatabase(ctx, backupPath, false)
	if err != nil {
		return fmt.Errorf("revalidate pre-update database backup: %w", err)
	}
	if err := validateDatabaseEvidence(verified, final, preUpdate.SchemaVersion); err != nil {
		return err
	}
	handoff.DatabasePath = filepath.Clean(databasePath)
	handoff.DatabaseBackupPath = backupPath
	handoff.DatabaseSizeBytes = size
	handoff.DatabaseSHA256 = checksum
	handoff.DatabaseSchema = final.SchemaVersion
	handoff.DatabaseHostID = final.HostID
	return nil
}

func restoreDatabaseForRollback(
	ctx context.Context,
	root string,
	handoff Handoff,
	options ActivationOptions,
) error {
	if handoff.DatabasePath == "" {
		return nil
	}
	if options.ValidateDatabase == nil || options.RestoreDatabase == nil {
		return errors.New("update database restore and validator are required")
	}
	expectedBackup, err := derivedDatabaseBackupPath(root, handoff.ID)
	if err != nil ||
		!samePath(handoff.DatabasePath, options.DatabasePath) ||
		!samePath(handoff.DatabaseBackupPath, expectedBackup) {
		return ErrIntegrity
	}
	current, err := options.ValidateDatabase(ctx, handoff.DatabasePath, false)
	if err != nil {
		return fmt.Errorf("validate database before update rollback: %w", err)
	}
	if current.QuickCheck != "ok" || current.ForeignKeyCount != 0 {
		return ErrIntegrity
	}
	if current.SchemaVersion < 1 || current.HostID != handoff.DatabaseHostID {
		return ErrIntegrity
	}
	if current.SchemaVersion <= handoff.PreUpdateInstalled.SchemaVersion {
		return nil
	}
	backupFile, err := OpenVerifiedArtifact(
		handoff.DatabaseBackupPath,
		handoff.DatabaseSizeBytes,
		handoff.DatabaseSHA256,
	)
	if err != nil {
		return err
	}
	defer backupFile.Close()
	backupState, err := options.ValidateDatabase(ctx, handoff.DatabaseBackupPath, false)
	if err != nil {
		return fmt.Errorf("validate update rollback database backup: %w", err)
	}
	expected := backup.Validation{
		QuickCheck: "ok", ForeignKeyCount: 0,
		SchemaVersion: handoff.DatabaseSchema, HostID: handoff.DatabaseHostID,
	}
	if err := validateDatabaseEvidence(expected, backupState, handoff.PreUpdateInstalled.SchemaVersion); err != nil {
		return err
	}
	if err := options.RestoreDatabase(
		ctx, backupFile, handoff.DatabasePath,
	); err != nil {
		return fmt.Errorf("restore pre-update database backup: %w", err)
	}
	restored, err := options.ValidateDatabase(ctx, handoff.DatabasePath, false)
	if err != nil {
		return fmt.Errorf("validate restored pre-update database: %w", err)
	}
	return validateDatabaseEvidence(expected, restored, handoff.PreUpdateInstalled.SchemaVersion)
}

func validateDatabaseEvidence(
	expected backup.Validation,
	actual backup.Validation,
	maxSchema int,
) error {
	if expected.QuickCheck != "ok" || actual.QuickCheck != "ok" ||
		expected.ForeignKeyCount != 0 || actual.ForeignKeyCount != 0 ||
		expected.SchemaVersion < 1 ||
		expected.SchemaVersion != maxSchema ||
		actual.SchemaVersion != expected.SchemaVersion ||
		actual.HostID == "" || actual.HostID != expected.HostID {
		return ErrIntegrity
	}
	return nil
}

func derivedDatabaseBackupPath(root, id string) (string, error) {
	if !updateIDPattern.MatchString(id) {
		return "", ErrIntegrity
	}
	return filepath.Join(root, "update-"+id+".pre-update.db"), nil
}

func digestRegularFile(path string) (int64, string, error) {
	file, err := openArtifactFile(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, "", err
	}
	if err := validateRegularFile(file, info); err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return 0, "", ErrIntegrity
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyHandoffEvidence(
	options ActivationOptions,
	handoff Handoff,
	installed Installed,
	now time.Time,
) (Verified, error) {
	if len(bytesTrimSpace(options.TrustedRoot)) == 0 {
		return Verified{}, ErrTrustMissing
	}
	verified, err := Verify(options.TrustedRoot, handoff.Bundle, TrustedState{},
		installed, options.Policy, now)
	if err != nil {
		return Verified{}, err
	}
	var recordedRoot Envelope[Root]
	if err := json.Unmarshal(handoff.Root, &recordedRoot); err != nil {
		return Verified{}, ErrIntegrity
	}
	root, err := canonicalJSON(verified.Root)
	if err != nil {
		return Verified{}, err
	}
	recorded, err := canonicalJSON(recordedRoot)
	if err != nil {
		return Verified{}, ErrIntegrity
	}
	if !bytes.Equal(root, recorded) ||
		verified.NextState != handoff.Trust ||
		verified.TargetPath != handoff.TargetPath ||
		verified.Target.Length != handoff.SizeBytes ||
		!strings.EqualFold(verified.Target.Hashes["sha256"], handoff.SHA256) ||
		verified.Target.Custom.Version != handoff.VersionName ||
		verified.Target.Custom.Commit != handoff.Commit ||
		verified.Target.Custom.SchemaMin != handoff.SchemaMin ||
		verified.Target.Custom.SchemaMax != handoff.SchemaMax ||
		verified.Target.Custom.APIVersion != handoff.APIVersion {
		return Verified{}, ErrIntegrity
	}
	return verified, nil
}

func verifyCurrentEvidence(options ActivationOptions, current Current, now time.Time) error {
	if current.VerifiedAt.IsZero() || current.VerifiedAt.After(now) {
		return ErrIntegrity
	}
	handoff := Handoff{
		ID: current.UpdateID, TargetPath: current.TargetPath,
		ArtifactPath: current.ArtifactPath, VersionName: current.VersionName,
		Commit: current.Commit, SizeBytes: current.SizeBytes, SHA256: current.SHA256,
		SchemaMin: current.SchemaMin, SchemaMax: current.SchemaMax,
		APIVersion: current.APIVersion, Bundle: current.Bundle,
	}
	verified, err := Verify(options.TrustedRoot, current.Bundle, TrustedState{},
		options.Installed, options.Policy, current.VerifiedAt)
	if err != nil {
		return err
	}
	root, err := json.Marshal(verified.Root)
	if err != nil {
		return err
	}
	handoff.Root = root
	handoff.Trust = verified.NextState
	_, err = verifyHandoffEvidence(options, handoff, options.Installed, current.VerifiedAt)
	return err
}

func activationPaths(root, fallback string) (string, string, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(fallback) == "" {
		return "", "", errors.New("update root and fallback executable are required")
	}
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	resolvedFallback, err := filepath.Abs(fallback)
	if err != nil {
		return "", "", err
	}
	return filepath.Clean(resolvedRoot), filepath.Clean(resolvedFallback), nil
}

func verifyArtifactFile(path string, size int64, checksum string) error {
	file, err := OpenVerifiedArtifact(path, size, checksum)
	if err != nil {
		return err
	}
	return file.Close()
}

func OpenVerifiedArtifact(path string, size int64, checksum string) (*os.File, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, ErrIntegrity
	}
	file, err := openArtifactFile(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := validateRegularFile(file, info); err != nil || info.Size() != size {
		_ = file.Close()
		return nil, ErrIntegrity
	}
	if err := VerifyArtifact(file, TargetFile{
		Length: size, Hashes: map[string]string{"sha256": checksum},
	}); err != nil {
		_ = file.Close()
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		_ = file.Close()
		return nil, ErrIntegrity
	}
	return file, nil
}

func verifyExecutablePath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: worker path is not a regular file", ErrIntegrity)
	}
	return nil
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil &&
		strings.EqualFold(filepath.Clean(leftAbs), filepath.Clean(rightAbs))
}

func platformForPath(path string) string {
	if strings.EqualFold(filepath.Ext(path), ".exe") {
		return "windows"
	}
	return "unix"
}
