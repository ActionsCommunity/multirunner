package consoleapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/GerardSmit/multirunner/internal/history"
)

func searchHandler(reader SearchReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := parseListPage(r, 25, 100)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		options := history.SearchOptions{
			Query:      strings.TrimSpace(r.URL.Query().Get("q")),
			EntityType: strings.TrimSpace(r.URL.Query().Get("type")),
			Repository: strings.TrimSpace(r.URL.Query().Get("repository")),
			State:      strings.TrimSpace(r.URL.Query().Get("state")),
			Limit:      min(100, page.Offset+page.Limit+1),
		}
		results, err := reader.Search(r.Context(), options)
		if errors.Is(err, history.ErrInvalidSearch) {
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "search_unavailable", "search is unavailable")
			return
		}
		start := min(page.Offset, len(results))
		end := min(start+page.Limit+1, len(results))
		writeJSON(w, http.StatusOK, pagedResponse(results[start:end], page, map[string]any{
			"type": options.EntityType, "repository": options.Repository, "state": options.State,
		}))
	}
}
