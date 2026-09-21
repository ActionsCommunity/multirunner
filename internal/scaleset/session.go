package scaleset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
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
	// Launch describes how to start each runner.
	Launch Options
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
	if err := be.EnsureImage(ctx, opts.Launch.Image); err != nil {
		return fmt.Errorf("ensure image for %q: %w", opts.Name, err)
	}

	groupID, err := runnerGroupID(ctx, client, opts.RunnerGroup)
	if err != nil {
		return err
	}

	set, err := ensureScaleSet(ctx, client, opts, groupID, logger)
	if err != nil {
		return err
	}
	client.SetSystemInfo(systemInfo(set.ID))

	owner, err := os.Hostname()
	if err != nil || owner == "" {
		owner = "multirunner"
	}
	// One host can hold several sessions, so an ambiguous owner makes a stuck
	// session impossible to attribute back to a pool.
	owner = fmt.Sprintf("%s-%s", owner, opts.Name)

	session, err := openSession(ctx, client, set.ID, owner, opts.Name, logger)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := session.Close(closeCtx); err != nil {
			logger.Warn("close message session failed", slog.String("scaleSet", opts.Name), slog.Any("error", err))
		}
	}()

	l, err := listener.New(session, listener.Config{
		ScaleSetID: set.ID,
		MaxRunners: opts.Launch.MaxRunners,
		Logger:     logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("create listener for %q: %w", opts.Name, err)
	}

	launchOpts := opts.Launch
	launchOpts.ScaleSetID = set.ID
	launchOpts.Logger = logger
	launcher := New(client, be, launchOpts)
	defer func() {
		if err := launcher.Shutdown(context.WithoutCancel(ctx)); err != nil {
			logger.Warn("stop scale set runners failed", slog.String("scaleSet", opts.Name), slog.Any("error", err))
		}
	}()

	logger.Info("listening for jobs",
		slog.String("scaleSet", opts.Name),
		slog.Int("scaleSetID", set.ID),
		slog.String("backend", be.Name()),
	)

	if err := l.Run(ctx, launcher); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("listener for %q stopped: %w", opts.Name, err)
	}
	return nil
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

// sessionConflictWait bounds how long a restart waits for GitHub to release the
// previous listener session, and sessionRetryInterval is how often it retries.
const (
	sessionConflictWait  = 3 * time.Minute
	sessionRetryInterval = 10 * time.Second
)

// openSession opens the listener session, waiting out the conflict a restart
// leaves behind. A scale set allows one active session, and GitHub keeps the
// previous one for about a minute after the process holding it goes away - so
// every restart that did not shut down cleanly hits a 409 that resolves itself.
// Failing immediately on that turns a normal restart into an outage, and the
// library's raw error does not say it is temporary.
func openSession(
	ctx context.Context,
	client *scaleset.Client,
	setID int,
	owner, name string,
	logger *slog.Logger,
) (*scaleset.MessageSessionClient, error) {
	deadline := time.Now().Add(sessionConflictWait)
	waiting := false
	for {
		session, err := client.MessageSessionClient(ctx, setID, owner)
		if err == nil {
			return session, nil
		}
		if !isSessionConflict(err) {
			return nil, fmt.Errorf("open message session for %q: %w", name, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("scale set %q still has an active session after %s; "+
				"another multirunner process is running against it, or a previous one has not been released yet: %w",
				name, sessionConflictWait, err)
		}
		if !waiting {
			waiting = true
			logger.Info("waiting for the previous listener session to be released",
				slog.String("scaleSet", name),
				slog.Duration("timeout", sessionConflictWait),
			)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sessionRetryInterval):
		}
	}
}

// isSessionConflict reports whether err is the one-active-session 409. The
// library surfaces it as a formatted string rather than a typed error, so the
// status and the exception name are matched together to avoid catching an
// unrelated conflict.
func isSessionConflict(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "409 Conflict") &&
		strings.Contains(msg, "RunnerScaleSetSessionConflictException")
}
