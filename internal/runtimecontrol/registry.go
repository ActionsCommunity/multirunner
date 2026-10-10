// Package runtimecontrol adapts durable commands to narrow authoritative
// runtime control interfaces.
package runtimecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/GerardSmit/multirunner/internal/backup"
	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/historysync"
	"github.com/GerardSmit/multirunner/internal/pool"
	"github.com/GerardSmit/multirunner/internal/restore"
	"github.com/GerardSmit/multirunner/internal/supportbundle"
	"github.com/GerardSmit/multirunner/internal/update"
)

const (
	CommandPoolPause       = "pool.pause"
	CommandPoolResume      = "pool.resume"
	CommandPoolDrain       = "pool.drain"
	CommandPoolCancelDrain = "pool.cancel_drain"
	CommandRunnerTerminate = "runner.terminate"
	CommandRunnerRecycle   = "runner.recycle"
	CommandHistorySync     = "history.sync"
	CommandSupportBundle   = "support_bundle.generate"
	CommandBackupCreate    = "backup.create"
	CommandRestoreStage    = "restore.stage"
	CommandUpdateStage     = "update.stage"
)

type PoolController interface {
	Name() string
	Pause()
	Resume()
	Paused() bool
	ActiveSessions() []string
	Drain(context.Context) error
	Terminate(context.Context, string) error
}

type HistorySyncer interface {
	Sync(context.Context) (historysync.Report, error)
}

type SupportBundleGenerator interface {
	Generate(context.Context, string, supportbundle.Request) (supportbundle.Metadata, error)
	Metadata(context.Context, string) (supportbundle.Metadata, error)
	Exists(context.Context, string) (bool, error)
}

type BackupGenerator interface {
	Generate(context.Context, string, backup.Request) (backup.Metadata, error)
	Metadata(context.Context, string) (backup.Metadata, error)
	Exists(context.Context, string) (bool, error)
}

type RestoreStager interface {
	Stage(context.Context, string, restore.Request) (restore.Metadata, error)
	Metadata(context.Context, string) (restore.Metadata, error)
	Exists(context.Context, string) (bool, error)
}

type UpdateStager interface {
	Stage(context.Context, string, update.Request) (update.Metadata, error)
	Metadata(context.Context, string) (update.Metadata, error)
	Exists(context.Context, string) (bool, error)
}

type Capability struct {
	CommandType string `json:"command_type"`
	Supported   bool   `json:"supported"`
	Reason      string `json:"reason,omitempty"`
}

type Registry struct {
	mu       sync.RWMutex
	pools    map[string]PoolController
	syncer   HistorySyncer
	bundles  SupportBundleGenerator
	backups  BackupGenerator
	restores RestoreStager
	updates  UpdateStager
	disabled map[string]string
	fences   *fenceGuard
}

func NewRegistry() *Registry {
	return &Registry{
		pools:    make(map[string]PoolController),
		disabled: make(map[string]string),
		fences:   &fenceGuard{tokens: make(map[string]int64)},
	}
}

// Disable prevents a command adapter from being exposed and records the
// operator-actionable reason returned by capability discovery and validation.
func (r *Registry) Disable(commandType, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disabled[commandType] = reason
}

func (r *Registry) SetPools(pools []PoolController) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools = make(map[string]PoolController, len(pools))
	for _, controller := range pools {
		if controller != nil {
			r.pools[controller.Name()] = controller
		}
	}
}

func (r *Registry) SetHistorySyncer(syncer HistorySyncer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncer = syncer
}

func (r *Registry) SetSupportBundleGenerator(generator SupportBundleGenerator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bundles = generator
}

func (r *Registry) SetBackupGenerator(generator BackupGenerator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backups = generator
}

func (r *Registry) SetRestoreStager(stager RestoreStager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restores = stager
}

func (r *Registry) SetUpdateStager(stager UpdateStager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = stager
}

