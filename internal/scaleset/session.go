package scaleset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/GerardSmit/multirunner/internal/backend"
)

// SessionOptions describes one pool's scale set session.
type SessionOptions struct {
	// Name is the runner scale set name. It is reused across restarts so a
	// restart does not churn registrations or strand queued jobs.
	Name string
	// RunnerGroup is the group the scale set lives in. Empty means default.
	RunnerGroup string
	// Labels are advertised by the scale set, so runs-on can target it.
	Labels []string
	// Target is the GitHub target URL used as a reconciliation boundary.
	Target string
	// Pool distinguishes backend resources when names and labels overlap.
	Pool string
	// Instance is the stable host identity. Empty uses the OS hostname.
	Instance string
	// Launch describes how to start each runner.
	Launch Options
	// OnStateChange reports whether this required session is serving,
	// backing off, or permanently unavailable.
	OnStateChange func(SessionAvailability)
}

// Run holds a long-poll session open for one pool and provisions runners onto
// be until ctx is cancelled.
//
// This is the whole scaleset provisioning mode: GitHub decides how many runners
// should exist, and every existing backend starts them unchanged.
func Run(
	ctx context.Context,
	client *scaleset.Client,
	be backend.Backend,
	opts SessionOptions,
	logger *slog.Logger,
) error {
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	if backend.OwnedRunnerStoreFor(be) == nil {
		return permanentSessionError{fmt.Errorf(
			"backend %q does not support scale-set ownership reconciliation", be.Name())}
	}
	if err := be.EnsureImage(sessionCtx, opts.Launch.Image); err != nil {
		return fmt.Errorf("ensure image for %q: %w", opts.Name, err)
	}

	groupID, err := runnerGroupID(sessionCtx, client, opts.RunnerGroup)
	if err != nil {
		return classifySessionError(err)
	}

	set, err := ensureScaleSet(sessionCtx, client, opts, groupID, logger)
	if err != nil {
		return classifySessionError(err)
	}
	client.SetSystemInfo(systemInfo(set.ID))

	instance := opts.Instance
	if instance == "" {
		instance, err = os.Hostname()
		if err != nil || instance == "" {
			instance = "multirunner"
		}
	}
	poolName := opts.Pool
	if poolName == "" {
		poolName = opts.Name
	}
	ownership := backend.RunnerOwnership{
		Instance:   instance,
		Target:     opts.Target,
		Pool:       poolName,
		ScaleSetID: set.ID,
	}
	// One host can hold several sessions, so an ambiguous owner makes a stuck
	// session impossible to attribute back to a pool.
	owner := fmt.Sprintf("%s-%s", instance, opts.Name)

	launchOpts := opts.Launch
	launchOpts.ScaleSetID = set.ID
	launchOpts.Ownership = ownership
	launchOpts.Logger = logger.WithGroup("launcher")
	launcher := New(sessionCtx, client, be, launchOpts)
	defer func() {
		if err := launcher.Shutdown(context.WithoutCancel(ctx)); err != nil {
			logger.Warn("stop scale set runners failed", slog.String("scaleSet", opts.Name), slog.Any("error", err))
		}
	}()
	reconciled, err := launcher.Reconcile(sessionCtx)
	if err != nil {
		return classifySessionError(fmt.Errorf("reconcile scale set %q: %w", opts.Name, err))
	}
	if reconciled > 0 {
		logger.Info("reconciled runners from previous process",
			slog.String("scaleSet", opts.Name),
			slog.Int("count", reconciled))
	}
	go launcher.reconcilePeriodically(sessionCtx)

	logger.Info("listening for jobs",
		slog.String("scaleSet", opts.Name),
		slog.Int("scaleSetID", set.ID),
		slog.String("backend", be.Name()),
	)

	listenerSession := listenerSession{
		client: client, scaleSetID: set.ID, owner: owner,
		opts: opts, launcher: launcher, logger: logger,
	}
	return superviseSession(sessionCtx, SupervisedSession{
		Name:          opts.Name,
		Run:           listenerSession.run,
		OnStateChange: opts.OnStateChange,
	}, defaultSupervisorConfig(), logger)
}

type listenerSession struct {
	client     *scaleset.Client
	scaleSetID int
	owner      string
	opts       SessionOptions
	launcher   *Launcher
	logger     *slog.Logger
}

