// Package consoleapi composes the versioned local console API, compatibility
// history API, authentication, and embedded frontend.
package consoleapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
	"github.com/GerardSmit/multirunner/internal/backup"
	"github.com/GerardSmit/multirunner/internal/configview"
	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/diagnostics"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/operations"
	"github.com/GerardSmit/multirunner/internal/restore"
	"github.com/GerardSmit/multirunner/internal/searchexport"
	"github.com/GerardSmit/multirunner/internal/supportbundle"
	"github.com/GerardSmit/multirunner/internal/transientlog"
	"github.com/GerardSmit/multirunner/internal/update"
)

type EventReader interface {
	ListOperationalEvents(context.Context, operations.EventQuery) ([]operations.Event, error)
	OperationalEventBounds(context.Context, string) (minimum, maximum int64, err error)
	OperationalSnapshot(context.Context, string) (operations.Snapshot, error)
	OperationalSnapshotAt(context.Context, string, int64) (operations.Snapshot, error)
}

type LiveEventSource interface {
	Subscribe(buffer int) (*operations.Subscription, error)
}

type CommandStore interface {
	CreateCommand(context.Context, control.Request) (control.Command, bool, error)
	Command(context.Context, string) (control.Command, error)
	TransitionCommand(context.Context, string, control.Transition) (control.Command, error)
}

type CommandAdapters interface {
	AdapterFor(control.Command) (control.Adapter, bool)
	Validate(control.Command) error
}

type DiagnosticRunner interface {
	Run(context.Context) diagnostics.Report
}

type ConfigurationReader interface {
	Snapshot() (configview.Snapshot, error)
}

type SupportBundleReader interface {
	Open(context.Context, string) (supportbundle.Metadata, *os.File, error)
}

type SearchReader interface {
	Search(context.Context, history.SearchOptions) ([]history.SearchResult, error)
}

type RunInspectionReader interface {
	WorkflowRun(context.Context, string, int64) (history.WorkflowRun, error)
	ListWorkflowJobs(context.Context, history.ListOptions) ([]history.WorkflowJob, error)
	ListRunnerSessions(context.Context, history.ListOptions) ([]history.RunnerSession, error)
}

type JobLogReader interface {
	Fetch(context.Context, string, string, int64) (transientlog.Result, error)
}

type AnalyticsReader interface {
	Analytics(context.Context, history.AnalyticsOptions) (history.AnalyticsReport, error)
}

type SavedViewStore interface {
	ListSavedViews(context.Context) ([]history.SavedView, error)
	CreateSavedView(context.Context, string, string, history.SavedViewInput) (history.SavedView, bool, error)
	DeleteSavedView(context.Context, string, string, int64) (bool, error)
}

type SearchExporter interface {
	Generate(context.Context, string, searchexport.Request, searchexport.Audit) (searchexport.Metadata, bool, error)
	Open(context.Context, string, searchexport.Audit) (searchexport.Metadata, *os.File, error)
}

type AlertStore interface {
	ListAlerts(context.Context, alerts.ListOptions) ([]alerts.Instance, error)
	Alert(context.Context, string) (alerts.Instance, error)
	AcknowledgeAlert(context.Context, alerts.AcknowledgeRequest) (alerts.Instance, error)
	SilenceAlert(context.Context, alerts.SilenceRequest) (alerts.Instance, alerts.Silence, error)
	AddAlertAnnotation(context.Context, alerts.AnnotationRequest) (alerts.Annotation, error)
	TransitionAlert(context.Context, alerts.TransitionRequest) (alerts.Instance, error)
}

type BackupReader interface {
	List(context.Context, int) ([]backup.Metadata, error)
	Open(context.Context, string) (backup.Metadata, *os.File, error)
}

type RestoreReader interface {
	List(context.Context, int) ([]restore.Metadata, error)
}

type UpdateReader interface {
	ListUpdates(context.Context, int) ([]update.Metadata, error)
}

type UpdateInspector interface {
	Inspect(context.Context) (update.Inspection, error)
}

