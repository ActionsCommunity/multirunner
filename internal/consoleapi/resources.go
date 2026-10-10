package consoleapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
	"github.com/GerardSmit/multirunner/internal/history"
)

type ResourceReader interface {
	ListWorkflowRuns(context.Context, history.ListOptions) ([]history.WorkflowRun, error)
	ListWorkflowJobs(context.Context, history.ListOptions) ([]history.WorkflowJob, error)
	ListRunnerSessions(context.Context, history.ListOptions) ([]history.RunnerSession, error)
	ListAlertAnnotations(context.Context, string, int, int) ([]alerts.Annotation, error)
	ListAuditRecords(context.Context, int, int) ([]history.AuditRecord, error)
}

type listResponse[T any] struct {
	Items          []T            `json:"items"`
	AppliedFilters map[string]any `json:"applied_filters"`
	Cursor         string         `json:"cursor"`
	NextCursor     string         `json:"next_cursor"`
	Count          int            `json:"count"`
}

type listPage struct {
	Limit     int
	Offset    int
	Cursor    string
	AfterTime *time.Time
	AfterID   string
}

type stableListCursor struct {
	Version   int    `json:"v"`
	Timestamp int64  `json:"t"`
	ID        string `json:"id"`
}

func parseListPage(r *http.Request, defaultLimit, maximum int) (listPage, error) {
	limit := defaultLimit
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > maximum {
			return listPage{}, &fieldError{
				Field: "limit", Message: "must be between 1 and " + strconv.Itoa(maximum),
			}
		}
		limit = parsed
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	offset := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err == nil {
			if parsed < 0 {
				return listPage{}, &fieldError{Field: "cursor", Message: "must be a valid cursor"}
			}
			offset = parsed
		} else {
			decoded, decodeErr := decodeStableListCursor(cursor)
			if decodeErr != nil {
				return listPage{}, &fieldError{Field: "cursor", Message: "must be a valid cursor"}
			}
			after := time.UnixMilli(decoded.Timestamp).UTC()
			return listPage{
				Limit: limit, Cursor: cursor, AfterTime: &after, AfterID: decoded.ID,
			}, nil
		}
	}
	return listPage{Limit: limit, Offset: offset, Cursor: cursor}, nil
}

func encodeStableListCursor(timestamp time.Time, id string) (string, error) {
	encoded, err := json.Marshal(stableListCursor{
		Version: 1, Timestamp: timestamp.UTC().UnixMilli(), ID: id,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeStableListCursor(value string) (stableListCursor, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return stableListCursor{}, err
	}
	var cursor stableListCursor
	if err := json.Unmarshal(encoded, &cursor); err != nil ||
		cursor.Version != 1 || cursor.Timestamp < 0 || cursor.ID == "" {
		return stableListCursor{}, errors.New("invalid stable list cursor")
	}
	return cursor, nil
}

func pagedResponse[T any](
	values []T, page listPage, filters map[string]any,
) listResponse[T] {
	hasMore := len(values) > page.Limit
	if hasMore {
		values = values[:page.Limit]
	}
	if values == nil {
		values = []T{}
	}
	next := ""
	if hasMore {
		next = strconv.Itoa(page.Offset + len(values))
	}
	return listResponse[T]{
		Items: values, AppliedFilters: filters, Cursor: page.Cursor,
		NextCursor: next, Count: len(values),
	}
}

func hostListHandler(options Options) http.HandlerFunc {
	type host struct {
		ID         string `json:"id"`
		Epoch      string `json:"epoch"`
		Status     string `json:"status"`
		Mode       string `json:"mode"`
		AppVersion string `json:"app_version"`
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		items := []host{{
			ID: options.HostID, Epoch: options.HostEpoch, Status: "operational",
			Mode: "local", AppVersion: options.AppVersion,
		}}
		writeJSON(w, http.StatusOK, listResponse[host]{
			Items: items, AppliedFilters: map[string]any{},
			Cursor: "", NextCursor: "", Count: len(items),
		})
	}
}

func resourceListHandler(kind string, reader ResourceReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reader == nil {
			writeError(w, http.StatusServiceUnavailable, "history_unavailable", kind+" are unavailable")
			return
		}
		page, err := parseListPage(r, 50, 200)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		switch kind {
		case "sessions":
			filters := map[string]any{}
			repository := strings.TrimSpace(r.URL.Query().Get("repository"))
			status := strings.TrimSpace(r.URL.Query().Get("status"))
			if repository != "" {
				filters["repository"] = repository
			}
			if status != "" {
				filters["status"] = status
			}
			values, err := reader.ListRunnerSessions(r.Context(), history.ListOptions{
				Repository: repository, Status: status,
				Limit: page.Limit + 1, Offset: page.Offset,
				AfterTime: page.AfterTime, AfterID: page.AfterID,
			})
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "history_unavailable", "runner sessions are unavailable")
				return
			}
			hasMore := len(values) > page.Limit
			if hasMore {
				values = values[:page.Limit]
			}
			next := ""
			if hasMore && len(values) > 0 {
				next, err = encodeStableListCursor(
					values[len(values)-1].StartedAt, values[len(values)-1].ID,
				)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "query_failed", "pagination cursor could not be created")
					return
				}
			}
			writeJSON(w, http.StatusOK, listResponse[runnerSessionResponse]{
				Items: runnerSessionAPIResponses(values), AppliedFilters: filters,
				Cursor: page.Cursor, NextCursor: next, Count: len(values),
			})
		case "repositories", "workflows", "pools", "tags":
			items, err := aggregateResources(r.Context(), reader, kind)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "history_unavailable", kind+" are unavailable")
				return
			}
			start := min(page.Offset, len(items))
			end := min(start+page.Limit+1, len(items))
			writeJSON(w, http.StatusOK, pagedResponse(items[start:end], page, map[string]any{}))
		}
	}
}

