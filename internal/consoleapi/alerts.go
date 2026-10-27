package consoleapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/operations"
)

func alertListHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		listOptions := alerts.ListOptions{
			State:    alerts.State(strings.TrimSpace(query.Get("state"))),
			Severity: strings.TrimSpace(query.Get("severity")),
		}
		page, err := parseListPage(r, 100, 500)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		listOptions.Limit = page.Limit + 1
		listOptions.Offset = page.Offset
		if listOptions.State != "" && listOptions.State != alerts.StatePending &&
			listOptions.State != alerts.StateOpen && listOptions.State != alerts.StateResolved {
			writeError(w, http.StatusBadRequest, "validation_failed", "state must be pending, open, or resolved")
			return
		}
		if listOptions.Severity != "" && !validAlertSeverity(listOptions.Severity) {
			writeError(w, http.StatusBadRequest, "validation_failed", "severity must be critical, high, medium, or low")
			return
		}
		items, err := options.Alerts.ListAlerts(r.Context(), listOptions)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "alerts_unavailable", "alerts are unavailable")
			return
		}
		filters := map[string]any{}
		if listOptions.State != "" {
			filters["state"] = listOptions.State
		}
		if listOptions.Severity != "" {
			filters["severity"] = listOptions.Severity
		}
		writeJSON(w, http.StatusOK, pagedResponse(items, page, filters))
	}
}

func alertReadHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		instance, err := options.Alerts.Alert(r.Context(), r.PathValue("id"))
		if errors.Is(err, alerts.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "alert was not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "alerts_unavailable", "alert is unavailable")
			return
		}
		w.Header().Set("ETag", strconv.Quote(strconv.Itoa(instance.Version)))
		writeJSON(w, http.StatusOK, instance)
	}
}

func alertAcknowledgeHandler(options Options) http.HandlerFunc {
	type input struct {
		Reason string `json:"reason"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		session, version, key, ok := alertMutationContext(w, r, options)
		if !ok {
			return
		}
		var body input
		if !decodeBoundedJSON(w, r, &body) {
			return
		}
		body.Reason = strings.TrimSpace(body.Reason)
		if len(body.Reason) > 500 {
			writeError(w, http.StatusBadRequest, "validation_failed", "reason cannot exceed 500 characters")
			return
		}
		instance, err := options.Alerts.AcknowledgeAlert(r.Context(), alerts.AcknowledgeRequest{
			AlertID: r.PathValue("id"), IdempotencyKey: key,
			ExpectedVersion: version, ActorID: session.ActorID, Reason: body.Reason,
			CorrelationID: wCorrelationID(r), OccurredAt: time.Now(),
		})
		writeAlertMutationResult(w, instance, err)
	}
}

func alertSilenceHandler(options Options) http.HandlerFunc {
	type input struct {
		Until  time.Time `json:"until"`
		Reason string    `json:"reason"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		session, version, key, ok := alertMutationContext(w, r, options)
		if !ok {
			return
		}
		var body input
		if !decodeBoundedJSON(w, r, &body) {
			return
		}
		body.Reason = strings.TrimSpace(body.Reason)
		instance, silence, err := options.Alerts.SilenceAlert(r.Context(), alerts.SilenceRequest{
			AlertID: r.PathValue("id"), IdempotencyKey: key,
			ExpectedVersion: version, Until: body.Until, ActorID: session.ActorID,
			Reason: body.Reason, CorrelationID: wCorrelationID(r), OccurredAt: time.Now(),
		})
		if err != nil {
			writeAlertMutationResult(w, alerts.Instance{}, err)
			return
		}
		w.Header().Set("ETag", strconv.Quote(strconv.Itoa(instance.Version)))
		writeJSON(w, http.StatusOK, map[string]any{
			"alert": instance, "silence": silence,
		})
	}
}

func alertAnnotationHandler(options Options) http.HandlerFunc {
	type input struct {
		Body string `json:"body"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		session, _, key, ok := alertMutationContextWithoutVersion(w, r, options)
		if !ok {
			return
		}
		var body input
		if !decodeBoundedJSON(w, r, &body) {
			return
		}
		annotation, err := options.Alerts.AddAlertAnnotation(r.Context(), alerts.AnnotationRequest{
			AlertID: r.PathValue("id"), IdempotencyKey: key, Body: body.Body,
			ActorID: session.ActorID, CorrelationID: wCorrelationID(r),
			OccurredAt: time.Now(),
		})
		switch {
		case errors.Is(err, alerts.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "alert was not found")
		case errors.Is(err, alerts.ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input")
		case err != nil:
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		default:
			writeJSON(w, http.StatusCreated, annotation)
		}
	}
}

func alertResolveHandler(options Options) http.HandlerFunc {
	type input struct {
		Reason string `json:"reason"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		session, version, key, ok := alertMutationContext(w, r, options)
		if !ok {
			return
		}
		var body input
		if !decodeBoundedJSON(w, r, &body) {
			return
		}
		body.Reason = strings.TrimSpace(body.Reason)
		if body.Reason == "" || len(body.Reason) > 500 {
			writeError(w, http.StatusBadRequest, "validation_failed", "reason must contain 1 to 500 characters")
			return
		}
		instance, err := options.Alerts.TransitionAlert(r.Context(), alerts.TransitionRequest{
			AlertID: r.PathValue("id"), IdempotencyKey: key,
			ExpectedState: alerts.StateOpen, ExpectedVersion: version,
			To: alerts.StateResolved, ActorKind: operations.ActorOperator,
			ActorID: session.ActorID, Reason: body.Reason,
			CorrelationID: wCorrelationID(r), OccurredAt: time.Now(),
		})
		writeAlertMutationResult(w, instance, err)
	}
}

func alertMutationContext(
	w http.ResponseWriter, r *http.Request, options Options,
) (consoleauth.Session, int, string, bool) {
	session, _, key, ok := alertMutationContextWithoutVersion(w, r, options)
	if !ok {
		return consoleauth.Session{}, 0, "", false
	}
	version, err := parseEntityVersion(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "version_required", "If-Match must contain the alert version")
		return consoleauth.Session{}, 0, "", false
	}
	return session, int(version), key, true
}

func alertMutationContextWithoutVersion(
	w http.ResponseWriter, r *http.Request, options Options,
) (consoleauth.Session, int, string, bool) {
	if !validMutationRequest(r, options) {
		writeError(w, http.StatusForbidden, "csrf_failed", "mutation request origin or CSRF state is invalid")
		return consoleauth.Session{}, 0, "", false
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "validation_failed", "Idempotency-Key must contain 1 to 128 characters")
		return consoleauth.Session{}, 0, "", false
	}
	session, ok := consoleauth.SessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "authentication is required")
		return consoleauth.Session{}, 0, "", false
	}
	return session, 0, key, true
}

func writeAlertMutationResult(w http.ResponseWriter, instance alerts.Instance, err error) {
	switch {
	case errors.Is(err, alerts.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "alert was not found")
	case errors.Is(err, alerts.ErrStateConflict):
		writeError(w, http.StatusPreconditionFailed, "version_conflict", "alert changed before the mutation could be applied")
	case errors.Is(err, alerts.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input")
	case err != nil:
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
	default:
		w.Header().Set("ETag", strconv.Quote(strconv.Itoa(instance.Version)))
		writeJSON(w, http.StatusOK, instance)
	}
}

func validAlertSeverity(value string) bool {
	switch value {
	case "critical", "high", "medium", "low":
		return true
	default:
		return false
	}
}