func (r *Registry) Capabilities() []Capability {
	r.mu.RLock()
	hasPools := len(r.pools) > 0
	hasSyncer := r.syncer != nil
	hasBundles := r.bundles != nil
	hasBackups := r.backups != nil
	hasRestores := r.restores != nil
	hasUpdates := r.updates != nil
	disabled := make(map[string]string, len(r.disabled))
	for commandType, reason := range r.disabled {
		disabled[commandType] = reason
	}
	r.mu.RUnlock()
	capabilities := []Capability{
		{CommandType: CommandPoolPause, Supported: hasPools},
		{CommandType: CommandPoolResume, Supported: hasPools},
		{CommandType: CommandPoolDrain, Supported: hasPools},
		{CommandType: CommandPoolCancelDrain, Supported: hasPools},
		{CommandType: CommandRunnerTerminate, Supported: hasPools},
		{CommandType: CommandRunnerRecycle, Supported: hasPools},
		{CommandType: CommandHistorySync, Supported: hasSyncer},
		{CommandType: CommandSupportBundle, Supported: hasBundles},
		{CommandType: CommandBackupCreate, Supported: hasBackups},
		{CommandType: CommandRestoreStage, Supported: hasRestores},
		{CommandType: CommandUpdateStage, Supported: hasUpdates},
	}
	for index := range capabilities {
		if reason, blocked := disabled[capabilities[index].CommandType]; blocked {
			capabilities[index].Supported = false
			capabilities[index].Reason = reason
			continue
		}
		if !capabilities[index].Supported {
			capabilities[index].Reason = "runtime capability is unavailable in this provisioning mode"
		}
	}
	return capabilities
}

func (r *Registry) AdapterFor(command control.Command) (control.Adapter, bool) {
	if _, disabled := r.disabledReason(command.Type); disabled {
		return nil, false
	}
	switch command.Type {
	case CommandPoolPause, CommandPoolResume, CommandPoolDrain, CommandPoolCancelDrain:
		controller, ok := r.pool(command.TargetID)
		if !ok {
			return nil, false
		}
		return &poolAdapter{controller: controller, commandType: command.Type, fences: r.fences}, true
	case CommandRunnerTerminate, CommandRunnerRecycle:
		var parameters struct {
			Pool string `json:"pool"`
		}
		if json.Unmarshal(command.Parameters, &parameters) != nil || parameters.Pool == "" {
			return nil, false
		}
		controller, ok := r.pool(parameters.Pool)
		if !ok {
			return nil, false
		}
		return &runnerAdapter{controller: controller, fences: r.fences}, true
	case CommandHistorySync:
		r.mu.RLock()
		syncer := r.syncer
		r.mu.RUnlock()
		if syncer == nil {
			return nil, false
		}
		return &historySyncAdapter{syncer: syncer, fences: r.fences}, true
	case CommandSupportBundle:
		r.mu.RLock()
		generator := r.bundles
		r.mu.RUnlock()
		if generator == nil {
			return nil, false
		}
		return &supportBundleAdapter{generator: generator, fences: r.fences}, true
	case CommandBackupCreate:
		r.mu.RLock()
		generator := r.backups
		r.mu.RUnlock()
		if generator == nil {
			return nil, false
		}
		return &backupAdapter{generator: generator, fences: r.fences}, true
	case CommandRestoreStage:
		r.mu.RLock()
		stager := r.restores
		r.mu.RUnlock()
		if stager == nil {
			return nil, false
		}
		return &restoreAdapter{stager: stager, fences: r.fences}, true
	case CommandUpdateStage:
		r.mu.RLock()
		stager := r.updates
		r.mu.RUnlock()
		if stager == nil {
			return nil, false
		}
		return &updateAdapter{stager: stager, fences: r.fences}, true
	default:
		return nil, false
	}
}

