package scaleset

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/actions/scaleset"
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestIsSessionConflict pins which failures count as "the previous session has
// not been released yet". Treating an unrelated 409 as that would make a real
// error look like a restart waiting to resolve itself.
func TestIsSessionConflict(t *testing.T) {
	conflict := errors.New(`request POST https://broker.actions.githubusercontent.com/rest/_apis/runtime/runnerscalesets/2/sessions?api-version=6.0-preview failed(status="409 Conflict", github_request_id="F60C"): unexpected status code 409 Conflict: GitHub.Actions.Runtime.WebApi.RunnerScaleSetSessionConflictException, GitHub.Actions.Runtime.WebApi: The actions runner scaleset linux-pool already has an active session.`)
	if !isSessionConflict(conflict) {
		t.Error("the one-active-session 409 was not recognised")
	}

	for _, other := range []error{
		errors.New(`failed(status="409 Conflict"): some other conflict`),
		errors.New("RunnerScaleSetSessionConflictException mentioned in a 500"),
		errors.New(`failed(status="403 Forbidden")`),
	} {
		if isSessionConflict(other) {
			t.Errorf("unrelated error treated as a session conflict: %v", other)
		}
	}
}
