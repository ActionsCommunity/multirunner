package consoleapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/history"
)

var analyticsRouteTimeout = 5 * time.Second

func analyticsHandler(reader AnalyticsReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		since, err := optionalRFC3339(r.URL.Query().Get("since"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "since must be RFC 3339")
			return
		}
		until, err := optionalRFC3339(r.URL.Query().Get("until"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "until must be RFC 3339")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), analyticsRouteTimeout)
		defer cancel()
		report, err := reader.Analytics(ctx, history.AnalyticsOptions{
			GroupBy: strings.TrimSpace(r.URL.Query().Get("group_by")),
			Since:   since, Until: until,
		})
		if errors.Is(err, history.ErrInvalidAnalytics) {
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusServiceUnavailable, "analytics_timeout", "analytics exceeded the route time budget")
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "analytics_unavailable", "analytics are unavailable")
			return
		}
		writeJSON(w, http.StatusOK, report)
	}
}

func optionalRFC3339(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}