func (r *Registry) Validate(command control.Command) error {
	if reason, disabled := r.disabledReason(command.Type); disabled {
		return UnsupportedCapabilityReason(command.Type, reason)
	}
	switch command.Type {
	case CommandPoolPause, CommandPoolResume, CommandPoolDrain, CommandPoolCancelDrain:
		if _, ok := r.pool(command.TargetID); !ok {
			return UnsupportedCapability(command.Type)
		}
		return nil
	case CommandRunnerTerminate, CommandRunnerRecycle:
		var parameters struct {
			Pool string `json:"pool"`
		}
		if json.Unmarshal(command.Parameters, &parameters) != nil || parameters.Pool == "" {
			return errors.New("runner command pool is required")
		}
		controller, ok := r.pool(parameters.Pool)
		if !ok {
			return UnsupportedCapability(command.Type)
		}
		if !contains(controller.ActiveSessions(), command.TargetID) {
			return fmt.Errorf("%w: runner session is not active", control.ErrStateConflict)
		}
		return nil
	case CommandHistorySync:
		r.mu.RLock()
		supported := r.syncer != nil
		r.mu.RUnlock()
		if !supported {
			return UnsupportedCapability(command.Type)
		}
		return nil
	case CommandSupportBundle:
		r.mu.RLock()
		supported := r.bundles != nil
		r.mu.RUnlock()
		if !supported {
			return UnsupportedCapability(command.Type)
		}
		if command.TargetType != "system" || command.TargetID != "support-bundles" {
			return errors.New("support bundle command requires the system support-bundles target")
		}
		var request supportbundle.Request
		if err := json.Unmarshal(command.Parameters, &request); err != nil {
			return errors.New("support bundle parameters are invalid")
		}
		if _, err := supportbundle.ValidateRequest(request, time.Now().UTC()); err != nil {
			return err
		}
		return nil
	case CommandBackupCreate:
		r.mu.RLock()
		supported := r.backups != nil
		r.mu.RUnlock()
		if !supported {
			return UnsupportedCapability(command.Type)
		}
		if command.TargetType != "system" || command.TargetID != "backups" {
			return errors.New("backup command requires the system backups target")
		}
		var request backup.Request
		if err := json.Unmarshal(command.Parameters, &request); err != nil {
			return errors.New("backup parameters are invalid")
		}
		return backup.ValidateRequest(request)
	case CommandRestoreStage:
		r.mu.RLock()
		supported := r.restores != nil
		r.mu.RUnlock()
		if !supported {
			return UnsupportedCapability(command.Type)
		}
		if command.TargetType != "system" || command.TargetID != "restore" {
			return errors.New("restore command requires the system restore target")
		}
		var request restore.Request
		if err := json.Unmarshal(command.Parameters, &request); err != nil {
			return errors.New("restore parameters are invalid")
		}
		return restore.ValidateRequest(request)
	case CommandUpdateStage:
		r.mu.RLock()
		supported := r.updates != nil
		r.mu.RUnlock()
		if !supported {
			return UnsupportedCapability(command.Type)
		}
		if command.TargetType != "system" || command.TargetID != "updates" {
			return errors.New("update command requires the system updates target")
		}
		var request update.Request
		if err := json.Unmarshal(command.Parameters, &request); err != nil {
			return errors.New("update parameters are invalid")
		}
		return update.ValidateRequest(request)
	default:
		return UnsupportedCapability(command.Type)
	}
}

func (r *Registry) pool(name string) (PoolController, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	controller, ok := r.pools[name]
	return controller, ok
}

func (r *Registry) disabledReason(commandType string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reason, disabled := r.disabled[commandType]
	return reason, disabled
}

type fenceGuard struct {
	mu     sync.Mutex
	tokens map[string]int64
}

func (g *fenceGuard) accept(domain string, token int64) error {
	if domain == "" || token < 1 {
		return control.ErrStaleFence
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if token < g.tokens[domain] {
		return control.ErrStaleFence
	}
	if token > g.tokens[domain] {
		g.tokens[domain] = token
	}
	return nil
}

type poolAdapter struct {
	controller  PoolController
	commandType string
	fences      *fenceGuard
}

func (a *poolAdapter) Prepare(_ context.Context, invocation control.Invocation) (json.RawMessage, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return nil, err
	}
	return poolState(a.controller)
}

