package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/backup"
	"github.com/GerardSmit/multirunner/internal/buildinfo"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/configview"
	"github.com/GerardSmit/multirunner/internal/consoleapi"
	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/consoleui"
	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/diagnostics"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/historysync"
	"github.com/GerardSmit/multirunner/internal/historyui"
	"github.com/GerardSmit/multirunner/internal/metrics"
	"github.com/GerardSmit/multirunner/internal/operations"
	"github.com/GerardSmit/multirunner/internal/pool"
	"github.com/GerardSmit/multirunner/internal/restore"
	"github.com/GerardSmit/multirunner/internal/runner"
	"github.com/GerardSmit/multirunner/internal/runtimecontrol"
	"github.com/GerardSmit/multirunner/internal/searchexport"
	"github.com/GerardSmit/multirunner/internal/supportbundle"
	"github.com/GerardSmit/multirunner/internal/transientlog"
	"github.com/GerardSmit/multirunner/internal/update"
	"github.com/GerardSmit/multirunner/internal/webhook"
)

type historyRuntime struct {
	store          *history.Store
	lifecycle      runner.LifecycleObserver
	webhook        webhook.Observer
	syncer         *historysync.Syncer
	journal        *operations.Journal
	epochID        string
	controls       *runtimecontrol.Registry
	controlCancel  context.CancelFunc
	controlDone    chan struct{}
	alertCancel    context.CancelFunc
	alertDone      chan struct{}
	alertEngine    *alerts.Engine
	diagnostics    *diagnostics.Service
	configuration  *configview.Inspector
	supportBundles *supportbundle.Service
	searchExports  *searchexport.Service
	logs           *transientlog.Service
	backups        *backup.Service
	restores       *restore.Service
	restoreKey     []byte
	listener       net.Listener
	handler        http.Handler
	authenticator  *consoleauth.Authenticator
}

