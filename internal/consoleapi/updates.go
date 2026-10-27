package consoleapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/GerardSmit/multirunner/internal/update"
)

func updateListHandler(reader UpdateReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := parseListPage(r, 100, 500)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		items, err := reader.ListUpdates(r.Context(), 500)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "updates_unavailable", "updates are unavailable")
			return
		}
		start := min(page.Offset, len(items))
		end := min(start+page.Limit+1, len(items))
		writeJSON(w, http.StatusOK, pagedResponse(items[start:end], page, map[string]any{}))
	}
}

func updateInspectionHandler(inspector UpdateInspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if inspector == nil {
			writeJSON(w, http.StatusOK, update.Inspection{
				CheckedAt: time.Now().UTC(),
				Reason:    "update metadata URL is not configured",
			})
			return
		}
		inspection, err := inspector.Inspect(r.Context())
		switch {
		case errors.Is(err, update.ErrUntrusted),
			errors.Is(err, update.ErrExpired),
			errors.Is(err, update.ErrRollback),
			errors.Is(err, update.ErrIntegrity):
			writeError(w, http.StatusConflict, "update_verification_failed", err.Error())
		case errors.Is(err, update.ErrNotFound),
			errors.Is(err, update.ErrIncompatible):
			writeError(w, http.StatusUnprocessableEntity, "update_incompatible", err.Error())
		case err != nil:
			writeError(w, http.StatusBadGateway, "update_check_failed", "update metadata is unavailable")
		default:
			writeJSON(w, http.StatusOK, inspection)
		}
	}
}
