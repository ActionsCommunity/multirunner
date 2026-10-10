package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

type Store interface {
	ResumePendingCommands(context.Context, string) ([]Command, error)
	ExpireQueuedCommands(context.Context, string) ([]Command, error)
	ClaimNextCommand(context.Context, string, time.Duration) (Command, error)
	TransitionCommand(context.Context, string, Transition) (Command, error)
	HeartbeatCommand(context.Context, string, string, int64, time.Duration) error
	ReconcileExpiredCommands(context.Context, string, time.Duration) ([]Command, error)
}

type Invocation struct {
	CommandID      string
	CommandType    string
	HostID         string
	TargetType     string
	TargetID       string
	ConflictDomain string
	Parameters     json.RawMessage
	Attempt        int
	FencingToken   int64
}

type EffectDisposition string

const (
	EffectSucceeded EffectDisposition = "succeeded"
	EffectFailed    EffectDisposition = "failed"
	EffectUnknown   EffectDisposition = "unknown"
)

type EffectResult struct {
	Disposition EffectDisposition
	Outcome     json.RawMessage
	Error       string
}

type ReconcileDisposition string

const (
	ReconcileApplied ReconcileDisposition = "applied"
	ReconcileAbsent  ReconcileDisposition = "absent"
	ReconcileUnknown ReconcileDisposition = "unknown"
)

type ReconcileResult struct {
	Disposition ReconcileDisposition
	Outcome     json.RawMessage
	Error       string
}

type Adapter interface {
	Prepare(context.Context, Invocation) (json.RawMessage, error)
	Execute(context.Context, Invocation) (EffectResult, error)
	Reconcile(context.Context, Invocation) (ReconcileResult, error)
}

type AdapterResolver interface {
	AdapterFor(Command) (Adapter, bool)
}

type AdapterMap map[string]Adapter

func (m AdapterMap) AdapterFor(command Command) (Adapter, bool) {
	adapter, ok := m[command.Type]
	return adapter, ok
}

type ExecutorHooks struct {
	BeforeEffect func(Command) error
	AfterEffect  func(Command, EffectResult) error
}

type ExecutorOptions struct {
	Owner        string
	Lease        time.Duration
	PollInterval time.Duration
	Hooks        ExecutorHooks
}

type Executor struct {
	store        Store
	adapters     AdapterResolver
	owner        string
	lease        time.Duration
	pollInterval time.Duration
	hooks        ExecutorHooks
}