func (a *poolAdapter) Execute(ctx context.Context, invocation control.Invocation) (control.EffectResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	switch a.commandType {
	case CommandPoolPause:
		a.controller.Pause()
	case CommandPoolResume, CommandPoolCancelDrain:
		a.controller.Resume()
	case CommandPoolDrain:
		if err := a.controller.Drain(ctx); err != nil {
			return control.EffectResult{Disposition: control.EffectUnknown}, err
		}
	default:
		return control.EffectResult{
			Disposition: control.EffectFailed,
			Error:       "unsupported pool command",
		}, nil
	}
	outcome, err := poolState(a.controller)
	return control.EffectResult{Disposition: control.EffectSucceeded, Outcome: outcome}, err
}

func (a *poolAdapter) Reconcile(_ context.Context, invocation control.Invocation) (control.ReconcileResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	paused := a.controller.Paused()
	active := len(a.controller.ActiveSessions())
	result := control.ReconcileResult{}
	switch a.commandType {
	case CommandPoolPause:
		if paused {
			result.Disposition = control.ReconcileApplied
		} else {
			result.Disposition = control.ReconcileAbsent
		}
	case CommandPoolResume, CommandPoolCancelDrain:
		if !paused {
			result.Disposition = control.ReconcileApplied
		} else {
			result.Disposition = control.ReconcileAbsent
		}
	case CommandPoolDrain:
		switch {
		case paused && active == 0:
			result.Disposition = control.ReconcileApplied
		case !paused:
			result.Disposition = control.ReconcileAbsent
		default:
			result.Disposition = control.ReconcileUnknown
			result.Error = "pool is paused but active runners remain"
		}
	default:
		result.Disposition = control.ReconcileUnknown
	}
	result.Outcome, _ = poolState(a.controller)
	return result, nil
}

type runnerAdapter struct {
	controller PoolController
	fences     *fenceGuard
}

func (a *runnerAdapter) Prepare(_ context.Context, invocation control.Invocation) (json.RawMessage, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return nil, err
	}
	active := contains(a.controller.ActiveSessions(), invocation.TargetID)
	return json.Marshal(map[string]any{
		"pool": a.controller.Name(), "active": active,
	})
}

func (a *runnerAdapter) Execute(ctx context.Context, invocation control.Invocation) (control.EffectResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	err := a.controller.Terminate(ctx, invocation.TargetID)
	if err != nil && !errors.Is(err, pool.ErrRunnerSessionNotFound) {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	outcome, marshalErr := json.Marshal(map[string]any{
		"pool": a.controller.Name(), "active": false,
	})
	return control.EffectResult{Disposition: control.EffectSucceeded, Outcome: outcome}, marshalErr
}

func (a *runnerAdapter) Reconcile(_ context.Context, invocation control.Invocation) (control.ReconcileResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	active := contains(a.controller.ActiveSessions(), invocation.TargetID)
	outcome, err := json.Marshal(map[string]any{
		"pool": a.controller.Name(), "active": active,
	})
	if active {
		return control.ReconcileResult{
			Disposition: control.ReconcileUnknown, Outcome: outcome,
			Error: "runner session is still active",
		}, err
	}
	return control.ReconcileResult{
		Disposition: control.ReconcileApplied, Outcome: outcome,
	}, err
}

type historySyncAdapter struct {
	syncer HistorySyncer
	fences *fenceGuard
}

type supportBundleAdapter struct {
	generator SupportBundleGenerator
	fences    *fenceGuard
}

type backupAdapter struct {
	generator BackupGenerator
	fences    *fenceGuard
}

type restoreAdapter struct {
	stager RestoreStager
	fences *fenceGuard
}

type updateAdapter struct {
	stager UpdateStager
	fences *fenceGuard
}

func (a *updateAdapter) Prepare(
	ctx context.Context, invocation control.Invocation,
) (json.RawMessage, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return nil, err
	}
	exists, err := a.stager.Exists(ctx, invocation.CommandID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]bool{"exists": exists})
}

