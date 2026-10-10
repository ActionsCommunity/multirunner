package consoleapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/operations"
	"github.com/GerardSmit/multirunner/internal/searchexport"
)

func savedViewsHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			views, err := options.SavedViews.ListSavedViews(r.Context())
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "saved_views_unavailable", "saved views are unavailable")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":           views,
				"count":           len(views),
				"cursor":          "",
				"next_cursor":     "",
				"applied_filters": map[string]any{},
			})
		case http.MethodPost:
			createSavedView(w, r, options)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
	}
}

func createSavedView(w http.ResponseWriter, r *http.Request, options Options) {
	if !validMutationRequest(r, options) {
		writeError(w, http.StatusForbidden, "csrf_failed", "mutation request origin or CSRF state is invalid")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var input history.SavedViewInput
	if !decodeBoundedJSON(w, r, &input) {
		return
	}
	session, ok := consoleauth.SessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "authentication is required")
		return
	}
	view, created, err := options.SavedViews.CreateSavedView(
		r.Context(), idempotencyKey, session.ActorID, input,
	)
	switch {
	case errors.Is(err, history.ErrSavedViewIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input")
	case errors.Is(err, history.ErrSavedViewNameConflict):
		writeError(w, http.StatusConflict, "name_conflict", "a saved view already uses this name")
	case errors.Is(err, history.ErrInvalidSearch):
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
	case err != nil:
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
	default:
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, view.Version))
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, view)
	}
}

func deleteSavedViewHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validMutationRequest(r, options) {
			writeError(w, http.StatusForbidden, "csrf_failed", "mutation request origin or CSRF state is invalid")
			return
		}
		version, err := parseEntityVersion(r.Header.Get("If-Match"))
		if err != nil {
			writeError(w, http.StatusPreconditionRequired, "version_required", "If-Match must contain the saved view version")
			return
		}
		_, err = options.SavedViews.DeleteSavedView(
			r.Context(), r.PathValue("id"),
			strings.TrimSpace(r.Header.Get("Idempotency-Key")), version,
		)
		switch {
		case errors.Is(err, history.ErrSavedViewNotFound):
			writeError(w, http.StatusNotFound, "not_found", "saved view was not found")
		case errors.Is(err, history.ErrSavedViewConflict):
			writeError(w, http.StatusPreconditionFailed, "version_conflict", "saved view changed before it could be deleted")
		case errors.Is(err, history.ErrSavedViewIdempotencyConflict):
			writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for a different deletion")
		case err != nil:
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func createSearchExportHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validMutationRequest(r, options) {
			writeError(w, http.StatusForbidden, "csrf_failed", "mutation request origin or CSRF state is invalid")
			return
		}
		var request searchexport.Request
		if !decodeBoundedJSON(w, r, &request) {
			return
		}
		session, ok := consoleauth.SessionFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication_required", "authentication is required")
			return
		}
		audit, err := searchExportAudit(
			session.ActorID, "history.export.generate", wCorrelationID(r),
		)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "export audit could not be created")
			return
		}
		metadata, created, err := options.Exports.Generate(
			r.Context(), strings.TrimSpace(r.Header.Get("Idempotency-Key")), request, audit,
		)
		switch {
		case errors.Is(err, searchexport.ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input")
		case errors.Is(err, searchexport.ErrInvalidRequest):
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		case errors.Is(err, history.ErrInvalidSearch):
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		case err != nil:
			writeError(w, http.StatusServiceUnavailable, "export_unavailable", "search export could not be generated")
		default:
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			writeJSON(w, status, metadata)
		}
	}
}

func downloadSearchExportHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := consoleauth.SessionFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication_required", "authentication is required")
			return
		}
		audit, err := searchExportAudit(
			session.ActorID, "history.export.download", wCorrelationID(r),
		)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "export audit could not be created")
			return
		}
		metadata, file, err := options.Exports.Open(r.Context(), r.PathValue("id"), audit)
		if errors.Is(err, searchexport.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "search export was not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "export_unavailable", "search export is unavailable")
			return
		}
		defer file.Close()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, metadata.FileName))
		w.Header().Set("X-Content-SHA256", metadata.SHA256)
		http.ServeContent(w, r, metadata.FileName, metadata.CreatedAt, file)
	}
}

func decodeBoundedJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "validation_failed", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCommandBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeCommandDecodeError(w, err)
		return false
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "request body must contain one JSON object")
		return false
	}
	return true
}

func parseEntityVersion(value string) (int64, error) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	version, err := strconv.ParseInt(value, 10, 64)
	if err != nil || version < 1 {
		return 0, errors.New("invalid entity version")
	}
	return version, nil
}

func searchExportAudit(actorID, action, correlationID string) (searchexport.Audit, error) {
	id, err := operations.NewOpaqueID()
	if err != nil {
		return searchexport.Audit{}, err
	}
	return searchexport.Audit{
		ID: id, OccurredAt: time.Now().UTC(), ActorKind: string(operations.ActorOperator),
		ActorID: actorID, Action: action, TargetType: "search_export",
		CorrelationID: correlationID, Outcome: "succeeded",
	}, nil
}
