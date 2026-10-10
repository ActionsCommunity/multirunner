package runtimecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backup"
	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/historysync"
	"github.com/GerardSmit/multirunner/internal/operations"
	"github.com/GerardSmit/multirunner/internal/pool"
	"github.com/GerardSmit/multirunner/internal/restore"
	"github.com/GerardSmit/multirunner/internal/supportbundle"
	"github.com/GerardSmit/multirunner/internal/update"
)

type fakePool struct {
	mu         sync.Mutex
	name       string
	paused     bool
	sessions   []string
	terminated []string
}

func (p *fakePool) Name() string {
	return p.name
}

func (p *fakePool) Pause() {
	p.mu.Lock()
	p.paused = true
	p.mu.Unlock()
}

func (p *fakePool) Resume() {
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
}

func (p *fakePool) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

func (p *fakePool) ActiveSessions() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sessions...)
}

func (p *fakePool) Drain(context.Context) error {
	p.Pause()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) > 0 {
		return errors.New("active sessions remain")
	}
	return nil
}

func (p *fakePool) Terminate(_ context.Context, session string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for index, value := range p.sessions {
		if value == session {
			p.sessions = append(p.sessions[:index], p.sessions[index+1:]...)
			p.terminated = append(p.terminated, session)
			return nil
		}
	}
	return pool.ErrRunnerSessionNotFound
}

type fakeSyncer struct {
	report historysync.Report
	calls  int
}

type fakeBundleGenerator struct {
	bundles map[string]supportbundle.Metadata
	calls   int
}

type fakeBackupGenerator struct {
	backups map[string]backup.Metadata
	calls   int
}

type fakeRestoreStager struct {
	restores map[string]restore.Metadata
	calls    int
}

type fakeUpdateStager struct {
	updates map[string]update.Metadata
	calls   int
}

func (s *fakeUpdateStager) Stage(
	_ context.Context, id string, request update.Request,
) (update.Metadata, error) {
	s.calls++
	if s.updates == nil {
		s.updates = make(map[string]update.Metadata)
	}
	metadata := update.Metadata{
		ID: id, CommandID: id, Version: request.Version,
		State: update.StateStaged, SHA256: "abc",
	}
	s.updates[id] = metadata
	return metadata, nil
}

func (s *fakeUpdateStager) Metadata(
	_ context.Context, id string,
) (update.Metadata, error) {
	metadata, ok := s.updates[id]
	if !ok {
		return update.Metadata{}, update.ErrNotFound
	}
	return metadata, nil
}

func (s *fakeUpdateStager) Exists(ctx context.Context, id string) (bool, error) {
	_, err := s.Metadata(ctx, id)
	return err == nil, nil
}

func (s *fakeRestoreStager) Stage(
	_ context.Context, id string, request restore.Request,
) (restore.Metadata, error) {
	s.calls++
	if s.restores == nil {
		s.restores = make(map[string]restore.Metadata)
	}
	metadata := restore.Metadata{
		ID: id, CommandID: id, BackupID: request.BackupID,
		State: restore.StateStaged, SHA256: "abc",
	}
	s.restores[id] = metadata
	return metadata, nil
}

func (s *fakeRestoreStager) Metadata(
	_ context.Context, id string,
) (restore.Metadata, error) {
	metadata, ok := s.restores[id]
	if !ok {
		return restore.Metadata{}, restore.ErrNotFound
	}
	return metadata, nil
}

func (s *fakeRestoreStager) Exists(ctx context.Context, id string) (bool, error) {
	_, err := s.Metadata(ctx, id)
	return err == nil, nil
}

func (g *fakeBackupGenerator) Generate(
	_ context.Context, id string, request backup.Request,
) (backup.Metadata, error) {
	g.calls++
	if g.backups == nil {
		g.backups = make(map[string]backup.Metadata)
	}
	metadata := backup.Metadata{
		ID: id, CommandID: id, Purpose: request.Purpose,
		State: backup.StateSucceeded, SHA256: "abc",
		DownloadURL: "/api/v1/backups/" + id + "/download",
	}
	g.backups[id] = metadata
	return metadata, nil
}

func (g *fakeBackupGenerator) Metadata(
	_ context.Context, id string,
) (backup.Metadata, error) {
	metadata, ok := g.backups[id]
	if !ok {
		return backup.Metadata{}, backup.ErrNotFound
	}
	return metadata, nil
}