// Options configures the local console HTTP handler.
type Options struct {
	Auth                  *consoleauth.Authenticator
	LegacyAPI             http.Handler
	UI                    http.Handler
	Listen                string
	Database              string
	StartedAt             time.Time
	AppVersion            string
	HostID                string
	HostEpoch             string
	Events                EventReader
	LiveEvents            LiveEventSource
	Heartbeat             time.Duration
	Commands              CommandStore
	Controls              CommandAdapters
	Diagnostics           DiagnosticRunner
	Configuration         ConfigurationReader
	SupportBundles        SupportBundleReader
	Search                SearchReader
	Runs                  RunInspectionReader
	Logs                  JobLogReader
	Analytics             AnalyticsReader
	SavedViews            SavedViewStore
	Exports               SearchExporter
	Alerts                AlertStore
	Backups               BackupReader
	Restores              RestoreReader
	Updates               UpdateReader
	UpdateInspector       UpdateInspector
	Resources             ResourceReader
	StreamHostLimit       int
	StreamSessionLimit    int
	StreamMaximumLifetime time.Duration
	StreamMetrics         StreamMetrics
}

// New creates the authenticated console handler.
func New(options Options) http.Handler {
	mux := http.NewServeMux()
	streams := newStreamAdmission(
		options.StreamHostLimit, options.StreamSessionLimit, options.StreamMetrics,
	)
	mux.HandleFunc("GET /api/v1/system", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "operational",
			"mode":          "local",
			"listen":        options.Listen,
			"database":      options.Database,
			"started_at":    options.StartedAt.UTC(),
			"app_version":   options.AppVersion,
			"api_version":   "v1",
			"configuration": "read_only",
		})
	})
	mux.HandleFunc("GET /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		session, ok := consoleauth.SessionFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication_required", "authentication is required")
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
	mux.HandleFunc("GET /api/v1/hosts", hostListHandler(options))
	if options.Events != nil && options.HostEpoch != "" {
		mux.HandleFunc("GET /api/v1/events", eventStreamHandler(options, streams))
		mux.HandleFunc("GET /api/v1/events/query", eventQueryHandler(options))
		mux.HandleFunc("GET /api/v1/events/snapshot", func(w http.ResponseWriter, r *http.Request) {
			snapshot, err := options.Events.OperationalSnapshot(r.Context(), options.HostEpoch)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "events_unavailable", "operational snapshot is unavailable")
				return
			}
			writeJSON(w, http.StatusOK, snapshot)
		})
		mux.HandleFunc("GET /api/v1/runners", runnerQueryHandler(options))
	}
	if options.Commands != nil && options.Controls != nil && options.HostID != "" {
		mux.HandleFunc("POST /api/v1/commands/preview", commandPreviewHandler(options))
		mux.HandleFunc("POST /api/v1/commands", commandCreateHandler(options))
		mux.HandleFunc("GET /api/v1/commands/{id}", commandReadHandler(options))
	}
	if options.Diagnostics != nil {
		mux.HandleFunc("GET /api/v1/diagnostics", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, options.Diagnostics.Run(r.Context()))
		})
	}
	if options.Configuration != nil {
		mux.HandleFunc("GET /api/v1/configuration", func(w http.ResponseWriter, _ *http.Request) {
			snapshot, err := options.Configuration.Snapshot()
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "configuration_unavailable", "configuration inspection is unavailable")
				return
			}
			writeJSON(w, http.StatusOK, snapshot)
		})
	}
	if options.SupportBundles != nil {
		mux.HandleFunc("GET /api/v1/support-bundles/{id}", func(w http.ResponseWriter, r *http.Request) {
			metadata, file, err := options.SupportBundles.Open(r.Context(), r.PathValue("id"))
			if errors.Is(err, supportbundle.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "support bundle was not found")
				return
			}
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "support_bundle_unavailable", "support bundle is unavailable")
				return
			}
			defer file.Close()
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, metadata.FileName))
			w.Header().Set("X-Content-SHA256", metadata.SHA256)
			http.ServeContent(w, r, metadata.FileName, metadata.CreatedAt, file)
		})
	}
	if options.Search != nil {
		mux.HandleFunc("GET /api/v1/search", searchHandler(options.Search))
	}
	if options.Runs != nil {
		mux.HandleFunc("GET /api/v1/runs/{id}", runInspectionHandler(options.Runs))
	}
	if options.Logs != nil {
		mux.HandleFunc("GET /api/v1/jobs/{id}/log", jobLogHandler(options.Logs))
	}
	if options.Analytics != nil {
		mux.HandleFunc("GET /api/v1/analytics", analyticsHandler(options.Analytics))
	}
	if options.SavedViews != nil {
		mux.HandleFunc("/api/v1/saved-views", savedViewsHandler(options))
		mux.HandleFunc("DELETE /api/v1/saved-views/{id}", deleteSavedViewHandler(options))
	}
	if options.Exports != nil {
		mux.HandleFunc("POST /api/v1/exports", createSearchExportHandler(options))
		mux.HandleFunc("GET /api/v1/exports/{id}", downloadSearchExportHandler(options))
	}
	if options.Alerts != nil {
		mux.HandleFunc("GET /api/v1/alerts", alertListHandler(options))
		mux.HandleFunc("GET /api/v1/alerts/{id}", alertReadHandler(options))
		mux.HandleFunc("POST /api/v1/alerts/{id}/acknowledge", alertAcknowledgeHandler(options))
		mux.HandleFunc("POST /api/v1/alerts/{id}/silence", alertSilenceHandler(options))
		mux.HandleFunc("POST /api/v1/alerts/{id}/annotations", alertAnnotationHandler(options))
		mux.HandleFunc("POST /api/v1/alerts/{id}/resolve", alertResolveHandler(options))
		mux.HandleFunc("GET /api/v1/incidents", alertListHandler(options))
		mux.HandleFunc("GET /api/v1/incidents/{id}", alertReadHandler(options))
	}
	if options.Backups != nil {
		mux.HandleFunc("GET /api/v1/backups", backupListHandler(options.Backups))
		mux.HandleFunc("GET /api/v1/backups/{id}/download", backupDownloadHandler(options.Backups))
	}
	if options.Restores != nil {
		mux.HandleFunc("GET /api/v1/restores", restoreListHandler(options.Restores))
	}
	if options.Updates != nil {
		mux.HandleFunc("GET /api/v1/updates", updateListHandler(options.Updates))
	}
	mux.HandleFunc("GET /api/v1/updates/check", updateInspectionHandler(options.UpdateInspector))
	mux.HandleFunc("GET /api/v1/pools", resourceListHandler("pools", options.Resources))
	mux.HandleFunc("GET /api/v1/sessions", resourceListHandler("sessions", options.Resources))
	mux.HandleFunc("GET /api/v1/repositories", resourceListHandler("repositories", options.Resources))
	mux.HandleFunc("GET /api/v1/workflows", resourceListHandler("workflows", options.Resources))
	mux.HandleFunc("GET /api/v1/annotations", annotationListHandler(options.Resources))
	mux.HandleFunc("GET /api/v1/tags", resourceListHandler("tags", options.Resources))
	mux.HandleFunc("GET /api/v1/audit", auditListHandler(options.Resources))
	mux.HandleFunc("/api/v1/", versionedFallback)
	mux.Handle("/api/", options.LegacyAPI)
	mux.Handle("/", options.UI)
	return correlationHeaders(options.Auth.Handler(securityHeaders(mux)))
}