func (r *historyRuntime) Close() error {
	if r == nil {
		return nil
	}
	if r.controlCancel != nil {
		r.controlCancel()
		<-r.controlDone
	}
	var closeErrors []error
	if r.journal != nil {
		if err := r.journal.CloseWithError(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	if r.alertCancel != nil {
		r.alertCancel()
		<-r.alertDone
	}
	if r.listener != nil {
		if err := r.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErrors = append(closeErrors, err)
		}
		r.listener = nil
	}
	replayComplete := true
	if r.alertEngine != nil {
		replayContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r.alertEngine.Replay(replayContext); err != nil {
			replayComplete = false
			closeErrors = append(closeErrors, err)
		}
		cancel()
	}
	if replayComplete && r.epochID != "" && r.store != nil {
		if err := r.store.EndHostEpoch(context.Background(), r.epochID, time.Now()); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	if r.store != nil {
		if err := r.store.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

func (r *historyRuntime) StartConsole(
	ctx context.Context, degradedReason string, logger *slog.Logger,
) {
	if r == nil || r.listener == nil || r.handler == nil {
		return
	}
	handler := r.handler
	if degradedReason != "" {
		handler = r.authenticator.Handler(degradedConsoleSecurityHeaders(
			degradedReadOnlyHandler(handler, degradedConsoleState{
				Listen: r.listener.Addr().String(), Reason: degradedReason,
			}),
		))
	}
	listener := r.listener
	r.listener = nil
	go serveHistoryDashboard(ctx, listener, handler, logger.With("component", "console"))
}

type degradedConsoleState struct {
	Listen string
	Reason string
}

func degradedReadOnlyHandler(next http.Handler, state degradedConsoleState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/system" {
			writeDegradedJSON(w, http.StatusOK, map[string]any{
				"status":            "degraded",
				"mode":              "read_only",
				"listen":            state.Listen,
				"app_version":       buildinfo.Current().String(),
				"api_version":       "v1",
				"configuration":     "read_only",
				"mutations_enabled": false,
				"degraded_reason":   state.Reason,
			})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeDegradedJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]any{
					"code":    "degraded_read_only",
					"message": "operations console mutations are disabled",
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func degradedConsoleSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func writeDegradedJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (r *historyRuntime) StartDiagnostics(
	cfg *config.Config, launchers []*pool.Launcher, backends []backend.Backend,
) error {
	if r == nil || r.diagnostics == nil {
		return nil
	}
	checks := []diagnostics.Check{
		{
			ID: "history.database", Version: 1, Category: "storage", Severity: "high",
			DocsRoute: "/docs/operations-console#diagnostics-and-support",
			Run: func(ctx context.Context) (diagnostics.Observation, error) {
				health, err := r.store.DatabaseHealth(ctx)
				if err != nil {
					return diagnostics.Observation{
						Status: diagnostics.StatusFail, Observed: "database check failed",
						Expected:    "SQLite quick_check returns ok",
						Remediation: "Run `multirunner doctor` and inspect the history database path.",
					}, err
				}
				status := diagnostics.StatusPass
				if health.QuickCheck != "ok" {
					status = diagnostics.StatusFail
				}
				return diagnostics.Observation{
					Status: status, Observed: health.QuickCheck, Expected: "ok",
					Evidence: map[string]any{
						"migration_count":  health.MigrationCount,
						"latest_migration": health.LatestMigration,
					},
				}, nil
			},
		},
		{
			ID: "configuration.drift", Version: 1, Category: "configuration", Severity: "medium",
			DocsRoute: "/docs/operations-console#configuration-inspection",
			Run: func(context.Context) (diagnostics.Observation, error) {
				snapshot, err := r.configuration.Snapshot()
				if err != nil {
					return diagnostics.Observation{Status: diagnostics.StatusFail,
						Observed: "configuration could not be inspected",
						Expected: "source configuration is readable"}, err
				}
				status := diagnostics.StatusPass
				observed := "loaded configuration matches the source file"
				if snapshot.Drifted || snapshot.ValidationError != "" {
					status = diagnostics.StatusWarn
					observed = "source configuration changed after service start"
				}
				return diagnostics.Observation{
					Status: status, Observed: observed,
					Expected:    "loaded configuration matches the source file",
					Remediation: snapshot.Guidance,
					Evidence: map[string]any{
						"changed_paths":    snapshot.ChangedPaths,
						"validation_error": snapshot.ValidationError,
					},
				}, nil
			},
		},
	}
	expectedOS := make(map[string]string, len(cfg.Pools))
	for _, configuredPool := range cfg.Pools {
		expectedOS[configuredPool.Name] = configuredPool.OS
	}
	for index, launcher := range launchers {
		if index >= len(backends) {
			break
		}
		name := launcher.Name()
		expected := expectedOS[name]
		currentBackend := backends[index]
		checks = append(checks, diagnostics.Check{
			ID: "pool." + name + ".backend", Version: 1, Category: "pool", Severity: "high",
			Timeout: 10 * time.Second,
			Run: func(ctx context.Context) (diagnostics.Observation, error) {
				if err := currentBackend.Ping(ctx); err != nil {
					return diagnostics.Observation{
						Status: diagnostics.StatusFail, Observed: "backend is unreachable",
						Expected:    "backend responds to health checks",
						Remediation: "Run `multirunner doctor` for daemon-specific guidance.",
					}, err
				}
				actual, err := currentBackend.OSType(ctx)
				if err != nil {
					return diagnostics.Observation{
						Status: diagnostics.StatusFail, Observed: "backend mode is unknown",
						Expected: expected,
					}, err
				}
				status := diagnostics.StatusPass
				if actual != expected {
					status = diagnostics.StatusFail
				}
				return diagnostics.Observation{
					Status: status, Observed: actual, Expected: expected,
					Evidence: map[string]any{"backend": currentBackend.Name(), "pool": name},
				}, nil
			},
		})
	}
	return r.diagnostics.SetChecks(checks)
}

func (r *historyRuntime) StartControls(
	ctx context.Context, launchers []*pool.Launcher, enablePoolControls bool,
	capabilities config.OperationsConsoleCapabilities,
	logger *slog.Logger,
) error {
	if r == nil || r.controls == nil || r.controlCancel != nil {
		return nil
	}
	if enablePoolControls && (capabilities.PoolPauseResume || capabilities.PoolDrain ||
		capabilities.RunnerTerminate || capabilities.RunnerRecycle) {
		controllers := make([]runtimecontrol.PoolController, len(launchers))
		for index, launcher := range launchers {
			controllers[index] = launcher
		}
		r.controls.SetPools(controllers)
	}
	if capabilities.HistorySync && r.syncer != nil {
		r.controls.SetHistorySyncer(r.syncer)
	}
	executor, err := control.NewExecutor(r.store, r.controls, control.ExecutorOptions{
		Owner: "host-" + r.epochID, Lease: 30 * time.Second,
	})
	if err != nil {
		return err
	}
	controlContext, cancel := context.WithCancel(ctx)
	r.controlCancel = cancel
	r.controlDone = make(chan struct{})
	go func() {
		defer close(r.controlDone)
		if err := executor.Run(controlContext); err != nil && controlContext.Err() == nil {
			logger.Error("command executor stopped", "err", err)
		}
	}()
	return nil
}

func setupHistory(ctx context.Context, configPath string, cfg *config.Config, provider github.ClientProvider, observability *metrics.Metrics, logger *slog.Logger) (*historyRuntime, error) {
	if !cfg.History.Enabled {
		return nil, nil
	}
	runtime, err := setupPersistentHistory(
		ctx, configPath, cfg, provider, observability, logger,
	)
	if err == nil {
		observability.SetOperationalJournalAvailable(true)
		return runtime, nil
	}
	observability.SetOperationalJournalAvailable(false)
	logger.Error(
		"operations console persistence unavailable; runner runtime will continue",
		"mode", "read_only", "err", err,
	)
	if fallbackErr := startDegradedConsole(ctx, configPath, cfg, logger); fallbackErr != nil {
		logger.Error(
			"operations console degraded endpoint is unavailable; runner runtime will continue",
			"err", fallbackErr,
		)
	}
	return nil, nil
}

func startDegradedConsole(
	ctx context.Context, configPath string, cfg *config.Config, logger *slog.Logger,
) error {
	ui, err := consoleui.New()
	if err != nil {
		return err
	}
	secretPath := consoleauth.SecretPath(cfg.History.DatabasePath)
	secret, err := consoleauth.EnsureSecret(secretPath)
	if err != nil {
		return err
	}
	authenticator, err := consoleauth.NewPersistent(secret, consoleauth.PairingStatePath(secretPath))
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.History.Listen)
	if err != nil {
		return err
	}
	configuration, configurationErr := configview.New(configPath, cfg)
	if configurationErr != nil {
		logger.Warn("degraded console configuration inspection unavailable", "err", configurationErr)
	}
	mux := http.NewServeMux()
	if configuration != nil {
		mux.HandleFunc("GET /api/v1/configuration", func(w http.ResponseWriter, _ *http.Request) {
			snapshot, snapshotErr := configuration.Snapshot()
			if snapshotErr != nil {
				writeDegradedJSON(w, http.StatusServiceUnavailable, map[string]any{
					"error": map[string]any{
						"code":    "configuration_unavailable",
						"message": "configuration inspection is unavailable",
					},
				})
				return
			}
			writeDegradedJSON(w, http.StatusOK, snapshot)
		})
	}
	unavailable := func(w http.ResponseWriter, _ *http.Request) {
		writeDegradedJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"code":    "degraded_read_only",
				"message": "operations console persistence is unavailable",
			},
		})
	}
	mux.HandleFunc("/api/v1/", unavailable)
	mux.HandleFunc("/api/", unavailable)
	mux.Handle("/", ui)
	handler := authenticator.Handler(degradedConsoleSecurityHeaders(
		degradedReadOnlyHandler(mux, degradedConsoleState{
			Listen: listener.Addr().String(), Reason: "console_persistence_unavailable",
		}),
	))
	go serveHistoryDashboard(ctx, listener, handler, logger.With("component", "console"))
	return nil
}

func setupPersistentHistory(ctx context.Context, configPath string, cfg *config.Config, provider github.ClientProvider, observability *metrics.Metrics, logger *slog.Logger) (*historyRuntime, error) {
	store, err := history.Open(ctx, cfg.History.DatabasePath)
	if err != nil {
		return nil, err
	}
	legacyAPI, err := historyui.New(store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	ui, err := consoleui.New()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	secretPath := consoleauth.SecretPath(cfg.History.DatabasePath)
	secret, err := consoleauth.EnsureSecret(secretPath)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	authenticator, err := consoleauth.NewPersistent(secret, consoleauth.PairingStatePath(secretPath))
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	configuration, err := configview.New(configPath, cfg)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	diagnosticService := diagnostics.NewService()
	installationHash := sha256.Sum256(secret)
	hostName, _ := os.Hostname()
	host, err := store.EnsureOperationalHost(ctx, hex.EncodeToString(installationHash[:]), hostName)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	epoch, err := store.StartHostEpoch(ctx, host.ID, time.Now())
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	store.SetCommandEpoch(epoch.ID)
	journal, err := operations.NewJournal(ctx, store, epoch.ID, operations.JournalOptions{
		OnAppendError: func(err error) {
			observability.ObserveOperationalJournalAppendError()
			logger.Error("append operational event", "err", err)
		},
		OnQueueOverflow: func() {
			logger.Error("operational event queue overflow")
		},
		OnRejected: func(reason operations.RejectionReason) {
			observability.ObserveOperationalJournalRejected(string(reason))
		},
		OnSlowSubscriber: func() {
			logger.Warn("disconnected slow console event subscriber")
		},
	})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	controls := runtimecontrol.NewRegistry()
	configureConsoleCapabilityGates(controls, cfg.History.OperationsConsole.Capabilities)
	bundleService, err := supportbundle.New(supportbundle.Options{
		Root:   cfg.History.DatabasePath + ".support-bundles",
		HostID: host.ID, HostEpoch: epoch.ID,
		AppVersion: buildinfo.Current().String(),
		Store:      store, Events: store,
		Diagnostics:   diagnosticService.Run,
		Configuration: configuration.Snapshot,
		DatabaseHealth: func(ctx context.Context) (any, error) {
			return store.DatabaseHealth(ctx)
		},
	})
	if err != nil {
		journal.Close()
		_ = store.Close()
		return nil, err
	}
	if cfg.History.OperationsConsole.Capabilities.SupportBundles {
		controls.SetSupportBundleGenerator(bundleService)
	}
	backupService, err := backup.New(backup.Options{
		Root: cfg.History.DatabasePath + ".backups", HostID: host.ID,
		AppVersion: buildinfo.Current().String(), Source: store, Store: store,
		Validate: history.ValidateBackupDatabase,
	})
	if err != nil {
		journal.Close()
		_ = store.Close()
		return nil, err
	}
	if cfg.History.OperationsConsole.Capabilities.BackupCreate {
		controls.SetBackupGenerator(backupService)
	}
	restoreService, err := restore.New(restore.Options{
		Root: cfg.History.DatabasePath + ".restore", DatabasePath: cfg.History.DatabasePath,
		HostID: host.ID, CurrentSchemaVersion: history.CurrentSchemaVersion(),
		Backups: backupService, Store: store,
		Validate: history.ValidateBackupDatabase, HandoffKey: secret,
	})
	if err != nil {
		journal.Close()
		_ = store.Close()
		return nil, err
	}
	if cfg.History.OperationsConsole.Capabilities.RestoreStage {
		controls.SetRestoreStager(restoreService)
	}
	var updateInspector *update.Inspector
	if cfg.Updates.MetadataURL != "" {
		source, err := update.NewHTTPSource(cfg.Updates.MetadataURL, nil)
		if err != nil {
			journal.Close()
			_ = store.Close()
			return nil, err
		}
		build := buildinfo.Current()
		installed := update.Installed{
			Version: build.Version, Commit: build.Commit,
			OS: goruntime.GOOS, Arch: goruntime.GOARCH,
			APIVersion: "v1", SchemaVersion: history.CurrentSchemaVersion(),
		}
		policy := update.Policy{
			AllowedTargetKeyIDs: cfg.Updates.AllowedTargetKeys,
			RevokedKeyIDs:       cfg.Updates.RevokedKeys,
			Repository:          cfg.Updates.Repository,
			Workflow:            cfg.Updates.Workflow,
			BuilderID:           cfg.Updates.BuilderID,
		}
		var trustedRoot []byte
		if cfg.Updates.TrustedRootPath != "" {
			trustedRoot, err = os.ReadFile(cfg.Updates.TrustedRootPath)
			if err != nil {
				journal.Close()
				_ = store.Close()
				return nil, fmt.Errorf("read trusted update root: %w", err)
			}
		} else {
			trustedRoot, err = update.EmbeddedRoot()
			if err != nil {
				journal.Close()
				_ = store.Close()
				return nil, err
			}
		}
		updateInspector, err = update.NewInspector(source, installed, trustedRoot, policy, nil)
		if err != nil {
			journal.Close()
			_ = store.Close()
			return nil, err
		}
		if len(trustedRoot) > 0 {
			updateService, err := update.New(update.Options{
				Root:        cfg.History.DatabasePath + ".updates",
				TrustedRoot: trustedRoot,
				Installed:   installed,
				HostID:      host.ID,
				Policy:      policy,
				Source:      source, Store: store, HandoffKey: secret,
			})
			if err != nil {
				journal.Close()
				_ = store.Close()
				return nil, err
			}
			if _, err := updateService.Prune(ctx); err != nil {
				journal.Close()
				_ = store.Close()
				return nil, fmt.Errorf("prune update artifacts: %w", err)
			}
			if cfg.History.OperationsConsole.Capabilities.UpdateStage {
				controls.SetUpdateStager(updateService)
			}
		}

	}
	exportService, err := searchexport.New(searchexport.Options{
		Root: cfg.History.DatabasePath + ".exports", Source: store, Store: store,
	})
	if err != nil {
		journal.Close()
		_ = store.Close()
		return nil, err
	}
	logService, err := transientlog.New(transientlog.Options{
		Jobs: store, Auditor: store,
		ClientFor: func(repository string) transientlog.LogFetcher {
			client := provider.ClientFor(repository)
			if client == nil {
				return nil
			}
			return client
		},
		Secrets: cfg.SecretValues(),
	})
	if err != nil {
		journal.Close()
		_ = store.Close()
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.History.Listen)
	if err != nil {
		journal.Close()
		_ = store.Close()
		return nil, err
	}
	historyObserver := history.NewLifecycleObserver(store, logger.With("component", "history"))
	operationsObserver := operations.NewRunnerLifecycleObserver(journal)
	runtime := &historyRuntime{
		store: store,
		lifecycle: runner.LifecycleObserverFunc(func(
			ctx context.Context, event runner.LifecycleEvent,
		) {
			historyObserver.ObserveRunnerLifecycle(ctx, event)
			operationsObserver.ObserveRunnerLifecycle(ctx, event)
		}),
		webhook: history.NewWebhookObserver(
			store, cfg.History.LegacyPrefixes, logger.With("component", "history"),
		),
		journal:        journal,
		epochID:        epoch.ID,
		controls:       controls,
		diagnostics:    diagnosticService,
		configuration:  configuration,
		supportBundles: bundleService,
		searchExports:  exportService,
		logs:           logService,
		backups:        backupService,
		restores:       restoreService,
		restoreKey:     append([]byte(nil), secret...),
	}
	endpoints, adapters, err := notificationConfiguration(cfg.History.Notifications)
	if err != nil {
		_ = listener.Close()
		_ = runtime.Close()
		return nil, err
	}
	if err := store.SyncNotificationEndpoints(ctx, endpoints, time.Now()); err != nil {
		_ = listener.Close()
		_ = runtime.Close()
		return nil, err
	}
	unfinishedEpochs, err := store.UnfinishedHostEpochs(ctx, host.ID, epoch.ID)
	if err != nil {
		_ = listener.Close()
		_ = runtime.Close()
		return nil, err
	}
	for _, unfinishedEpoch := range unfinishedEpochs {
		recoveryEngine, err := alerts.NewEngine(alerts.Options{
			Store: store, Source: journal, EpochID: unfinishedEpoch.ID,
			Logger: logger.With("component", "alert-recovery"),
		})
		if err != nil {
			_ = listener.Close()
			_ = runtime.Close()
			return nil, err
		}
		if err := recoveryEngine.Replay(ctx); err != nil {
			_ = listener.Close()
			_ = runtime.Close()
			return nil, err
		}
		if err := store.EndHostEpoch(ctx, unfinishedEpoch.ID, time.Now()); err != nil {
			_ = listener.Close()
			_ = runtime.Close()
			return nil, err
		}
		logger.Info("replayed unfinished alert epoch",
			"host_epoch", unfinishedEpoch.ID,
			"last_sequence", unfinishedEpoch.LastSequence)
	}
	alertEngine, err := alerts.NewEngine(alerts.Options{
		Store: store, Source: journal, EpochID: epoch.ID,
		Logger: logger.With("component", "alerts"),
	})
	if err != nil {
		_ = listener.Close()
		_ = runtime.Close()
		return nil, err
	}
	runtime.alertEngine = alertEngine
	deliveryWorker, err := alerts.NewDeliveryWorker(alerts.DeliveryWorkerOptions{
		Store: store, Adapters: adapters, Owner: "host-" + epoch.ID,
		Logger: logger.With("component", "notifications"),
	})
	if err != nil {
		_ = listener.Close()
		_ = runtime.Close()
		return nil, err
	}
	alertContext, alertCancel := context.WithCancel(ctx)
	runtime.alertCancel = alertCancel
	runtime.alertDone = make(chan struct{})
	go func() {
		defer close(runtime.alertDone)
		var workers sync.WaitGroup
		workers.Add(5)
		go func() {
			defer workers.Done()
			if err := alertEngine.Run(alertContext); err != nil && alertContext.Err() == nil {
				logger.Error("alert engine stopped", "err", err)
			}
		}()
		go func() {
			defer workers.Done()
			if err := deliveryWorker.Run(alertContext); err != nil && alertContext.Err() == nil {
				logger.Error("notification delivery worker stopped", "err", err)
			}
		}()
		go func() {
			defer workers.Done()
			runAlertRetentionLoop(
				alertContext, store, logger.With("component", "alert-retention"),
			)
		}()
		go func() {
			defer workers.Done()
			runBackupRetentionLoop(
				alertContext, backupService, restoreService,
				logger.With("component", "recovery-retention"),
			)
		}()
		go func() {
			defer workers.Done()
			runTieredRetentionLoop(
				alertContext, store, logger.With("component", "tiered-retention"),
			)
		}()
		workers.Wait()
	}()
	observability.EnableHistory(store)

	repositories := historyRepositories(cfg, provider)
	if len(repositories) > 0 {
		runtime.syncer, err = historysync.New(store, repositories, historysync.Options{
			Backfill:       cfg.History.Backfill == "all_available",
			LegacyPrefixes: cfg.History.LegacyPrefixes,
			Observer:       observability,
		})
		if err != nil {
			_ = listener.Close()
			_ = runtime.Close()
			return nil, err
		}

		go runHistorySyncLoop(ctx, runtime.syncer, time.Duration(cfg.History.SyncIntervalSec)*time.Second,
			cfg.History.RetentionDays, store, logger.With("component", "history-sync"))
	} else {
		logger.Warn("history REST reconciliation is unavailable for this scope; workflow_job webhooks will provide new records",
			"scope", cfg.GitHub.Scope)
	}
	handler := consoleapi.New(consoleapi.Options{
		Auth:            authenticator,
		LegacyAPI:       legacyAPI,
		UI:              ui,
		Listen:          cfg.History.Listen,
		Database:        cfg.History.DatabasePath,
		StartedAt:       time.Now(),
		AppVersion:      buildinfo.Current().String(),
		HostID:          host.ID,
		HostEpoch:       epoch.ID,
		Events:          store,
		LiveEvents:      journal,
		Commands:        store,
		Controls:        controls,
		Diagnostics:     diagnosticService,
		Configuration:   configuration,
		SupportBundles:  bundleService,
		Search:          store,
		Runs:            store,
		Logs:            logService,
		Analytics:       store,
		SavedViews:      store,
		Exports:         exportService,
		Alerts:          store,
		Backups:         backupService,
		Restores:        restoreService,
		Updates:         store,
		UpdateInspector: updateInspector,
		Resources:       store,
		StreamMetrics:   observability,
	})
	runtime.listener = listener
	runtime.handler = handler
	runtime.authenticator = authenticator
	return runtime, nil
}

func configureConsoleCapabilityGates(
	registry *runtimecontrol.Registry,
	capabilities config.OperationsConsoleCapabilities,
) {
	const reason = "disabled by history.operations_console.capabilities configuration"
	gates := map[string]bool{
		runtimecontrol.CommandPoolPause:       capabilities.PoolPauseResume,
		runtimecontrol.CommandPoolResume:      capabilities.PoolPauseResume,
		runtimecontrol.CommandPoolCancelDrain: capabilities.PoolDrain,
		runtimecontrol.CommandPoolDrain:       capabilities.PoolDrain,
		runtimecontrol.CommandRunnerTerminate: capabilities.RunnerTerminate,
		runtimecontrol.CommandRunnerRecycle:   capabilities.RunnerRecycle,
		runtimecontrol.CommandHistorySync:     capabilities.HistorySync,
		runtimecontrol.CommandSupportBundle:   capabilities.SupportBundles,
		runtimecontrol.CommandBackupCreate:    capabilities.BackupCreate,
		runtimecontrol.CommandRestoreStage:    capabilities.RestoreStage,
		runtimecontrol.CommandUpdateStage:     capabilities.UpdateStage,
	}
	for commandType, enabled := range gates {
		if !enabled {
			registry.Disable(commandType, reason)
		}
	}
}

func runTieredRetentionLoop(
	ctx context.Context, store *history.Store, logger *slog.Logger,
) {
	prune := func() {
		result, err := store.PruneTiered(
			ctx, time.Now().UTC(), history.DefaultTieredRetentionPolicy(),
		)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("tiered retention failed", "err", err)
			}
			return
		}
		logger.Debug("tiered retention complete", "result", result)
	}
	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

func runAlertRetentionLoop(
	ctx context.Context, store *history.Store, logger *slog.Logger,
) {
	prune := func() {
		result, err := store.PruneAlertHistory(
			ctx, time.Now().UTC().Add(-180*24*time.Hour),
		)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("alert retention failed", "err", err)
			}
			return
		}
		if result.Alerts > 0 || result.Deliveries > 0 {
			logger.Info("alert retention complete",
				"alerts", result.Alerts, "deliveries", result.Deliveries)
		}
	}
	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