func (g *fakeBackupGenerator) Exists(ctx context.Context, id string) (bool, error) {
	_, err := g.Metadata(ctx, id)
	return err == nil, nil
}

func (g *fakeBundleGenerator) Generate(
	_ context.Context, id string, request supportbundle.Request,
) (supportbundle.Metadata, error) {
	g.calls++
	if g.bundles == nil {
		g.bundles = make(map[string]supportbundle.Metadata)
	}
	metadata := supportbundle.Metadata{
		ID: id, CommandID: id, From: request.From, To: request.To,
		ExpiresAt:   time.Now().Add(time.Hour),
		DownloadURL: "/api/v1/support-bundles/" + id,
	}
	g.bundles[id] = metadata
	return metadata, nil
}

func (g *fakeBundleGenerator) Metadata(
	_ context.Context, id string,
) (supportbundle.Metadata, error) {
	metadata, ok := g.bundles[id]
	if !ok {
		return supportbundle.Metadata{}, supportbundle.ErrNotFound
	}
	return metadata, nil
}

func (g *fakeBundleGenerator) Exists(ctx context.Context, id string) (bool, error) {
	_, err := g.Metadata(ctx, id)
	return err == nil, nil
}

func (s *fakeSyncer) Sync(context.Context) (historysync.Report, error) {
	s.calls++
	return s.report, nil
}

func TestRegistryPoolAndRunnerAdaptersAreCapabilityAware(t *testing.T) {
	controller := &fakePool{name: "linux", sessions: []string{"session-1"}}
	registry := NewRegistry()
	registry.SetPools([]PoolController{controller})

	pauseCommand := testRuntimeCommand(CommandPoolPause, "pool", "linux", "pool:linux", nil)
	adapter, ok := registry.AdapterFor(pauseCommand)
	if !ok {
		t.Fatal("pool pause adapter is unavailable")
	}
	invocation := runtimeInvocation(pauseCommand, 1)
	before, err := adapter.Prepare(t.Context(), invocation)
	if err != nil || !jsonContains(t, before, `"paused":false`) {
		t.Fatalf("pool before state = %s err=%v", before, err)
	}
	result, err := adapter.Execute(t.Context(), invocation)
	if err != nil || result.Disposition != control.EffectSucceeded || !controller.Paused() {
		t.Fatalf("pool pause result=%+v err=%v", result, err)
	}
	reconciled, err := adapter.Reconcile(t.Context(), invocation)
	if err != nil || reconciled.Disposition != control.ReconcileApplied {
		t.Fatalf("pool pause reconciliation=%+v err=%v", reconciled, err)
	}

	runnerCommand := testRuntimeCommand(
		CommandRunnerTerminate, "runner", "session-1", "pool:linux",
		json.RawMessage(`{"pool":"linux"}`),
	)
	runnerAdapter, ok := registry.AdapterFor(runnerCommand)
	if !ok {
		t.Fatal("runner terminate adapter is unavailable")
	}
	runnerResult, err := runnerAdapter.Execute(t.Context(), runtimeInvocation(runnerCommand, 2))
	if err != nil || runnerResult.Disposition != control.EffectSucceeded ||
		len(controller.ActiveSessions()) != 0 {
		t.Fatalf("runner termination=%+v err=%v sessions=%v",
			runnerResult, err, controller.ActiveSessions())
	}

	stale := runtimeInvocation(runnerCommand, 1)
	if _, err := runnerAdapter.Prepare(t.Context(), stale); !errors.Is(err, control.ErrStaleFence) {
		t.Fatalf("stale adapter fence error = %v", err)
	}
}

