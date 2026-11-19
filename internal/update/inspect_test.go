package update

import (
	"strings"
	"testing"
	"time"
)

func TestInspectorAllowsTrustedMetadataWithoutDownloadingTarget(t *testing.T) {
	repository := newTestRepository(t)
	source := &fakeSource{
		bundle: MetadataBundle{
			Timestamp: repository.timestamp,
			Snapshot:  repository.snapshot,
			Targets:   repository.targets,
		},
		artifact: repository.artifact,
	}
	inspector, err := NewInspector(
		source, repository.installed, repository.root, repository.policy,
		func() time.Time { return repository.now },
	)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := inspector.Inspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Verified || !inspection.ApplyAllowed ||
		inspection.Version != "v1.2.0" || source.targetCalls != 0 {
		t.Fatalf("inspection = %+v, target calls = %d", inspection, source.targetCalls)
	}
}

func TestInspectorTrustlessModeNeverAllowsApply(t *testing.T) {
	repository := newTestRepository(t)
	source := &fakeSource{
		bundle: MetadataBundle{
			Timestamp: repository.timestamp,
			Snapshot:  repository.snapshot,
			Targets:   repository.targets,
		},
	}
	inspector, err := NewInspector(
		source, repository.installed, nil, Policy{},
		func() time.Time { return repository.now },
	)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := inspector.Inspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Verified || inspection.ApplyAllowed || !inspection.Compatible ||
		!strings.Contains(inspection.Reason, "untrusted") || source.targetCalls != 0 {
		t.Fatalf("inspection = %+v, target calls = %d", inspection, source.targetCalls)
	}
}
