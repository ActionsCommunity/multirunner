package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/GerardSmit/multirunner/internal/backup"
	"github.com/GerardSmit/multirunner/internal/buildinfo"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/restore"
	"github.com/GerardSmit/multirunner/internal/servicehost"
	"github.com/GerardSmit/multirunner/internal/update"
)

const (
	serviceWorkerMarkerEnv      = "MULTIRUNNER_INTERNAL_SERVICE_WORKER"
	serviceWorkerMarkerValue    = "supervised-v1"
	serviceWorkerConfigEnv      = "MULTIRUNNER_INTERNAL_SERVICE_CONFIG"
	serviceWorkerInstallDepsEnv = "MULTIRUNNER_INTERNAL_SERVICE_INSTALL_DEPS"
	serviceWorkerInteractiveEnv = "MULTIRUNNER_INTERNAL_SERVICE_INTERACTIVE"
	serviceWorkerStopTimeout    = 12 * time.Second
	serviceOutputDrainTimeout   = time.Second
)

type serviceProcessSpec struct {
	path               string
	args               []string
	env                []string
	secrets            []string
	verifiedExecutable *os.File
	stopTimeout        time.Duration
	drainTimeout       time.Duration
}

type supervisedProcessGroup interface {
	Kill() error
	Close() error
}

type supervisedProcessError struct {
	reason string
	tail   string
}

func (e *supervisedProcessError) Error() string {
	if e.tail == "" {
		return e.reason
	}
	return e.reason + "\noutput_tail:\n" + e.tail
}

type recoveryExhaustedError struct {
	count int
}

type restoreSupervisorHooks struct {
	activate       func(context.Context, restore.ActivationOptions) (restore.Handoff, bool, error)
	rollback       func(context.Context, restore.ActivationOptions, string, time.Time) (restore.Handoff, bool, error)
	hasRestore     func(string, []byte) (bool, error)
	activateUpdate func(context.Context, update.ActivationOptions) (update.Handoff, string, bool, error)
	rollbackUpdate func(update.ActivationOptions, string, time.Time) (update.Handoff, string, bool, error)
	hasUpdate      func(string, []byte) (bool, error)
	openUpdate     func(update.ActivationOptions, string) (*os.File, bool, error)
	run            func(context.Context, serviceProcessSpec, service.Logger) error
}

func (e *recoveryExhaustedError) Error() string {
	return fmt.Sprintf("service recovery stopped after %d failures within %s", e.count, servicehost.FailureWindow)
}

func superviseServiceWorker(ctx context.Context, configPath string, interactive, installDeps bool, logger service.Logger) error {
	return superviseServiceWorkerWithHooks(
		ctx, configPath, interactive, installDeps, logger,
		restoreSupervisorHooks{
			activate:       restore.ActivatePending,
			rollback:       restore.RollbackPending,
			hasRestore:     restore.HasActiveHandoff,
			activateUpdate: update.ActivatePending,
			rollbackUpdate: update.RollbackPending,
			hasUpdate:      update.HasActiveHandoff,
			openUpdate:     update.OpenWorker,
			run:            superviseProcess,
		},
	)
}