func TestRegistryHistorySyncAdapterAndCapabilities(t *testing.T) {
	syncer := &fakeSyncer{report: historysync.Report{
		Repositories: []historysync.RepositoryReport{
			{Repository: "actionscommunity/multirunner", Runs: 2, Jobs: 3},
		},
	}}
	registry := NewRegistry()
	registry.SetHistorySyncer(syncer)
	command := testRuntimeCommand(CommandHistorySync, "system", "history", "history", nil)
	adapter, ok := registry.AdapterFor(command)
	if !ok {
		t.Fatal("history sync adapter is unavailable")
	}

	result, err := adapter.Execute(t.Context(), runtimeInvocation(command, 1))
	if err != nil || result.Disposition != control.EffectSucceeded || syncer.calls != 1 ||
		!jsonContains(t, result.Outcome, `"jobs":3`) {
		t.Fatalf("history sync result=%s disposition=%s calls=%d err=%v",
			result.Outcome, result.Disposition, syncer.calls, err)
	}
	reconciled, err := adapter.Reconcile(t.Context(), runtimeInvocation(command, 2))
	if err != nil || reconciled.Disposition != control.ReconcileUnknown {
		t.Fatalf("history sync reconciliation=%+v err=%v", reconciled, err)
	}

	capabilities := registry.Capabilities()
	var historySupported, poolSupported bool
	for _, capability := range capabilities {
		switch capability.CommandType {
		case CommandHistorySync:
			historySupported = capability.Supported
		case CommandPoolPause:
			poolSupported = capability.Supported
		}
	}
	if !historySupported || poolSupported {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestRegistryDisabledCapabilityReportsConfiguredReason(t *testing.T) {
	registry := NewRegistry()
	registry.SetPools([]PoolController{&fakePool{
		name: "linux", sessions: []string{"runner-1"},
	}})
	registry.Disable(CommandRunnerTerminate, "disabled by operator configuration")
	command := testRuntimeCommand(
		CommandRunnerTerminate, "runner", "runner-1", "pool:linux",
		json.RawMessage(`{"pool":"linux"}`),
	)
	if _, ok := registry.AdapterFor(command); ok {
		t.Fatal("disabled capability exposed an adapter")
	}
	err := registry.Validate(command)
	if !errors.Is(err, ErrCapabilityUnsupported) ||
		!strings.Contains(err.Error(), "disabled by operator configuration") {
		t.Fatalf("validation error = %v", err)
	}
	for _, capability := range registry.Capabilities() {
		if capability.CommandType == CommandRunnerTerminate {
			if capability.Supported ||
				capability.Reason != "disabled by operator configuration" {
				t.Fatalf("capability = %+v", capability)
			}
			return
		}
	}
	t.Fatal("runner termination capability was not reported")
}

func TestRegistrySupportBundleAdapterGeneratesAndReconciles(t *testing.T) {
	generator := &fakeBundleGenerator{}
	registry := NewRegistry()
	registry.SetSupportBundleGenerator(generator)
	command := testRuntimeCommand(
		CommandSupportBundle, "system", "support-bundles", "support-bundles",
		json.RawMessage(`{"from":"2026-10-08T18:00:00Z","to":"2026-10-08T19:00:00Z"}`),
	)
	if err := registry.Validate(command); err != nil {
		t.Fatal(err)
	}
	adapter, ok := registry.AdapterFor(command)
	if !ok {
		t.Fatal("support bundle adapter is unavailable")
	}
	result, err := adapter.Execute(t.Context(), runtimeInvocation(command, 1))
	if err != nil || result.Disposition != control.EffectSucceeded ||
		generator.calls != 1 || !jsonContains(t, result.Outcome, `"id":"command"`) {
		t.Fatalf("support bundle result=%s disposition=%s calls=%d err=%v",
			result.Outcome, result.Disposition, generator.calls, err)
	}
	reconciled, err := adapter.Reconcile(t.Context(), runtimeInvocation(command, 1))
	if err != nil || reconciled.Disposition != control.ReconcileApplied ||
		!jsonContains(t, reconciled.Outcome, `"download_url":"/api/v1/support-bundles/command"`) {
		t.Fatalf("support bundle reconciliation=%+v err=%v", reconciled, err)
	}
}

func TestRegistryBackupAdapterGeneratesAndReconciles(t *testing.T) {
	generator := &fakeBackupGenerator{}
	registry := NewRegistry()
	registry.SetBackupGenerator(generator)
	command := testRuntimeCommand(
		CommandBackupCreate, "system", "backups", "database-maintenance",
		json.RawMessage(`{"purpose":"manual"}`),
	)
	if err := registry.Validate(command); err != nil {
		t.Fatal(err)
	}

	adapter, ok := registry.AdapterFor(command)
	if !ok {
		t.Fatal("backup adapter is unavailable")
	}
	result, err := adapter.Execute(t.Context(), runtimeInvocation(command, 1))
	if err != nil || result.Disposition != control.EffectSucceeded ||
		generator.calls != 1 || !jsonContains(t, result.Outcome, `"sha256":"abc"`) {
		t.Fatalf("backup result=%s disposition=%s calls=%d err=%v",
			result.Outcome, result.Disposition, generator.calls, err)
	}
	reconciled, err := adapter.Reconcile(t.Context(), runtimeInvocation(command, 1))
	if err != nil || reconciled.Disposition != control.ReconcileApplied ||
		!jsonContains(t, reconciled.Outcome, `"download_url":"/api/v1/backups/command/download"`) {
		t.Fatalf("backup reconciliation=%+v err=%v", reconciled, err)
	}
}

func TestRegistryRestoreAdapterStagesAndReconciles(t *testing.T) {
	stager := &fakeRestoreStager{}
	registry := NewRegistry()
	registry.SetRestoreStager(stager)
	command := testRuntimeCommand(
		CommandRestoreStage, "system", "restore", "database-maintenance",
		json.RawMessage(`{"backup_id":"backup-1"}`),
	)
	if err := registry.Validate(command); err != nil {
		t.Fatal(err)
	}

	adapter, ok := registry.AdapterFor(command)
	if !ok {
		t.Fatal("restore adapter is unavailable")
	}
	result, err := adapter.Execute(t.Context(), runtimeInvocation(command, 1))
	if err != nil || result.Disposition != control.EffectSucceeded ||
		stager.calls != 1 || !jsonContains(t, result.Outcome, `"state":"staged"`) {
		t.Fatalf("restore result=%s disposition=%s calls=%d err=%v",
			result.Outcome, result.Disposition, stager.calls, err)
	}
	reconciled, err := adapter.Reconcile(t.Context(), runtimeInvocation(command, 1))
	if err != nil || reconciled.Disposition != control.ReconcileApplied ||
		!jsonContains(t, reconciled.Outcome, `"backup_id":"backup-1"`) {
		t.Fatalf("restore reconciliation=%+v err=%v", reconciled, err)
	}
}

func TestRegistryUpdateAdapterStagesAndReconciles(t *testing.T) {
	stager := &fakeUpdateStager{}
	registry := NewRegistry()
	registry.SetUpdateStager(stager)
	command := testRuntimeCommand(
		CommandUpdateStage, "system", "updates", "host-maintenance",
		json.RawMessage(`{"version":"v1.2.0"}`),
	)
	if err := registry.Validate(command); err != nil {
		t.Fatal(err)
	}
	adapter, ok := registry.AdapterFor(command)
	if !ok {
		t.Fatal("update adapter is unavailable")
	}
	result, err := adapter.Execute(t.Context(), runtimeInvocation(command, 1))
	if err != nil || result.Disposition != control.EffectSucceeded ||
		stager.calls != 1 || !jsonContains(t, result.Outcome, `"state":"staged"`) {
		t.Fatalf("update result=%s disposition=%s calls=%d err=%v",
			result.Outcome, result.Disposition, stager.calls, err)
	}
	reconciled, err := adapter.Reconcile(t.Context(), runtimeInvocation(command, 1))
	if err != nil || reconciled.Disposition != control.ReconcileApplied ||
		!jsonContains(t, reconciled.Outcome, `"version":"v1.2.0"`) {
		t.Fatalf("update reconciliation=%+v err=%v", reconciled, err)
	}
}

func testRuntimeCommand(
	commandType, targetType, targetID, domain string, parameters json.RawMessage,
) control.Command {
	if len(parameters) == 0 {
		parameters = json.RawMessage(`{}`)
	}
	return control.Command{
		ID: "command", Type: commandType, HostID: "host",
		TargetType: targetType, TargetID: targetID, ConflictDomain: domain,
		Parameters: parameters, ActorKind: operations.ActorOperator,
		ActorID: "local-operator", Attempt: 1, FencingToken: 1,
	}
}

func runtimeInvocation(command control.Command, token int64) control.Invocation {
	return control.Invocation{
		CommandID: command.ID, CommandType: command.Type, HostID: command.HostID,
		TargetType: command.TargetType, TargetID: command.TargetID,
		ConflictDomain: command.ConflictDomain, Parameters: command.Parameters,
		Attempt: command.Attempt, FencingToken: token,
	}
}

func jsonContains(t *testing.T, value json.RawMessage, fragment string) bool {
	t.Helper()
	if !json.Valid(value) {
		t.Fatalf("invalid JSON: %s", value)
	}
	return string(value) != "" && containsFragment(string(value), fragment)
}

func containsFragment(value, fragment string) bool {
	return len(fragment) <= len(value) &&
		func() bool {
			for index := 0; index+len(fragment) <= len(value); index++ {
				if value[index:index+len(fragment)] == fragment {
					return true
				}
			}
			return false
		}()
}
