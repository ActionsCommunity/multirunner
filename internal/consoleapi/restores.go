package consoleapi

import (
	"net/http"
)

func restoreListHandler(reader RestoreReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := parseListPage(r, 100, 500)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		items, err := reader.List(r.Context(), 500)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "restores_unavailable", "restores are unavailable")
			return
		}
		start := min(page.Offset, len(items))
		end := min(start+page.Limit+1, len(items))
		writeJSON(w, http.StatusOK, pagedResponse(items[start:end], page, map[string]any{}))
	}
}