type namedResource struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func aggregateResources(
	ctx context.Context, reader ResourceReader, kind string,
) ([]namedResource, error) {
	counts := map[string]int{}
	switch kind {
	case "pools":
		var afterTime *time.Time
		var afterID string
		for {
			values, err := reader.ListRunnerSessions(ctx, history.ListOptions{
				Limit: 1000, AfterTime: afterTime, AfterID: afterID,
			})
			if err != nil {
				return nil, err
			}
			for _, value := range values {
				if value.PoolName != "" {
					counts[value.PoolName]++
				}
			}
			if len(values) < 1000 {
				break
			}
			last := values[len(values)-1]
			afterTime, afterID = &last.StartedAt, last.ID
		}
	case "tags":
		for offset := 0; ; offset += 1000 {
			values, err := reader.ListWorkflowJobs(ctx, history.ListOptions{Limit: 1000, Offset: offset})
			if err != nil {
				return nil, err
			}
			for _, value := range values {
				for _, label := range value.Labels {
					if label = strings.TrimSpace(label); label != "" {
						counts[label]++
					}
				}
			}
			if len(values) < 1000 {
				break
			}
		}
	default:
		var afterTime *time.Time
		var afterID string
		for {
			values, err := reader.ListWorkflowRuns(ctx, history.ListOptions{
				Limit: 1000, AfterTime: afterTime, AfterID: afterID,
			})
			if err != nil {
				return nil, err
			}
			for _, value := range values {
				name := value.Repository
				if kind == "workflows" {
					name = value.WorkflowName
				}
				if name = strings.TrimSpace(name); name != "" {
					counts[name]++
				}
			}
			if len(values) < 1000 {
				break
			}
			last := values[len(values)-1]
			afterTime, afterID = &last.CreatedAt, strconv.FormatInt(last.ID, 10)
		}
	}
	result := make([]namedResource, 0, len(counts))
	for name, count := range counts {
		result = append(result, namedResource{Name: name, Count: count})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func annotationListHandler(reader ResourceReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reader == nil {
			writeError(w, http.StatusServiceUnavailable, "annotations_unavailable", "annotations are unavailable")
			return
		}
		page, err := parseListPage(r, 50, 200)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		if page.AfterTime != nil {
			writeValidationError(w, &fieldError{Field: "cursor", Message: "must be a non-negative offset"})
			return
		}
		alertID := strings.TrimSpace(r.URL.Query().Get("alert_id"))
		items, err := reader.ListAlertAnnotations(r.Context(), alertID, page.Limit+1, page.Offset)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "annotations_unavailable", "annotations are unavailable")
			return
		}
		filters := map[string]any{}
		if alertID != "" {
			filters["alert_id"] = alertID
		}
		writeJSON(w, http.StatusOK, pagedResponse(items, page, filters))
	}
}

func auditListHandler(reader ResourceReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reader == nil {
			writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "audit history is unavailable")
			return
		}
		page, err := parseListPage(r, 50, 200)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		if page.AfterTime != nil {
			writeValidationError(w, &fieldError{Field: "cursor", Message: "must be a non-negative offset"})
			return
		}
		items, err := reader.ListAuditRecords(r.Context(), page.Limit+1, page.Offset)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "audit history is unavailable")
			return
		}
		writeJSON(w, http.StatusOK, pagedResponse(items, page, map[string]any{}))
	}
}