func superviseServiceWorkerWithHooks(
	ctx context.Context, configPath string, interactive, installDeps bool,
	logger service.Logger, hooks restoreSupervisorHooks,
) error {
	if hooks.activate == nil {
		hooks.activate = restore.ActivatePending
	}
	if hooks.rollback == nil {
		hooks.rollback = restore.RollbackPending
	}
	if hooks.hasRestore == nil {
		hooks.hasRestore = restore.HasActiveHandoff
	}
	if hooks.activateUpdate == nil {
		hooks.activateUpdate = update.ActivatePending
	}
	if hooks.rollbackUpdate == nil {
		hooks.rollbackUpdate = update.RollbackPending
	}
	if hooks.hasUpdate == nil {
		hooks.hasUpdate = update.HasActiveHandoff
	}
	if hooks.openUpdate == nil {
		hooks.openUpdate = update.OpenWorker
	}
	if hooks.run == nil {
		hooks.run = superviseProcess
	}
	if !interactive {
		count, err := servicehost.FailureCount(configPath, time.Now())
		if err != nil {
			return fmt.Errorf("read service recovery state: %w", err)
		}
		if count >= servicehost.CrashLoopFailureCount {
			return &recoveryExhaustedError{count: count}
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	activated := false
	updateActivated := false
	workerPath := ""
	var handoffKey []byte
	var updateOptions update.ActivationOptions
	var optionsErr error
	if cfg.History.Enabled {
		var keyErr error
		secretPath := consoleauth.SecretPath(cfg.History.DatabasePath)
		if interactive {
			handoffKey, keyErr = consoleauth.EnsureSecret(secretPath)
		} else {
			handoffKey, keyErr = consoleauth.LoadSecret(secretPath)
		}
		if keyErr != nil {
			return fmt.Errorf("load restore handoff key: %w", keyErr)
		}
		fallback, executableErr := os.Executable()
		if executableErr != nil {
			return fmt.Errorf("locate service executable: %w", executableErr)
		}
		updateOptions, optionsErr = updateActivationOptions(cfg, fallback, handoffKey)
		if optionsErr != nil {
			return optionsErr
		}
		if _, recoveredWorker, rolledBack, rollbackErr := hooks.rollbackUpdate(
			updateOptions,
			"service supervisor restarted before update health confirmation",
			time.Now(),
		); rollbackErr != nil {
			return fmt.Errorf("recover unconfirmed update: %w", rollbackErr)
		} else if rolledBack {
			workerPath = recoveredWorker
			if logger != nil {
				_ = logger.Warning("rolled back an unconfirmed application update before worker startup")
			}
		}
		if _, rolledBack, rollbackErr := hooks.rollback(
			ctx, restore.ActivationOptions{
				DatabasePath:         cfg.History.DatabasePath,
				CurrentSchemaVersion: history.CurrentSchemaVersion(),
				Validate:             history.ValidateBackupDatabase, HandoffKey: handoffKey,
			},
			"service supervisor restarted before restore health confirmation",
			time.Now(),
		); rollbackErr != nil {
			return fmt.Errorf("recover unconfirmed restore: %w", rollbackErr)
		} else if rolledBack && logger != nil {
			_ = logger.Warning("rolled back an unconfirmed history restore before worker startup")
		}
		restorePending, pendingErr := hooks.hasRestore(
			cfg.History.DatabasePath, handoffKey,
		)
		if pendingErr != nil {
			return fmt.Errorf("inspect staged restore: %w", pendingErr)
		}
		updatePending, pendingErr := hooks.hasUpdate(
			updateOptions.Root, handoffKey,
		)
		if pendingErr != nil {
			return fmt.Errorf("inspect staged update: %w", pendingErr)
		}
		if restorePending && updatePending {
			return errors.New("staged restore and application update conflict; cancel one before service startup")
		}
		_, activated, err = hooks.activate(ctx, restore.ActivationOptions{
			DatabasePath:         cfg.History.DatabasePath,
			CurrentSchemaVersion: history.CurrentSchemaVersion(),
			Validate:             history.ValidateBackupDatabase, HandoffKey: handoffKey,
		})
		if err != nil {
			return fmt.Errorf("activate staged restore: %w", err)
		}
		if activated && logger != nil {
			_ = logger.Info("activated staged history restore; awaiting worker health")
		}
		if !activated && cfg.History.OperationsConsole.Capabilities.UpdateApply {
			_, selectedWorker, didActivate, activateErr := hooks.activateUpdate(ctx, updateOptions)
			if activateErr != nil {
				return fmt.Errorf("activate staged update: %w", activateErr)
			}
			workerPath = selectedWorker
			updateActivated = didActivate
			if updateActivated && logger != nil {
				_ = logger.Info("activated staged application update; awaiting worker health")
			}
		} else if workerPath == "" {
			var resolveErr error
			workerPath, resolveErr = update.ResolveWorker(updateOptions)
			if resolveErr != nil {
				return fmt.Errorf("resolve current update worker: %w", resolveErr)
			}
		}
	}
	spec, err := newServiceWorkerSpecForPath(configPath, interactive, installDeps, workerPath)
	if err != nil {
		return err
	}
	if cfg.History.Enabled {
		verified, _, openErr := hooks.openUpdate(updateOptions, spec.path)
		if openErr != nil {
			return fmt.Errorf("open verified update worker: %w", openErr)
		}
		spec.verifiedExecutable = verified
	}
	runErr := hooks.run(ctx, spec, logger)
	if spec.verifiedExecutable != nil {
		_ = spec.verifiedExecutable.Close()
	}
	if !activated && !updateActivated {
		return runErr
	}
	recoveryWorker := workerPath
	updateRolledBack := false
	if updateActivated {
		_, recoveredWorker, rolledBack, rollbackErr := hooks.rollbackUpdate(
			updateOptions,
			"worker exited before update health confirmation", time.Now(),
		)
		if rollbackErr != nil {
			return errors.Join(runErr, fmt.Errorf("roll back staged update: %w", rollbackErr))
		}
		updateRolledBack = rolledBack
		if rolledBack {
			recoveryWorker = recoveredWorker
			if logger != nil {
				_ = logger.Warning("updated worker did not become healthy; previous application restored")
			}
		}
	}
	restoreRolledBack := false
	_, rolledBack, rollbackErr := hooks.rollback(
		ctx, restore.ActivationOptions{
			DatabasePath:         cfg.History.DatabasePath,
			CurrentSchemaVersion: history.CurrentSchemaVersion(),
			Validate:             history.ValidateBackupDatabase, HandoffKey: handoffKey,
		},
		"worker exited before restore health confirmation", time.Now(),
	)
	if rollbackErr != nil {
		return errors.Join(runErr, fmt.Errorf("roll back staged restore: %w", rollbackErr))
	}
	restoreRolledBack = rolledBack
	if !restoreRolledBack && !updateRolledBack {
		return runErr
	}
	if restoreRolledBack && logger != nil {
		_ = logger.Warning("restored worker did not become healthy; original history database restored")
	}
	if ctx.Err() != nil {
		return nil
	}
	recoverySpec, specErr := newServiceWorkerSpecForPath(
		configPath, interactive, installDeps, recoveryWorker,
	)
	if specErr != nil {
		return errors.Join(runErr, specErr)
	}
	if cfg.History.Enabled {
		verified, _, openErr := hooks.openUpdate(updateOptions, recoverySpec.path)
		if openErr != nil {
			return errors.Join(runErr, fmt.Errorf("open verified recovery worker: %w", openErr))
		}
		recoverySpec.verifiedExecutable = verified
	}
	recoveryErr := hooks.run(ctx, recoverySpec, logger)
	if recoverySpec.verifiedExecutable != nil {
		_ = recoverySpec.verifiedExecutable.Close()
	}
	if recoveryErr != nil {
		return errors.Join(runErr, fmt.Errorf("run worker after restore rollback: %w", recoveryErr))
	}
	return nil
}

func updateActivationOptions(
	cfg *config.Config,
	fallback string,
	handoffKey []byte,
) (update.ActivationOptions, error) {
	var trustedRoot []byte
	var err error
	if cfg.Updates.TrustedRootPath != "" {
		trustedRoot, err = os.ReadFile(cfg.Updates.TrustedRootPath)
		if err != nil {
			return update.ActivationOptions{}, fmt.Errorf("read trusted update root: %w", err)
		}
	} else {
		trustedRoot, err = update.EmbeddedRoot()
		if err != nil {
			return update.ActivationOptions{}, err
		}
	}
	build := buildinfo.Current()
	return update.ActivationOptions{
		Root: cfg.History.DatabasePath + ".updates", FallbackPath: fallback,
		HandoffKey: handoffKey, TrustedRoot: trustedRoot,
		Installed: update.Installed{
			Version: build.Version, Commit: build.Commit,
			OS: goruntime.GOOS, Arch: goruntime.GOARCH,
			APIVersion: "v1", SchemaVersion: history.CurrentSchemaVersion(),
		},
		Policy: update.Policy{
			AllowedTargetKeyIDs: cfg.Updates.AllowedTargetKeys,
			RevokedKeyIDs:       cfg.Updates.RevokedKeys,
			Repository:          cfg.Updates.Repository,
			Workflow:            cfg.Updates.Workflow,
			BuilderID:           cfg.Updates.BuilderID,
		},
		DatabasePath: cfg.History.DatabasePath,
		BackupDatabase: func(ctx context.Context, destination string) (backup.Validation, error) {
			return createUpdateDatabaseBackup(ctx, cfg.History.DatabasePath, destination)
		},
		RestoreDatabase:  restoreUpdateDatabaseBackup,
		ValidateDatabase: history.ValidateBackupDatabase,
	}, nil
}

func createUpdateDatabaseBackup(
	ctx context.Context,
	databasePath string,
	destination string,
) (backup.Validation, error) {
	if err := history.OnlineBackupDatabase(ctx, databasePath, destination); err != nil {
		return backup.Validation{}, err
	}
	return history.ValidateBackupDatabase(ctx, destination, false)
}

func restoreUpdateDatabaseBackup(
	ctx context.Context,
	input *os.File,
	databasePath string,
) error {
	stagedPath := databasePath + ".update-restore.tmp"
	rollbackBase := databasePath + ".update-rollback.tmp"
	for _, path := range []string{
		stagedPath, rollbackBase, rollbackBase + "-wal", rollbackBase + "-shm",
	} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("update database recovery artifact already exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := input.Seek(0, 0); err != nil {
		return err
	}
	output, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	copyErr = errors.Join(copyErr, output.Close())
	if copyErr != nil {
		_ = os.Remove(stagedPath)
		return copyErr
	}
	if _, err := history.ValidateBackupDatabase(ctx, stagedPath, false); err != nil {
		_ = os.Remove(stagedPath)
		return err
	}

	moved := make([]string, 0, 3)
	activated := false
	restoreOriginal := func() error {
		var result error
		if activated {
			_ = os.Remove(databasePath)
		}
		for index := len(moved) - 1; index >= 0; index-- {
			suffix := moved[index]
			if err := os.Rename(rollbackBase+suffix, databasePath+suffix); err != nil {
				result = errors.Join(result, err)
			}
		}
		return result
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(databasePath + suffix); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			_ = os.Remove(stagedPath)
			_ = restoreOriginal()
			return err
		}
		if err := os.Rename(databasePath+suffix, rollbackBase+suffix); err != nil {
			_ = os.Remove(stagedPath)
			_ = restoreOriginal()
			return err
		}
		moved = append(moved, suffix)
	}
	if err := os.Rename(stagedPath, databasePath); err != nil {
		return errors.Join(err, restoreOriginal())
	}
	activated = true
	if _, err := history.ValidateBackupDatabase(ctx, databasePath, false); err != nil {
		return errors.Join(err, restoreOriginal())
	}
	for _, suffix := range moved {
		_ = os.Remove(rollbackBase + suffix)
	}
	return nil
}

func newServiceWorkerSpec(configPath string, interactive, installDeps bool) (serviceProcessSpec, error) {
	return newServiceWorkerSpecForPath(configPath, interactive, installDeps, "")
}

func newServiceWorkerSpecForPath(
	configPath string,
	interactive, installDeps bool,
	executable string,
) (serviceProcessSpec, error) {
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return serviceProcessSpec{}, fmt.Errorf("locate service executable: %w", err)
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return serviceProcessSpec{}, err
	}
	env := withoutServiceWorkerEnvironment(os.Environ())
	env = append(env,
		serviceWorkerMarkerEnv+"="+serviceWorkerMarkerValue,
		serviceWorkerConfigEnv+"="+configPath,
		serviceWorkerInstallDepsEnv+"="+strconv.FormatBool(installDeps),
		serviceWorkerInteractiveEnv+"="+strconv.FormatBool(interactive),
	)
	return serviceProcessSpec{
		path:         executable,
		env:          env,
		secrets:      serviceRedactionSecrets(cfg),
		stopTimeout:  serviceWorkerStopTimeout,
		drainTimeout: serviceOutputDrainTimeout,
	}, nil
}