func runBackupRetentionLoop(
	ctx context.Context, backups *backup.Service, restores *restore.Service,
	logger *slog.Logger,
) {
	prune := func() {
		count, err := backups.Prune(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("backup retention failed", "err", err)
			}
			return
		}
		if count > 0 {
			logger.Info("backup retention complete", "backups", count)
		}
		restoreCount, err := restores.Prune(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("restore retention failed", "err", err)
			}
			return
		}
		if restoreCount > 0 {
			logger.Info("restore retention complete", "handoffs", restoreCount)
		}
	}
	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

func notificationConfiguration(
	config config.Notifications,
) ([]alerts.Endpoint, alerts.AdapterMap, error) {
	var endpoints []alerts.Endpoint
	adapters := make(alerts.AdapterMap)
	if config.AiTextURL != "" {
		const configRef = "history.notifications.aitext"
		endpoint := alerts.Endpoint{
			ID: alerts.EndpointID(configRef), Name: "AiText",
			Kind: "aitext", ConfigRef: configRef, Enabled: true,
		}
		adapter, err := alerts.NewAiTextAdapter(config.AiTextURL, nil)
		if err != nil {
			return nil, nil, err
		}
		endpoints = append(endpoints, endpoint)
		adapters[endpoint.ID] = adapter
	}
	for _, configured := range config.Webhooks {
		configRef := "history.notifications.webhooks." + configured.Name
		endpoint := alerts.Endpoint{
			ID: alerts.EndpointID(configRef), Name: configured.Name,
			Kind: "webhook", ConfigRef: configRef, Enabled: true,
		}
		adapter, err := alerts.NewWebhookAdapter(configured.URL, configured.Secret, nil)
		if err != nil {
			return nil, nil, err
		}
		endpoints = append(endpoints, endpoint)
		adapters[endpoint.ID] = adapter
	}
	return endpoints, adapters, nil
}