func (s listenerSession) run(ctx context.Context, available func()) error {
	session, err := s.client.MessageSessionClient(ctx, s.scaleSetID, s.owner)
	if err != nil {
		return classifySessionError(fmt.Errorf("open message session for %q: %w", s.opts.Name, err))
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := session.Close(closeCtx); err != nil {
			s.logger.Warn("close message session failed", slog.String("scaleSet", s.opts.Name), slog.Any("error", err))
		}
	}()

	l, err := listener.New(session, listener.Config{
		ScaleSetID: s.scaleSetID,
		MaxRunners: s.opts.Launch.MaxRunners,
		Logger:     s.logger.WithGroup("listener"),
	})
	if err != nil {
		return classifySessionError(fmt.Errorf("create listener for %q: %w", s.opts.Name, err))
	}
	available()
	if err := l.Run(ctx, s.launcher); err != nil && !errors.Is(err, context.Canceled) {
		return classifySessionError(fmt.Errorf("listener for %q stopped: %w", s.opts.Name, err))
	}
	return nil
}

func classifySessionError(err error) error {
	if isPermanentSessionError(err) {
		return permanentSessionError{err}
	}
	return err
}

type permanentSessionError struct {
	error
}

func runnerGroupID(ctx context.Context, client *scaleset.Client, name string) (int, error) {
	if name == "" || name == scaleset.DefaultRunnerGroup {
		return 1, nil
	}
	group, err := client.GetRunnerGroupByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("resolve runner group %q: %w", name, err)
	}
	return group.ID, nil
}

// ensureScaleSet reuses an existing scale set with the same name rather than
// creating a second one, so restarts do not churn registrations and jobs
// already queued against the name are still served.
func ensureScaleSet(
	ctx context.Context,
	client scaleSetManager,
	opts SessionOptions,
	groupID int,
	logger *slog.Logger,
) (*scaleset.RunnerScaleSet, error) {
	existing, err := client.GetRunnerScaleSet(ctx, groupID, opts.Name)
	if err == nil && existing != nil {
		desired := desiredScaleSet(opts, groupID)
		if scaleSetMatches(existing, desired) {
			logger.Info("reusing scale set", slog.String("name", opts.Name), slog.Int("id", existing.ID))
			return existing, nil
		}
		updated, updateErr := client.UpdateRunnerScaleSet(ctx, existing.ID, desired)
		if updateErr != nil {
			return nil, fmt.Errorf("update scale set %q: %w", opts.Name, updateErr)
		}
		logger.Info("updated scale set", slog.String("name", opts.Name), slog.Int("id", updated.ID))
		return updated, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup scale set %q: %w", opts.Name, err)
	}

	created, err := client.CreateRunnerScaleSet(ctx, desiredScaleSet(opts, groupID))
	if err != nil {
		return nil, fmt.Errorf("create scale set %q: %w", opts.Name, err)
	}
	logger.Info("created scale set", slog.String("name", opts.Name), slog.Int("id", created.ID))
	return created, nil
}

type scaleSetManager interface {
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, runnerScaleSetName string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, runnerScaleSet *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(ctx context.Context, runnerScaleSetID int, runnerScaleSet *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
}

func desiredScaleSet(opts SessionOptions, groupID int) *scaleset.RunnerScaleSet {
	labels := make([]scaleset.Label, 0, len(opts.Labels))
	for _, name := range opts.Labels {
		labels = append(labels, scaleset.Label{Name: name, Type: "System"})
	}
	if len(labels) == 0 {
		labels = append(labels, scaleset.Label{Name: opts.Name, Type: "System"})
	}
	return &scaleset.RunnerScaleSet{
		Name:          opts.Name,
		RunnerGroupID: groupID,
		Labels:        labels,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
}

func scaleSetMatches(existing, desired *scaleset.RunnerScaleSet) bool {
	if existing.Name != desired.Name ||
		existing.RunnerGroupID != desired.RunnerGroupID ||
		existing.RunnerSetting.DisableUpdate != desired.RunnerSetting.DisableUpdate ||
		len(existing.Labels) != len(desired.Labels) {
		return false
	}
	labels := make(map[string]int, len(existing.Labels))
	for _, label := range existing.Labels {
		labels[label.Name]++
	}
	for _, label := range desired.Labels {
		labels[label.Name]--
	}
	for _, count := range labels {
		if count != 0 {
			return false
		}
	}
	return true
}

func systemInfo(scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     "multirunner",
		Subsystem:  "multirunner",
		ScaleSetID: scaleSetID,
	}
}