func superviseProcess(ctx context.Context, spec serviceProcessSpec, logger service.Logger) error {
	if spec.path == "" {
		return errors.New("service worker path is empty")
	}
	stopTimeout := spec.stopTimeout
	if stopTimeout <= 0 {
		stopTimeout = serviceWorkerStopTimeout
	}
	drainTimeout := spec.drainTimeout
	if drainTimeout <= 0 {
		drainTimeout = serviceOutputDrainTimeout
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create service output pipe: %w", err)
	}
	defer reader.Close()

	// #nosec G204 -- the executable is os.Executable and arguments are passed without a shell.
	cmd := exec.Command(spec.path, spec.args...)
	cmd.Env = spec.env
	cmd.Stdout = writer
	cmd.Stderr = writer
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = writer.Close()
		return fmt.Errorf("create service control pipe: %w", err)
	}
	if err := prepareSupervisedProcess(cmd, spec.verifiedExecutable); err != nil {
		_ = stdin.Close()
		_ = writer.Close()
		return fmt.Errorf("prepare service worker: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = writer.Close()
		return fmt.Errorf("start service worker: %w", err)
	}
	group, err := attachSupervisedProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdin.Close()
		_ = writer.Close()
		return fmt.Errorf("contain service worker: %w", err)
	}
	defer group.Close()
	_ = writer.Close()

	tail := &sanitizedLogTail{}
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		copyServiceOutputWithSecrets(reader, logger, tail, spec.secrets...)
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var waitErr error
	intentionalStop := false
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		select {
		case waitErr = <-waitDone:
		default:
			intentionalStop = true
			_ = stdin.Close()
			waitErr = stopSupervisedProcess(cmd, group, waitDone, stopTimeout)
		}
	}
	_ = stdin.Close()

	// Descendants can inherit the worker's output descriptor. Terminate the
	// process tree after the worker exits so output draining cannot hang.
	_ = group.Kill()
	waitForServiceOutput(reader, drainDone, drainTimeout)

	if intentionalStop {
		return nil
	}
	if waitErr == nil {
		return &supervisedProcessError{
			reason: "service_worker_exit exit_code=0 unexpected=true",
			tail:   tail.String(),
		}
	}
	return &supervisedProcessError{
		reason: describeSupervisedProcessExit(cmd.ProcessState, waitErr),
		tail:   tail.String(),
	}
}