func eventStreamHandler(options Options, admission *streamAdmission) http.HandlerFunc {
	heartbeat := options.Heartbeat
	if heartbeat <= 0 {
		heartbeat = 15 * time.Second
	}
	maximumLifetime := options.StreamMaximumLifetime
	if maximumLifetime <= 0 {
		maximumLifetime = DefaultStreamMaximumLifetime
	}
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming is unavailable")
			return
		}
		session, ok := consoleauth.SessionFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication_required", "authentication is required")
			return
		}
		release, reason, admitted := admission.acquire(session.ActorID)
		if !admitted {
			w.Header().Set("Retry-After", "1")
			writeErrorDetails(w, http.StatusTooManyRequests, "rate_limited",
				"event stream admission limit was reached", nil,
				map[string]any{"after_seconds": 1, "reason": reason})
			return
		}
		defer release()
		revoked, valid := options.Auth.SessionRevocation(session)
		if !valid {
			writeError(w, http.StatusUnauthorized, "authentication_required", "authentication is required")
			return
		}
		deadline := time.Now().Add(maximumLifetime)
		if session.ExpiresAt.Before(deadline) {
			deadline = session.ExpiresAt
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		go func() {
			select {
			case <-revoked:
				cancel()
			case <-ctx.Done():
			}
		}()
		r = r.WithContext(ctx)
		sessionValid := func() bool {
			return options.Auth.ValidateSession(session)
		}
		after, err := parseEventCursor(r.Header.Get("Last-Event-ID"), options.HostEpoch)
		if err != nil {
			writeError(w, http.StatusConflict, "event_cursor_invalid", err.Error())
			return
		}
		minimum, maximum, err := options.Events.OperationalEventBounds(r.Context(), options.HostEpoch)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "events_unavailable", "operational events are unavailable")
			return
		}
		if after > maximum {
			writeError(w, http.StatusConflict, "event_cursor_invalid", "event cursor is newer than the current host sequence")
			return
		}
		if after > 0 && minimum > 0 && after < minimum-1 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"code":           "event_cursor_expired",
				"message":        "event cursor is older than retained history",
				"host_epoch":     options.HostEpoch,
				"snapshot_route": "/api/v1/events/snapshot",
			})
			return
		}

		var subscription *operations.Subscription
		if options.LiveEvents != nil {
			subscription, err = options.LiveEvents.Subscribe(256)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "events_unavailable", "live events are unavailable")
				return
			}
			defer subscription.Close()
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		sent := after
		replayed := 0
		for sent < maximum {
			events, err := options.Events.ListOperationalEvents(r.Context(), operations.EventQuery{
				HostEpoch: options.HostEpoch, AfterSequence: sent, Limit: 1000,
			})
			if err != nil {
				return
			}
			if len(events) == 0 {
				break
			}
			for _, event := range events {
				if !sessionValid() {
					return
				}
				if replayed >= 10000 {
					_, _ = fmt.Fprintf(w, "event: stream-reset\ndata: {\"code\":\"event_replay_limit\",\"snapshot_route\":\"/api/v1/events/snapshot\"}\n\n")
					flusher.Flush()
					return
				}
				if err := writeSSEEvent(w, event); err != nil {
					return
				}
				sent = event.Sequence
				replayed++
			}
			flusher.Flush()
		}
		if !sessionValid() {
			return
		}
		_, _ = fmt.Fprintf(
			w,
			"event: replay-complete\ndata: {\"host_epoch\":%q,\"sequence\":%d,\"replayed\":%d}\n\n",
			options.HostEpoch,
			sent,
			replayed,
		)
		flusher.Flush()

		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		if subscription == nil {
			return
		}
		for {
			select {
			case <-revoked:
				return
			case <-r.Context().Done():
				if errors.Is(r.Context().Err(), context.DeadlineExceeded) && sessionValid() {
					_, _ = fmt.Fprint(w, "event: stream-close\ndata: {\"code\":\"stream_lifetime_exceeded\",\"retry_after_seconds\":1}\n\n")
					flusher.Flush()
				}
				return
			case <-ticker.C:
				if !sessionValid() {
					return
				}
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case event, ok := <-subscription.Events:
				if !ok {
					return
				}
				if event.HostEpoch != options.HostEpoch || event.Sequence <= sent {
					continue
				}
				if !sessionValid() {
					return
				}
				if err := writeSSEEvent(w, event); err != nil {
					return
				}
				sent = event.Sequence
				flusher.Flush()
			}
		}
	}
}

