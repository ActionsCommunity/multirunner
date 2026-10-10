package consoleapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/GerardSmit/multirunner/internal/operations"
)

const (
	defaultQueryLimit = 50
	maxQueryLimit     = 200
)

type queryCursor struct {
	Kind        string `json:"kind"`
	HostEpoch   string `json:"host_epoch"`
	MaxSequence int64  `json:"max_sequence"`
	Position    int64  `json:"position"`
	FilterHash  string `json:"filter_hash,omitempty"`
}

type collectionResponse[T any] struct {
	Items         []T            `json:"items"`
	AppliedFilter map[string]any `json:"applied_filters"`
	Cursor        string         `json:"cursor"`
	NextCursor    string         `json:"next_cursor"`
	Count         int            `json:"count"`
	HostEpoch     string         `json:"host_epoch"`
	MaxSequence   int64          `json:"max_sequence"`
}

func eventQueryHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, err := queryLimit(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		filters := map[string]string{
			"type":        strings.TrimSpace(r.URL.Query().Get("type")),
			"entity_type": strings.TrimSpace(r.URL.Query().Get("entity_type")),
			"entity_id":   strings.TrimSpace(r.URL.Query().Get("entity_id")),
		}
		filterHash := queryFilterHash(filters)
		cursorValue := r.URL.Query().Get("cursor")
		cursor, err := decodeQueryCursor(cursorValue, "events", options.HostEpoch, filterHash)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		if cursor.MaxSequence == 0 {
			_, cursor.MaxSequence, err = options.Events.OperationalEventBounds(r.Context(), options.HostEpoch)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "events_unavailable", "operational events are unavailable")
				return
			}
		}
		events, err := options.Events.ListOperationalEvents(r.Context(), operations.EventQuery{
			HostEpoch: options.HostEpoch, AfterSequence: cursor.Position,
			MaxSequence: cursor.MaxSequence, Type: filters["type"],
			EntityType: filters["entity_type"], EntityID: filters["entity_id"],
			Limit: limit + 1,
		})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "events_unavailable", "operational events are unavailable")
			return
		}
		hasMore := len(events) > limit
		if hasMore {
			events = events[:limit]
		}
		nextCursor := ""
		if hasMore && len(events) > 0 {
			cursor.Position = events[len(events)-1].Sequence
			cursor.FilterHash = filterHash
			nextCursor, err = encodeQueryCursor(cursor)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "query_failed", "query cursor could not be created")
				return
			}
		}
		writeJSON(w, http.StatusOK, collectionResponse[operations.Event]{
			Items: events, AppliedFilter: stringFilters(filters), Cursor: cursorValue,
			NextCursor: nextCursor, Count: len(events), HostEpoch: options.HostEpoch,
			MaxSequence: cursor.MaxSequence,
		})
	}
}

func runnerQueryHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, err := queryLimit(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		cursorValue := r.URL.Query().Get("cursor")
		cursor, err := decodeQueryCursor(cursorValue, "runners", options.HostEpoch, "")
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		if cursor.MaxSequence == 0 {
			_, cursor.MaxSequence, err = options.Events.OperationalEventBounds(r.Context(), options.HostEpoch)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "runners_unavailable", "runner state is unavailable")
				return
			}
		}
		snapshot, err := options.Events.OperationalSnapshotAt(r.Context(), options.HostEpoch, cursor.MaxSequence)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "runners_unavailable", "runner state is unavailable")
			return
		}
		start := int(cursor.Position)
		if start > len(snapshot.Runners) {
			writeError(w, http.StatusBadRequest, "validation_failed", "query cursor is outside the runner collection")
			return
		}
		end := min(start+limit, len(snapshot.Runners))
		runners := snapshot.Runners[start:end]
		nextCursor := ""
		if end < len(snapshot.Runners) {
			cursor.Position = int64(end)
			nextCursor, err = encodeQueryCursor(cursor)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "query_failed", "query cursor could not be created")
				return
			}
		}
		writeJSON(w, http.StatusOK, collectionResponse[operations.RunnerState]{
			Items: runners, AppliedFilter: map[string]any{}, Cursor: cursorValue,
			NextCursor: nextCursor, Count: len(runners), HostEpoch: options.HostEpoch,
			MaxSequence: cursor.MaxSequence,
		})
	}
}

func queryLimit(r *http.Request) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return defaultQueryLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxQueryLimit {
		return 0, errors.New("limit must be between 1 and 200")
	}
	return limit, nil
}

func decodeQueryCursor(value, kind, hostEpoch, filterHash string) (queryCursor, error) {
	if value == "" {
		return queryCursor{Kind: kind, HostEpoch: hostEpoch, FilterHash: filterHash}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return queryCursor{}, errors.New("query cursor is invalid")
	}
	var cursor queryCursor
	if err := json.Unmarshal(data, &cursor); err != nil ||
		cursor.Kind != kind || cursor.HostEpoch != hostEpoch ||
		cursor.MaxSequence < 0 || cursor.Position < 0 ||
		cursor.FilterHash != filterHash {
		return queryCursor{}, errors.New("query cursor does not match this request")
	}
	return cursor, nil
}

func encodeQueryCursor(cursor queryCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func queryFilterHash(filters map[string]string) string {
	canonical := filters["type"] + "\x00" + filters["entity_type"] + "\x00" + filters["entity_id"]
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

func stringFilters(filters map[string]string) map[string]any {
	result := make(map[string]any)
	for key, value := range filters {
		if value != "" {
			result[key] = value
		}
	}
	return result
}