func stopSupervisedProcess(cmd *exec.Cmd, group supervisedProcessGroup, waitDone <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waitDone:
		return err
	case <-timer.C:
		if err := group.Kill(); err != nil {
			_ = cmd.Process.Kill()
		}
		waitTimer := time.NewTimer(serviceOutputDrainTimeout)
		defer waitTimer.Stop()
		select {
		case err := <-waitDone:
			return err
		case <-waitTimer.C:
			return errors.New("service worker did not exit after forced termination")
		}
	}
}

func waitForServiceOutput(reader io.Closer, drainDone <-chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-drainDone:
	case <-timer.C:
		_ = reader.Close()
		select {
		case <-drainDone:
		case <-time.After(time.Second):
		}
	}
}

func withoutServiceWorkerEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case serviceWorkerMarkerEnv, serviceWorkerConfigEnv, serviceWorkerInstallDepsEnv, serviceWorkerInteractiveEnv:
			continue
		default:
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func serviceRedactionSecrets(cfg *config.Config) []string {
	secrets := cfg.SecretValues()
	if cfg.Auth.PrivateKeyPath != "" {
		if privateKey, err := os.ReadFile(cfg.Auth.PrivateKeyPath); err == nil {
			secrets = append(secrets, string(privateKey))
			for line := range strings.Lines(string(privateKey)) {
				line = strings.TrimSpace(line)
				if len(line) >= 16 && !strings.Contains(line, "PRIVATE KEY-----") {
					secrets = append(secrets, line)
				}
			}
		}
	}
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if found && len(value) >= 4 && isSensitiveEnvironmentName(name) {
			secrets = append(secrets, value)
		}
	}
	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})
	filtered := secrets[:0]
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if _, exists := seen[secret]; exists {
			continue
		}
		seen[secret] = struct{}{}
		filtered = append(filtered, secret)
	}
	return filtered
}

func isSensitiveEnvironmentName(name string) bool {
	upper := strings.ToUpper(name)
	for _, part := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "JIT_CONFIG", "AUTHORIZATION", "API_KEY", "ACCESS_KEY"} {
		if strings.Contains(upper, part) {
			return true
		}
	}
	return false
}
