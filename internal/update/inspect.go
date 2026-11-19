package update

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Inspection struct {
	CheckedAt          time.Time `json:"checked_at"`
	TrustConfigured    bool      `json:"trust_configured"`
	Verified           bool      `json:"verified"`
	MetadataConsistent bool      `json:"metadata_consistent"`
	Compatible         bool      `json:"compatible"`
	ApplyAllowed       bool      `json:"apply_allowed"`
	Version            string    `json:"version,omitempty"`
	Commit             string    `json:"commit,omitempty"`
	TargetPath         string    `json:"target_path,omitempty"`
	SizeBytes          int64     `json:"size_bytes,omitempty"`
	SchemaMin          int       `json:"schema_min,omitempty"`
	SchemaMax          int       `json:"schema_max,omitempty"`
	APIVersion         string    `json:"api_version,omitempty"`
	TimestampExpires   time.Time `json:"timestamp_expires,omitempty"`
	SnapshotExpires    time.Time `json:"snapshot_expires,omitempty"`
	TargetsExpires     time.Time `json:"targets_expires,omitempty"`
	Reason             string    `json:"reason,omitempty"`
}

type Inspector struct {
	source      Source
	installed   Installed
	trustedRoot []byte
	policy      Policy
	now         func() time.Time
}

func NewInspector(
	source Source,
	installed Installed,
	trustedRoot []byte,
	policy Policy,
	now func() time.Time,
) (*Inspector, error) {
	if source == nil || installed.OS == "" || installed.Arch == "" ||
		installed.APIVersion == "" || installed.SchemaVersion < 1 {
		return nil, errors.New("update inspection source and installed identity are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Inspector{
		source: source, installed: installed,
		trustedRoot: append([]byte(nil), trustedRoot...),
		policy:      policy, now: now,
	}, nil
}

func (i *Inspector) Inspect(ctx context.Context) (Inspection, error) {
	inspection := Inspection{
		CheckedAt:       i.now().UTC(),
		TrustConfigured: len(bytesTrimSpace(i.trustedRoot)) > 0,
	}
	rootVersion := 0
	if inspection.TrustConfigured {
		root, err := decodeEnvelope[Root](i.trustedRoot)
		if err != nil {
			return Inspection{}, err
		}
		rootVersion = root.Signed.Version
	}
	bundle, err := i.source.Bundle(ctx, rootVersion)
	if err != nil {
		return Inspection{}, err
	}
	if inspection.TrustConfigured {
		verified, err := Verify(
			i.trustedRoot, bundle, TrustedState{RootVersion: rootVersion},
			i.installed, i.policy, inspection.CheckedAt,
		)
		if err != nil {
			return Inspection{}, err
		}
		fillInspection(&inspection, verified.TargetPath, verified.Target)
		inspection.Verified = true
		inspection.MetadataConsistent = true
		inspection.Compatible = true
		inspection.ApplyAllowed = true
		inspection.TimestampExpires = verified.Timestamp.Signed.Expires
		inspection.SnapshotExpires = verified.Snapshot.Signed.Expires
		inspection.TargetsExpires = verified.Targets.Signed.Expires
		return inspection, nil
	}

	timestamp, snapshot, targets, err := inspectUntrustedBundle(bundle)
	if err != nil {
		return Inspection{}, err
	}
	inspection.MetadataConsistent = true
	inspection.TimestampExpires = timestamp.Signed.Expires
	inspection.SnapshotExpires = snapshot.Signed.Expires
	inspection.TargetsExpires = targets.Signed.Expires
	targetPath, target, err := selectUntrustedTarget(targets.Signed.Targets, i.installed)
	if err != nil {
		inspection.Reason = err.Error()
		return inspection, nil
	}
	fillInspection(&inspection, targetPath, target)
	inspection.Compatible = true
	inspection.Reason = "metadata is untrusted; configure a trusted root and signer/provenance policy to stage or apply"
	return inspection, nil
}

func inspectUntrustedBundle(
	bundle MetadataBundle,
) (Envelope[Timestamp], Envelope[Snapshot], Envelope[Targets], error) {
	timestamp, err := decodeEnvelope[Timestamp](bundle.Timestamp)
	if err != nil {
		return Envelope[Timestamp]{}, Envelope[Snapshot]{}, Envelope[Targets]{}, err
	}
	snapshotMeta, ok := timestamp.Signed.Meta["snapshot.json"]
	if !ok || verifyLinked("snapshot.json", bundle.Snapshot, snapshotMeta) != nil {
		return Envelope[Timestamp]{}, Envelope[Snapshot]{}, Envelope[Targets]{}, ErrIntegrity
	}
	snapshot, err := decodeEnvelope[Snapshot](bundle.Snapshot)
	if err != nil || snapshot.Signed.Version != snapshotMeta.Version {
		return Envelope[Timestamp]{}, Envelope[Snapshot]{}, Envelope[Targets]{}, ErrIntegrity
	}
	targetsMeta, ok := snapshot.Signed.Meta["targets.json"]
	if !ok || verifyLinked("targets.json", bundle.Targets, targetsMeta) != nil {
		return Envelope[Timestamp]{}, Envelope[Snapshot]{}, Envelope[Targets]{}, ErrIntegrity
	}
	targets, err := decodeEnvelope[Targets](bundle.Targets)
	if err != nil || targets.Signed.Version != targetsMeta.Version {
		return Envelope[Timestamp]{}, Envelope[Snapshot]{}, Envelope[Targets]{}, ErrIntegrity
	}
	return timestamp, snapshot, targets, nil
}

func selectUntrustedTarget(
	targets map[string]TargetFile,
	installed Installed,
) (string, TargetFile, error) {
	var selectedPath string
	var selected TargetFile
	for path, target := range targets {
		if target.Custom.OS != installed.OS || target.Custom.Arch != installed.Arch {
			continue
		}
		if selectedPath != "" {
			return "", TargetFile{}, fmt.Errorf("%w: multiple targets match platform", ErrIntegrity)
		}
		selectedPath, selected = path, target
	}
	if selectedPath == "" {
		return "", TargetFile{}, ErrNotFound
	}
	custom := selected.Custom
	if selected.Length < 1 || !validSHA256(selected.Hashes["sha256"]) ||
		!validReleaseVersion(custom.Version) || !validCommit(custom.Commit) ||
		custom.APIVersion != installed.APIVersion ||
		installed.SchemaVersion < custom.SchemaMin ||
		installed.SchemaVersion > custom.SchemaMax ||
		compareReleaseVersions(custom.Version, installed.Version) <= 0 {
		return "", TargetFile{}, ErrIncompatible
	}
	return selectedPath, selected, nil
}

func fillInspection(inspection *Inspection, targetPath string, target TargetFile) {
	inspection.Version = target.Custom.Version
	inspection.Commit = target.Custom.Commit
	inspection.TargetPath = targetPath
	inspection.SizeBytes = target.Length
	inspection.SchemaMin = target.Custom.SchemaMin
	inspection.SchemaMax = target.Custom.SchemaMax
	inspection.APIVersion = target.Custom.APIVersion
}