func NewExecutor(store Store, adapters AdapterResolver, options ExecutorOptions) (*Executor, error) {
	if store == nil || adapters == nil {
		return nil, errors.New("command store and adapter resolver are required")
	}
	if strings.TrimSpace(options.Owner) == "" {
		return nil, errors.New("executor owner is required")
	}
	if options.Lease <= 0 {
		options.Lease = 30 * time.Second
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	return &Executor{
		store: store, adapters: adapters, owner: options.Owner,
		lease: options.Lease, pollInterval: options.PollInterval,
		hooks: options.Hooks,
	}, nil
}

func (e *Executor) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()
	for {
		worked, err := e.RunOnce(ctx)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *Executor) RunOnce(ctx context.Context) (bool, error) {
	resumed, err := e.store.ResumePendingCommands(ctx, e.owner)
	if err != nil {
		return false, fmt.Errorf("resume pending commands: %w", err)
	}
	expired, err := e.store.ExpireQueuedCommands(ctx, e.owner)
	if err != nil {
		return len(resumed) > 0, fmt.Errorf("expire queued commands: %w", err)
	}
	recovered, err := e.store.ReconcileExpiredCommands(ctx, e.owner, e.lease)
	if err != nil {
		return len(resumed) > 0 || len(expired) > 0, fmt.Errorf("recover expired commands: %w", err)
	}
	for _, command := range recovered {
		if err := e.reconcile(ctx, command); err != nil {
			return true, err
		}
	}
	command, err := e.store.ClaimNextCommand(ctx, e.owner, e.lease)
	if errors.Is(err, ErrNotFound) {
		return len(resumed) > 0 || len(expired) > 0 || len(recovered) > 0, nil
	}
	if err != nil {
		return len(resumed) > 0 || len(expired) > 0 || len(recovered) > 0,
			fmt.Errorf("claim command: %w", err)
	}
	if err := e.execute(ctx, command); err != nil {
		return true, err
	}
	return true, nil
}

func (e *Executor) execute(ctx context.Context, command Command) error {
	effectContext, cancel := commandEffectContext(ctx, command)
	defer cancel()
	if commandDeadlineExpired(command, time.Now()) {
		return e.interruptBeforeEffect(ctx, command, StateClaimed)
	}
	adapter, ok := e.adapters.AdapterFor(command)
	if !ok {
		_, err := e.store.TransitionCommand(ctx, command.ID, Transition{
			ExpectedState: StateClaimed, To: StateFailed,
			ActorKind: operations.ActorSystem, ActorID: e.owner,
			FencingToken: command.FencingToken,
			Error:        "command capability is unsupported",
			Detail:       json.RawMessage(`{"code":"capability_unsupported"}`),
		})
		return err
	}
	invocation := invocationFor(command)
	beforeState, err := adapter.Prepare(effectContext, invocation)
	if err != nil {
		if commandDeadlineExpired(command, time.Now()) &&
			errors.Is(effectContext.Err(), context.DeadlineExceeded) {
			return e.interruptBeforeEffect(ctx, command, StateClaimed)
		}
		_, transitionErr := e.store.TransitionCommand(ctx, command.ID, Transition{
			ExpectedState: StateClaimed, To: StateFailed,
			ActorKind: operations.ActorSystem, ActorID: e.owner,
			FencingToken: command.FencingToken, Error: err.Error(),
			Detail: json.RawMessage(`{"phase":"prepare"}`),
		})
		if transitionErr != nil {
			return errors.Join(err, transitionErr)
		}
		return nil
	}
	if commandDeadlineExpired(command, time.Now()) {
		return e.interruptBeforeEffect(ctx, command, StateClaimed)
	}
	command, err = e.store.TransitionCommand(ctx, command.ID, Transition{
		ExpectedState: StateClaimed, To: StateRunning,
		ActorKind: operations.ActorSystem, ActorID: e.owner,
		FencingToken: command.FencingToken, BeforeState: beforeState,
	})
	if err != nil {
		return fmt.Errorf("mark command running: %w", err)
	}
	if e.hooks.BeforeEffect != nil {
		if err := e.hooks.BeforeEffect(command); err != nil {
			return err
		}
	}
	if commandDeadlineExpired(command, time.Now()) {
		return e.interruptBeforeEffect(ctx, command, StateRunning)
	}
	effect, executeErr := e.executeWithHeartbeat(effectContext, adapter, command)
	if e.hooks.AfterEffect != nil {
		if err := e.hooks.AfterEffect(command, effect); err != nil {
			return err
		}
	}
	transition := Transition{
		ExpectedState: StateRunning, ActorKind: operations.ActorSystem,
		ActorID: e.owner, FencingToken: command.FencingToken,
		Outcome: effect.Outcome, Error: effect.Error,
	}
	switch effect.Disposition {
	case EffectSucceeded:
		transition.To = StateSucceeded
	case EffectFailed:
		transition.To = StateFailed
	case EffectUnknown:
		transition.To = StateUnknownOutcome
	default:
		transition.To = StateUnknownOutcome
		if transition.Error == "" {
			transition.Error = "adapter returned an invalid effect disposition"
		}
	}
	if executeErr != nil {
		transition.To = StateUnknownOutcome
		if errors.Is(executeErr, context.DeadlineExceeded) && command.TimeoutAt != nil {
			transition.Error = "command deadline expired while effect outcome was uncertain"
		} else {
			transition.Error = executeErr.Error()
		}
	}
	if _, err := e.store.TransitionCommand(ctx, command.ID, transition); err != nil {
		return fmt.Errorf("record command outcome: %w", err)
	}
	return nil
}

func (e *Executor) interruptBeforeEffect(
	ctx context.Context, command Command, expected State,
) error {
	_, err := e.store.TransitionCommand(ctx, command.ID, Transition{
		ExpectedState: expected, To: StateInterrupted,
		ActorKind: operations.ActorSystem, ActorID: e.owner,
		FencingToken: command.FencingToken,
		Error:        "command deadline expired before effect execution",
		Detail:       json.RawMessage(`{"code":"command_deadline_expired","effect_started":false}`),
	})
	if err != nil {
		return fmt.Errorf("record command deadline: %w", err)
	}
	return nil
}

func (e *Executor) reconcile(ctx context.Context, command Command) error {
	adapter, ok := e.adapters.AdapterFor(command)
	if !ok {
		_, err := e.store.TransitionCommand(ctx, command.ID, Transition{
			ExpectedState: StateReconciling, To: StateUnknownOutcome,
			ActorKind: operations.ActorSystem, ActorID: e.owner,
			FencingToken: command.FencingToken,
			Error:        "command capability is unavailable during reconciliation",
		})
		return err
	}
	result, reconcileErr := adapter.Reconcile(ctx, invocationFor(command))
	transition := Transition{
		ExpectedState: StateReconciling, ActorKind: operations.ActorSystem,
		ActorID: e.owner, FencingToken: command.FencingToken,
		Outcome: result.Outcome, Error: result.Error,
	}
	switch result.Disposition {
	case ReconcileApplied:
		transition.To = StateSucceeded
	case ReconcileAbsent:
		transition.To = StateInterrupted
	case ReconcileUnknown:
		transition.To = StateUnknownOutcome
	default:
		transition.To = StateUnknownOutcome
		if transition.Error == "" {
			transition.Error = "adapter returned an invalid reconciliation disposition"
		}
	}
	if reconcileErr != nil {
		transition.To = StateUnknownOutcome
		transition.Error = reconcileErr.Error()
	}
	if _, err := e.store.TransitionCommand(ctx, command.ID, transition); err != nil {
		return fmt.Errorf("record reconciliation outcome: %w", err)
	}
	return nil
}

func (e *Executor) executeWithHeartbeat(
	ctx context.Context, adapter Adapter, command Command,
) (EffectResult, error) {
	effectContext, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		effect EffectResult
		err    error
	}
	results := make(chan result, 1)
	go func() {
		effect, err := adapter.Execute(effectContext, invocationFor(command))
		results <- result{effect: effect, err: err}
	}()
	interval := e.lease / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case result := <-results:
			return result.effect, result.err
		case <-ctx.Done():
			return EffectResult{Disposition: EffectUnknown}, ctx.Err()
		case <-ticker.C:
			if err := e.store.HeartbeatCommand(
				ctx, command.ID, e.owner, command.FencingToken, e.lease,
			); err != nil {
				cancel()
				return EffectResult{Disposition: EffectUnknown}, fmt.Errorf("heartbeat command: %w", err)
			}
		}
	}
}

func invocationFor(command Command) Invocation {
	return Invocation{
		CommandID: command.ID, CommandType: command.Type, HostID: command.HostID,
		TargetType: command.TargetType, TargetID: command.TargetID,
		ConflictDomain: command.ConflictDomain,
		Parameters:     append(json.RawMessage(nil), command.Parameters...),
		Attempt:        command.Attempt, FencingToken: command.FencingToken,
	}
}

func commandEffectContext(ctx context.Context, command Command) (context.Context, context.CancelFunc) {
	if command.TimeoutAt == nil {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, command.TimeoutAt.UTC())
}

func commandDeadlineExpired(command Command, now time.Time) bool {
	return command.TimeoutAt != nil && !now.Before(command.TimeoutAt.UTC())
}
