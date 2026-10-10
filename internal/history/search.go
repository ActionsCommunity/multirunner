package history

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

var ErrInvalidSearch = errors.New("invalid search query")

type SearchOptions struct {
	Query      string
	EntityType string
	Repository string
	State      string
	Limit      int
}

type SearchResult struct {
	EntityType string    `json:"entity_type"`
	EntityKey  string    `json:"entity_key"`
	Repository string    `json:"repository,omitempty"`
	Title      string    `json:"title"`
	Context    string    `json:"context"`
	Timestamp  time.Time `json:"timestamp"`
	State      string    `json:"state"`
	Route      string    `json:"route"`
}

func (s *Store) Search(ctx context.Context, options SearchOptions) ([]SearchResult, error) {
	return s.search(ctx, options, 100)
}

func (s *Store) search(
	ctx context.Context, options SearchOptions, maximumLimit int,
) ([]SearchResult, error) {
	query, err := searchExpression(options.Query)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSearch, err)
	}
	if options.EntityType != "" && !validSearchEntityType(options.EntityType) {
		return nil, fmt.Errorf("%w: unsupported search entity type %q",
			ErrInvalidSearch, options.EntityType)
	}
	limit := options.Limit
	if limit <= 0 {
		limit = 25
	}
	if maximumLimit < 1 {
		maximumLimit = 100
	}
	if limit > maximumLimit {
		limit = maximumLimit
	}
	statement := `SELECT entity_type, entity_key, repository, title, context,
		CAST(timestamp AS INTEGER), state
		FROM search_index WHERE search_index MATCH ?`
	args := []any{query}
	if options.EntityType != "" {
		statement += ` AND entity_type=?`
		args = append(args, options.EntityType)
	}
	if options.Repository != "" {
		statement += ` AND repository=?`
		args = append(args, options.Repository)
	}
	if options.State != "" {
		statement += ` AND state=?`
		args = append(args, options.State)
	}
	statement += ` ORDER BY bm25(search_index), CAST(timestamp AS INTEGER) DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("search history: %w", err)
	}
	defer rows.Close()
	var results []SearchResult
	for rows.Next() {
		var result SearchResult
		var timestamp int64
		if err := rows.Scan(
			&result.EntityType, &result.EntityKey, &result.Repository,
			&result.Title, &result.Context, &timestamp, &result.State,
		); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		result.Timestamp = millisTime(timestamp)
		result.Route = searchRoute(result)
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search history: %w", err)
	}
	return results, nil
}

func searchExpression(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return "", errors.New("search query must contain at least 2 characters")
	}
	if len(value) > 256 {
		return "", errors.New("search query cannot exceed 256 characters")
	}
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return r
		}
		return ' '
	}, value)
	terms := strings.Fields(cleaned)
	if len(terms) == 0 {
		return "", errors.New("search query must contain a letter or number")
	}
	if len(terms) > 8 {
		return "", errors.New("search query cannot exceed 8 terms")
	}
	for index := range terms {
		if len(terms[index]) > 64 {
			return "", errors.New("search terms cannot exceed 64 characters")
		}
		terms[index] = `"` + terms[index] + `"*`
	}
	return strings.Join(terms, " "), nil
}

func validSearchEntityType(value string) bool {
	switch value {
	case "run", "job", "step", "runner", "command", "alert", "annotation":
		return true
	default:
		return false
	}
}

func searchRoute(result SearchResult) string {
	repository := url.QueryEscape(result.Repository)
	switch result.EntityType {
	case "run":
		_, id := splitEntityKey(result.EntityKey)
		return "/runs?repository=" + repository + "&run_id=" + url.QueryEscape(id)
	case "job":
		_, id := splitEntityKey(result.EntityKey)
		return "/runs?repository=" + repository + "&job_id=" + url.QueryEscape(id)
	case "step":
		remainder, number := splitEntityKey(result.EntityKey)
		_, jobID := splitEntityKey(remainder)
		return "/runs?repository=" + repository + "&job_id=" +
			url.QueryEscape(jobID) + "&step=" + url.QueryEscape(number)
	case "runner":
		return "/runners?session_id=" + url.QueryEscape(result.EntityKey)
	case "command":
		return "/audit?command_id=" + url.QueryEscape(result.EntityKey)
	case "alert":
		return "/alerts?alert_id=" + url.QueryEscape(result.EntityKey)
	case "annotation":
		alertID, _ := splitEntityKey(result.EntityKey)
		return "/alerts?alert_id=" + url.QueryEscape(alertID)
	default:
		return ""
	}
}

func splitEntityKey(value string) (string, string) {
	index := strings.LastIndexByte(value, ':')
	if index < 0 {
		return "", value
	}
	return value[:index], value[index+1:]
}