func parseEventCursor(value, hostEpoch string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	index := strings.LastIndexByte(value, ':')
	if index < 1 || value[:index] != hostEpoch {
		return 0, errors.New("event cursor belongs to another host epoch")
	}
	sequence, err := strconv.ParseInt(value[index+1:], 10, 64)
	if err != nil || sequence < 0 {
		return 0, errors.New("event cursor sequence is invalid")
	}
	return sequence, nil
}

func writeSSEEvent(w http.ResponseWriter, event operations.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: operational-event\ndata: %s\n\n", event.ID, data)
	return err
}

type fieldViolation struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type fieldError struct {
	Field   string
	Message string
}

func (e *fieldError) Error() string { return e.Field + " " + e.Message }

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeErrorDetails(w, status, code, message, nil, nil)
}

func writeErrorDetails(
	w http.ResponseWriter, status int, code, message string,
	violations []fieldViolation, retry any,
) {
	if violations == nil {
		violations = []fieldViolation{}
	}
	writeJSON(w, status, map[string]any{
		"code": code, "message": message,
		"correlation_id":   w.Header().Get("X-Correlation-ID"),
		"field_violations": violations,
		"retry":            retry,
	})
}

func writeValidationError(w http.ResponseWriter, err error) {
	var value *fieldError
	if errors.As(err, &value) {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", err.Error(),
			[]fieldViolation{{Field: value.Field, Message: value.Message}}, nil)
		return
	}
	writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
}

