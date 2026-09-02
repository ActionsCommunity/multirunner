package scaleset

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/actions/scaleset"

	"github.com/GerardSmit/multirunner/internal/buildinfo"
)

type fakeScaleSetManager struct {
	existing *scaleset.RunnerScaleSet
	getErr   error
	created  *scaleset.RunnerScaleSet
	updated  *scaleset.RunnerScaleSet
	updateID int
}

func (f *fakeScaleSetManager) GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error) {
	return f.existing, f.getErr
}

func (f *fakeScaleSetManager) CreateRunnerScaleSet(_ context.Context, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.created = set
	result := *set
	result.ID = 10
	return &result, nil
}

func (f *fakeScaleSetManager) UpdateRunnerScaleSet(_ context.Context, id int, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.updateID = id
	f.updated = set
	result := *set
	result.ID = id
	return &result, nil
}

func TestEnsureScaleSetCreatesMissingSet(t *testing.T) {
	client := &fakeScaleSetManager{}
	opts := SessionOptions{Name: "linux", Labels: []string{"self-hosted", "linux"}}

	got, err := ensureScaleSet(context.Background(), client, opts, 2, testLogger())
	if err != nil {
		t.Fatalf("ensureScaleSet: %v", err)
	}
	if got.ID != 10 || client.created == nil {
		t.Fatalf("created set = %+v, want ID 10", got)
	}
	if client.created.RunnerGroupID != 2 || !client.created.RunnerSetting.DisableUpdate {
		t.Errorf("created set = %+v", client.created)
	}
}

func TestEnsureScaleSetReusesMatchingSet(t *testing.T) {
	opts := SessionOptions{Name: "linux", Labels: []string{"linux", "self-hosted"}}
	existing := desiredScaleSet(opts, 2)
	existing.ID = 7
	client := &fakeScaleSetManager{existing: existing}

	got, err := ensureScaleSet(context.Background(), client, opts, 2, testLogger())
	if err != nil {
		t.Fatalf("ensureScaleSet: %v", err)
	}
	if got != existing || client.updated != nil {
		t.Fatalf("matching set was not reused: got=%+v updated=%+v", got, client.updated)
	}
}

func TestEnsureScaleSetUpdatesConfigurationDrift(t *testing.T) {
	existing := &scaleset.RunnerScaleSet{
		ID:            7,
		Name:          "linux",
		RunnerGroupID: 2,
		Labels:        []scaleset.Label{{Name: "old"}},
	}
	client := &fakeScaleSetManager{existing: existing}
	opts := SessionOptions{Name: "linux", Labels: []string{"self-hosted", "linux"}}

	got, err := ensureScaleSet(context.Background(), client, opts, 2, testLogger())
	if err != nil {
		t.Fatalf("ensureScaleSet: %v", err)
	}
	if got.ID != 7 || client.updateID != 7 || client.updated == nil {
		t.Fatalf("updated set = %+v, update ID = %d", client.updated, client.updateID)
	}
	if !scaleSetMatches(got, desiredScaleSet(opts, 2)) {
		t.Errorf("updated set does not match desired configuration: %+v", got)
	}
}

func TestEnsureScaleSetDoesNotCreateAfterLookupError(t *testing.T) {
	client := &fakeScaleSetManager{getErr: errors.New("service unavailable")}

	if _, err := ensureScaleSet(context.Background(), client, SessionOptions{Name: "linux"}, 1, testLogger()); err == nil {
		t.Fatal("expected lookup error")
	}
	if client.created != nil {
		t.Fatalf("created a set after lookup failure: %+v", client.created)
	}
}

func TestDesiredScaleSetDefaultsLabelToName(t *testing.T) {
	got := desiredScaleSet(SessionOptions{Name: "linux"}, 1)
	if len(got.Labels) != 1 || got.Labels[0].Name != "linux" {
		t.Fatalf("labels = %+v, want scale set name", got.Labels)
	}
}

func TestSystemInfoIncludesBuildIdentity(t *testing.T) {
	originalVersion, originalCommit := buildinfo.Version, buildinfo.Commit
	buildinfo.Version, buildinfo.Commit = "v1.2.3", "abc123"
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit = originalVersion, originalCommit
	})

	got := systemInfo(42)
	if got.System != "multirunner" || got.Subsystem != "multirunner" {
		t.Errorf("system identity = %q/%q", got.System, got.Subsystem)
	}
	if got.Version != "v1.2.3" || got.CommitSHA != "abc123" {
		t.Errorf("build identity = %q at %q", got.Version, got.CommitSHA)
	}
	if got.ScaleSetID != 42 {
		t.Errorf("scale set ID = %d, want 42", got.ScaleSetID)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