func (a *updateAdapter) Execute(
	ctx context.Context, invocation control.Invocation,
) (control.EffectResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	if metadata, err := a.stager.Metadata(ctx, invocation.CommandID); err == nil {
		outcome, marshalErr := json.Marshal(metadata)
		if metadata.State == update.StateStaged || metadata.State == update.StateSucceeded {
			return control.EffectResult{
				Disposition: control.EffectSucceeded, Outcome: outcome,
			}, marshalErr
		}
		if metadata.State == update.StateFailed || metadata.State == update.StateRolledBack {
			errorText := metadata.Error
			if errorText == "" {
				errorText = metadata.RollbackReason
			}
			return control.EffectResult{
				Disposition: control.EffectFailed, Outcome: outcome, Error: errorText,
			}, marshalErr
		}
	} else if !errors.Is(err, update.ErrNotFound) {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	var request update.Request
	if err := json.Unmarshal(invocation.Parameters, &request); err != nil {
		return control.EffectResult{Disposition: control.EffectFailed, Error: "invalid update parameters"}, nil
	}
	metadata, err := a.stager.Stage(ctx, invocation.CommandID, request)
	if err != nil {
		return control.EffectResult{Disposition: control.EffectFailed, Error: err.Error()}, nil
	}
	outcome, err := json.Marshal(metadata)
	return control.EffectResult{Disposition: control.EffectSucceeded, Outcome: outcome}, err
}

func (a *updateAdapter) Reconcile(
	ctx context.Context, invocation control.Invocation,
) (control.ReconcileResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	metadata, err := a.stager.Metadata(ctx, invocation.CommandID)
	if errors.Is(err, update.ErrNotFound) {
		return control.ReconcileResult{Disposition: control.ReconcileAbsent}, nil
	}
	if err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	outcome, err := json.Marshal(metadata)
	switch metadata.State {
	case update.StateStaged, update.StateActivating, update.StateSucceeded:
		return control.ReconcileResult{
			Disposition: control.ReconcileApplied, Outcome: outcome,
		}, err
	case update.StateFailed, update.StateRolledBack:
		errorText := metadata.Error
		if errorText == "" {
			errorText = metadata.RollbackReason
		}
		return control.ReconcileResult{
			Disposition: control.ReconcileAbsent, Outcome: outcome, Error: errorText,
		}, err
	default:
		return control.ReconcileResult{
			Disposition: control.ReconcileUnknown, Outcome: outcome,
		}, err
	}
}

func (a *restoreAdapter) Prepare(
	ctx context.Context, invocation control.Invocation,
) (json.RawMessage, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return nil, err
	}
	exists, err := a.stager.Exists(ctx, invocation.CommandID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]bool{"exists": exists})
}

func (a *restoreAdapter) Execute(
	ctx context.Context, invocation control.Invocation,
) (control.EffectResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	if metadata, err := a.stager.Metadata(ctx, invocation.CommandID); err == nil {
		outcome, marshalErr := json.Marshal(metadata)
		if metadata.State == restore.StateStaged || metadata.State == restore.StateSucceeded {
			return control.EffectResult{
				Disposition: control.EffectSucceeded, Outcome: outcome,
			}, marshalErr
		}
		if metadata.State == restore.StateFailed || metadata.State == restore.StateRolledBack {
			return control.EffectResult{
				Disposition: control.EffectFailed, Outcome: outcome, Error: metadata.Error,
			}, marshalErr
		}
	} else if !errors.Is(err, restore.ErrNotFound) {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	var request restore.Request
	if err := json.Unmarshal(invocation.Parameters, &request); err != nil {
		return control.EffectResult{Disposition: control.EffectFailed, Error: "invalid restore parameters"}, nil
	}
	metadata, err := a.stager.Stage(ctx, invocation.CommandID, request)
	if err != nil {
		return control.EffectResult{Disposition: control.EffectFailed, Error: err.Error()}, nil
	}
	outcome, err := json.Marshal(metadata)
	return control.EffectResult{Disposition: control.EffectSucceeded, Outcome: outcome}, err
}

func (a *restoreAdapter) Reconcile(
	ctx context.Context, invocation control.Invocation,
) (control.ReconcileResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	metadata, err := a.stager.Metadata(ctx, invocation.CommandID)
	if errors.Is(err, restore.ErrNotFound) {
		return control.ReconcileResult{Disposition: control.ReconcileAbsent}, nil
	}
	if err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	outcome, err := json.Marshal(metadata)
	switch metadata.State {
	case restore.StateStaged, restore.StateActivating, restore.StateSucceeded:
		return control.ReconcileResult{
			Disposition: control.ReconcileApplied, Outcome: outcome,
		}, err
	case restore.StateFailed, restore.StateRolledBack:
		errorText := metadata.Error
		if errorText == "" {
			errorText = metadata.RollbackReason
		}
		return control.ReconcileResult{
			Disposition: control.ReconcileAbsent, Outcome: outcome,
			Error: errorText,
		}, err
	default:
		return control.ReconcileResult{
			Disposition: control.ReconcileUnknown, Outcome: outcome,
		}, err
	}
}

func (a *backupAdapter) Prepare(
	ctx context.Context, invocation control.Invocation,
) (json.RawMessage, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return nil, err
	}
	exists, err := a.generator.Exists(ctx, invocation.CommandID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]bool{"exists": exists})
}

