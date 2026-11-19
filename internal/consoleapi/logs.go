package consoleapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/transientlog"
)

func jobLogHandler(reader JobLogReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || jobID <= 0 {
			writeError(w, http.StatusBadRequest, "validation_failed", "job ID must be positive")
			return
		}
		session, ok := consoleauth.SessionFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication_required", "authentication is required")
			return
		}
		result, err := reader.Fetch(
			r.Context(), session.ActorID, wCorrelationID(r), jobID,
		)
		if errors.Is(err, transientlog.ErrAuditUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "job log access could not be audited")
			return
		}
		if err != nil {
			writeError(w, http.StatusBadGateway, "job_log_unavailable", "job log is unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Set("X-Log-Repository", result.Repository)
		w.Header().Set("X-Log-Bytes", strconv.Itoa(len(result.Content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Content)
	}
}