func checkHistoryDatabase(ctx context.Context, cfg *config.Config) error {
	if !cfg.History.Enabled {
		return nil
	}
	store, err := history.Open(ctx, cfg.History.DatabasePath)
	if err != nil {
		return err
	}
	return store.Close()
}

func historyRepositories(cfg *config.Config, provider github.ClientProvider) []historysync.Repository {
	var names []string
	switch cfg.GitHub.Scope {
	case config.ScopeRepo:
		names = []string{cfg.GitHub.Owner + "/" + cfg.GitHub.Repo}
	case config.ScopeRepos:
		for _, ref := range cfg.GitHub.ResolvedRepos() {
			names = append(names, ref.Owner+"/"+ref.Repo)
		}
	}
	repositories := make([]historysync.Repository, 0, len(names))
	for _, name := range names {
		if client := provider.ClientFor(name); client != nil {
			repositories = append(repositories, historysync.Repository{Name: name, API: client})
		}
	}
	return repositories
}

func runHistorySyncLoop(ctx context.Context, syncer *historysync.Syncer, interval time.Duration, retentionDays int, store *history.Store, logger *slog.Logger) {
	run := func() {
		report, err := syncer.Sync(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("history synchronization failed", "err", err)
			}
			return
		}
		for _, repository := range report.Repositories {
			if repository.Error != nil {
				logger.Error("history repository synchronization degraded",
					"repository", repository.Repository, "err", repository.Error)
				continue
			}
			logger.Info("history repository synchronized",
				"repository", repository.Repository, "runs", repository.Runs,
				"jobs", repository.Jobs, "exact", repository.ExactJobs,
				"inferred", repository.InferredJobs)
		}
		if retentionDays > 0 {
			if _, err := store.PruneBefore(ctx, time.Now().UTC().AddDate(0, 0, -retentionDays)); err != nil && ctx.Err() == nil {
				logger.Error("history retention failed", "err", err)
			}
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func serveHistoryDashboard(ctx context.Context, listener net.Listener, handler http.Handler, logger *slog.Logger) {
	server := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			logger.Error("history dashboard shutdown failed", "err", err)
		}
	}()
	logger.Info("history dashboard listening", "addr", listener.Addr().String())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("history dashboard stopped", "err", err)
	}
}