func (a *backupAdapter) Execute(
	ctx context.Context, invocation control.Invocation,
) (control.EffectResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	if metadata, err := a.generator.Metadata(ctx, invocation.CommandID); err == nil {
		outcome, marshalErr := json.Marshal(metadata)
		if metadata.State == backup.StateSucceeded {
			return control.EffectResult{
				Disposition: control.EffectSucceeded, Outcome: outcome,
			}, marshalErr
		}
		if metadata.State == backup.StateFailed {
			return control.EffectResult{
				Disposition: control.EffectFailed, Outcome: outcome, Error: metadata.Error,
			}, marshalErr
		}
	} else if !errors.Is(err, backup.ErrNotFound) {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	var request backup.Request
	if err := json.Unmarshal(invocation.Parameters, &request); err != nil {
		return control.EffectResult{Disposition: control.EffectFailed, Error: "invalid backup parameters"}, nil
	}
	metadata, err := a.generator.Generate(ctx, invocation.CommandID, request)
	if err != nil {
		return control.EffectResult{Disposition: control.EffectFailed, Error: err.Error()}, nil
	}
	outcome, err := json.Marshal(metadata)
	return control.EffectResult{Disposition: control.EffectSucceeded, Outcome: outcome}, err
}

func (a *backupAdapter) Reconcile(
	ctx context.Context, invocation control.Invocation,
) (control.ReconcileResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	metadata, err := a.generator.Metadata(ctx, invocation.CommandID)
	if errors.Is(err, backup.ErrNotFound) {
		return control.ReconcileResult{Disposition: control.ReconcileAbsent}, nil
	}
	if err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	outcome, err := json.Marshal(metadata)
	switch metadata.State {
	case backup.StateSucceeded:
		return control.ReconcileResult{
			Disposition: control.ReconcileApplied, Outcome: outcome,
		}, err
	case backup.StateFailed:
		return control.ReconcileResult{
			Disposition: control.ReconcileAbsent, Outcome: outcome, Error: metadata.Error,
		}, err
	default:
		return control.ReconcileResult{
			Disposition: control.ReconcileUnknown, Outcome: outcome,
		}, err
	}
}

func (a *supportBundleAdapter) Prepare(
	ctx context.Context, invocation control.Invocation,
) (json.RawMessage, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return nil, err
	}
	exists, err := a.generator.Exists(ctx, invocation.CommandID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]bool{"exists": exists})
}