func versionedFallback(w http.ResponseWriter, r *http.Request) {
	if methods := allowedMethods(r.URL.Path); methods != "" {
		if strings.Contains(", "+methods+", ", ", "+r.Method+", ") {
			writeError(w, http.StatusServiceUnavailable, "capability_unavailable", "API capability is unavailable")
			return
		}
		w.Header().Set("Allow", methods)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "API route was not found")
}

func allowedMethods(path string) string {
	exact := map[string]string{
		"/api/v1/system": "GET", "/api/v1/session": "GET",
		"/api/v1/hosts": "GET", "/api/v1/events": "GET",
		"/api/v1/events/query": "GET", "/api/v1/events/snapshot": "GET",
		"/api/v1/runners": "GET", "/api/v1/commands/preview": "POST",
		"/api/v1/commands": "POST", "/api/v1/diagnostics": "GET",
		"/api/v1/configuration": "GET", "/api/v1/search": "GET",
		"/api/v1/analytics": "GET", "/api/v1/saved-views": "GET, POST",
		"/api/v1/exports": "POST", "/api/v1/alerts": "GET",
		"/api/v1/incidents": "GET", "/api/v1/backups": "GET",
		"/api/v1/restores": "GET", "/api/v1/updates": "GET",
		"/api/v1/updates/check": "GET", "/api/v1/pools": "GET",
		"/api/v1/sessions": "GET", "/api/v1/repositories": "GET",
		"/api/v1/workflows": "GET", "/api/v1/annotations": "GET",
		"/api/v1/tags": "GET", "/api/v1/audit": "GET",
	}
	if methods := exact[path]; methods != "" {
		return methods
	}
	switch {
	case strings.HasPrefix(path, "/api/v1/commands/"),
		strings.HasPrefix(path, "/api/v1/support-bundles/"),
		strings.HasPrefix(path, "/api/v1/runs/"),
		strings.HasPrefix(path, "/api/v1/jobs/"),
		strings.HasPrefix(path, "/api/v1/exports/"),
		strings.HasPrefix(path, "/api/v1/backups/"):
		return "GET"
	case strings.HasPrefix(path, "/api/v1/saved-views/"):
		return "DELETE"
	case strings.HasPrefix(path, "/api/v1/alerts/"):
		if strings.HasSuffix(path, "/acknowledge") || strings.HasSuffix(path, "/silence") ||
			strings.HasSuffix(path, "/annotations") || strings.HasSuffix(path, "/resolve") {
			return "POST"
		}
		return "GET"
	case strings.HasPrefix(path, "/api/v1/incidents/"):
		return "GET"
	}
	return ""
}

func correlationHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID, err := operations.NewOpaqueID()
		if err != nil {
			correlationID = "unavailable"
		}
		w.Header().Set("X-Correlation-ID", correlationID)
		r.Header.Set("X-Correlation-ID", correlationID)
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