func (a *supportBundleAdapter) Execute(
	ctx context.Context, invocation control.Invocation,
) (control.EffectResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	if metadata, err := a.generator.Metadata(ctx, invocation.CommandID); err == nil {
		outcome, marshalErr := json.Marshal(metadata)
		return control.EffectResult{
			Disposition: control.EffectSucceeded, Outcome: outcome,
		}, marshalErr
	} else if !errors.Is(err, supportbundle.ErrNotFound) {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	var request supportbundle.Request
	if err := json.Unmarshal(invocation.Parameters, &request); err != nil {
		return control.EffectResult{Disposition: control.EffectFailed, Error: "invalid support bundle parameters"}, nil
	}
	metadata, err := a.generator.Generate(ctx, invocation.CommandID, request)
	if err != nil {
		return control.EffectResult{Disposition: control.EffectFailed, Error: err.Error()}, nil
	}
	outcome, err := json.Marshal(metadata)
	return control.EffectResult{Disposition: control.EffectSucceeded, Outcome: outcome}, err
}

func (a *supportBundleAdapter) Reconcile(
	ctx context.Context, invocation control.Invocation,
) (control.ReconcileResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	metadata, err := a.generator.Metadata(ctx, invocation.CommandID)
	if errors.Is(err, supportbundle.ErrNotFound) {
		return control.ReconcileResult{Disposition: control.ReconcileAbsent}, nil
	}
	if err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	outcome, err := json.Marshal(metadata)
	return control.ReconcileResult{
		Disposition: control.ReconcileApplied, Outcome: outcome,
	}, err
}

func (a *historySyncAdapter) Prepare(_ context.Context, invocation control.Invocation) (json.RawMessage, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"sync":"requested"}`), nil
}

func (a *historySyncAdapter) Execute(ctx context.Context, invocation control.Invocation) (control.EffectResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	report, err := a.syncer.Sync(ctx)
	if err != nil {
		return control.EffectResult{Disposition: control.EffectUnknown}, err
	}
	outcome, marshalErr := syncOutcome(report)
	if report.Degraded {
		return control.EffectResult{
			Disposition: control.EffectFailed, Outcome: outcome,
			Error: "one or more repositories failed to synchronize",
		}, marshalErr
	}
	return control.EffectResult{Disposition: control.EffectSucceeded, Outcome: outcome}, marshalErr
}

func (a *historySyncAdapter) Reconcile(_ context.Context, invocation control.Invocation) (control.ReconcileResult, error) {
	if err := a.fences.accept(invocation.ConflictDomain, invocation.FencingToken); err != nil {
		return control.ReconcileResult{Disposition: control.ReconcileUnknown}, err
	}
	return control.ReconcileResult{
		Disposition: control.ReconcileUnknown,
		Error:       "history synchronization has no durable command marker",
	}, nil
}

func poolState(controller PoolController) (json.RawMessage, error) {
	sessions := controller.ActiveSessions()
	sort.Strings(sessions)
	return json.Marshal(map[string]any{
		"pool": controller.Name(), "paused": controller.Paused(),
		"active_sessions": sessions, "active_count": len(sessions),
	})
}

func syncOutcome(report historysync.Report) (json.RawMessage, error) {
	type repository struct {
		Name  string `json:"name"`
		Runs  int    `json:"runs"`
		Jobs  int    `json:"jobs"`
		Error bool   `json:"error"`
	}
	repositories := make([]repository, 0, len(report.Repositories))
	for _, item := range report.Repositories {
		repositories = append(repositories, repository{
			Name: item.Repository, Runs: item.Runs, Jobs: item.Jobs,
			Error: item.Error != nil,
		})
	}
	return json.Marshal(map[string]any{
		"degraded": report.Degraded, "repositories": repositories,
	})
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var _ PoolController = (*pool.Launcher)(nil)

var ErrCapabilityUnsupported = errors.New("runtime capability is unsupported")

func UnsupportedCapability(commandType string) error {
	return UnsupportedCapabilityReason(
		commandType, "runtime capability is unavailable in this provisioning mode",
	)
}

func UnsupportedCapabilityReason(commandType, reason string) error {
	return fmt.Errorf("%w: capability %q is unsupported: %s",
		ErrCapabilityUnsupported, commandType, reason)
}
