package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

var ErrInvalidAnalytics = errors.New("invalid analytics query")

const defaultAnalyticsSnapshotTTL = 5 * time.Minute

type AnalyticsOptions struct {
	GroupBy string
	Since   time.Time
	Until   time.Time
}

type AnalyticsRow struct {
	Key                    string  `json:"key"`
	Repository             string  `json:"repository"`
	Workflow               string  `json:"workflow,omitempty"`
	TotalJobs              int64   `json:"total_jobs"`
	SuccessfulJobs         int64   `json:"successful_jobs"`
	FailedJobs             int64   `json:"failed_jobs"`
	CancelledJobs          int64   `json:"cancelled_jobs"`
	InfrastructureFailures int64   `json:"infrastructure_failures"`
	ExactAttributions      int64   `json:"exact_attributions"`
	SuccessRate            float64 `json:"success_rate"`
	AverageDurationSeconds float64 `json:"average_duration_seconds"`
	P50DurationSeconds     float64 `json:"p50_duration_seconds"`
	P95DurationSeconds     float64 `json:"p95_duration_seconds"`
}

type AnalyticsReport struct {
	GeneratedAt time.Time      `json:"generated_at"`
	GroupBy     string         `json:"group_by"`
	Since       time.Time      `json:"since"`
	Until       time.Time      `json:"until"`
	Rows        []AnalyticsRow `json:"rows"`
}

type analyticsAccumulator struct {
	row       AnalyticsRow
	durations []float64
}

func (s *Store) Analytics(
	ctx context.Context, options AnalyticsOptions,
) (AnalyticsReport, error) {
	defaultWindow := options.Since.IsZero() && options.Until.IsZero()
	options, err := normalizeAnalyticsOptions(options, nowUTC())
	if err != nil {
		return AnalyticsReport{}, err
	}
	cacheKey := analyticsCacheKey(options, defaultWindow)
	version, err := s.analyticsVersion(ctx)
	if err != nil {
		return AnalyticsReport{}, err
	}
	maxAge := time.Duration(0)
	if defaultWindow {
		maxAge = defaultAnalyticsSnapshotTTL
	}
	if report, ok, err := s.cachedAnalytics(ctx, cacheKey, version, maxAge); err != nil {
		return AnalyticsReport{}, err
	} else if ok {
		return report, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		repository, workflow_name, conclusion, attribution_source,
		started_at, completed_at
		FROM workflow_jobs
		WHERE created_at>=? AND created_at<=?
		ORDER BY created_at ASC LIMIT 200001`,
		timeMillis(options.Since), timeMillis(options.Until))
	if err != nil {
		return AnalyticsReport{}, fmt.Errorf("query analytics source: %w", err)
	}
	defer rows.Close()
	groups := make(map[string]*analyticsAccumulator)
	count := 0
	for rows.Next() {
		count++
		if count > 200000 {
			return AnalyticsReport{}, fmt.Errorf("%w: analytics source exceeds 200000 jobs",
				ErrInvalidAnalytics)
		}
		var repository, workflow, conclusion, attribution string
		var started, completed sql.NullInt64
		if err := rows.Scan(
			&repository, &workflow, &conclusion, &attribution,
			&started, &completed,
		); err != nil {
			return AnalyticsReport{}, fmt.Errorf("scan analytics source: %w", err)
		}
		key := repository
		if options.GroupBy == "workflow" {
			key += "\x00" + workflow
		}
		group := groups[key]
		if group == nil {
			group = &analyticsAccumulator{row: AnalyticsRow{
				Key: key, Repository: repository, Workflow: workflow,
			}}
			if options.GroupBy == "repository" {
				group.row.Workflow = ""
			}
			groups[key] = group
		}
		group.row.TotalJobs++
		switch conclusion {
		case "success":
			group.row.SuccessfulJobs++
		case "failure", "timed_out", "startup_failure":
			group.row.FailedJobs++
		case "cancelled":
			group.row.CancelledJobs++
		}
		if conclusion == "failure" && attribution != "exact" {
			group.row.InfrastructureFailures++
		}
		if attribution == "exact" {
			group.row.ExactAttributions++
		}
		if started.Valid && completed.Valid && completed.Int64 >= started.Int64 {
			group.durations = append(group.durations,
				float64(completed.Int64-started.Int64)/1000)
		}
	}
	if err := rows.Err(); err != nil {
		return AnalyticsReport{}, fmt.Errorf("query analytics source: %w", err)
	}
	if err := rows.Close(); err != nil {
		return AnalyticsReport{}, fmt.Errorf("close analytics source: %w", err)
	}
	report := AnalyticsReport{
		GeneratedAt: nowUTC(), GroupBy: options.GroupBy,
		Since: options.Since, Until: options.Until,
		Rows: make([]AnalyticsRow, 0, len(groups)),
	}
	for _, group := range groups {
		if group.row.TotalJobs > 0 {
			group.row.SuccessRate =
				float64(group.row.SuccessfulJobs) / float64(group.row.TotalJobs)
		}
		if len(group.durations) > 0 {
			sort.Float64s(group.durations)
			var total float64
			for _, duration := range group.durations {
				total += duration
			}
			group.row.AverageDurationSeconds = total / float64(len(group.durations))
			group.row.P50DurationSeconds = percentile(group.durations, 0.50)
			group.row.P95DurationSeconds = percentile(group.durations, 0.95)
		}
		report.Rows = append(report.Rows, group.row)
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		if report.Rows[i].TotalJobs != report.Rows[j].TotalJobs {
			return report.Rows[i].TotalJobs > report.Rows[j].TotalJobs
		}
		return report.Rows[i].Key < report.Rows[j].Key
	})
	if err := s.storeAnalyticsSnapshot(ctx, cacheKey, version, report); err != nil {
		return AnalyticsReport{}, err
	}
	return report, nil
}

func analyticsCacheKey(options AnalyticsOptions, defaultWindow bool) string {
	if defaultWindow {
		return "default:" + options.GroupBy
	}
	return fmt.Sprintf("%s:%d:%d", options.GroupBy,
		timeMillis(options.Since), timeMillis(options.Until))
}

func (s *Store) analyticsVersion(ctx context.Context) (int64, error) {
	var version int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT source_version FROM analytics_state WHERE id=1`,
	).Scan(&version); err != nil {
		return 0, fmt.Errorf("read analytics source version: %w", err)
	}
	return version, nil
}

func advanceAnalyticsVersionTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE analytics_state SET source_version=source_version+1 WHERE id=1`,
	); err != nil {
		return fmt.Errorf("advance analytics source version: %w", err)
	}
	return nil
}

func (s *Store) cachedAnalytics(
	ctx context.Context, cacheKey string, version int64, maxAge time.Duration,
) (AnalyticsReport, bool, error) {
	var encoded []byte
	var generatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT generated_at, report_json
		FROM analytics_snapshots WHERE cache_key=? AND source_version=?`,
		cacheKey, version).Scan(&generatedAt, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return AnalyticsReport{}, false, nil
	}
	if err != nil {
		return AnalyticsReport{}, false, fmt.Errorf("read analytics snapshot: %w", err)
	}
	if maxAge > 0 && nowUTC().Sub(millisTime(generatedAt)) >= maxAge {
		return AnalyticsReport{}, false, nil
	}
	var report AnalyticsReport
	if err := json.Unmarshal(encoded, &report); err != nil {
		return AnalyticsReport{}, false, fmt.Errorf("decode analytics snapshot: %w", err)
	}
	return report, true, nil
}

func (s *Store) storeAnalyticsSnapshot(
	ctx context.Context, cacheKey string, version int64, report AnalyticsReport,
) error {
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode analytics snapshot: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO analytics_snapshots (
		cache_key, source_version, generated_at, report_json
	) VALUES (?, ?, ?, ?)
	ON CONFLICT(cache_key) DO UPDATE SET
		source_version=excluded.source_version,
		generated_at=excluded.generated_at,
		report_json=excluded.report_json`,
		cacheKey, version, timeMillis(report.GeneratedAt), encoded); err != nil {
		return fmt.Errorf("store analytics snapshot: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM analytics_snapshots
		WHERE cache_key NOT IN (
			SELECT cache_key FROM analytics_snapshots
			ORDER BY generated_at DESC LIMIT 32
		)`); err != nil {
		return fmt.Errorf("prune analytics snapshots: %w", err)
	}
	return nil
}

func normalizeAnalyticsOptions(
	options AnalyticsOptions, now time.Time,
) (AnalyticsOptions, error) {
	if options.GroupBy == "" {
		options.GroupBy = "repository"
	}
	if options.GroupBy != "repository" && options.GroupBy != "workflow" {
		return AnalyticsOptions{}, fmt.Errorf("%w: group_by must be repository or workflow",
			ErrInvalidAnalytics)
	}
	if options.Until.IsZero() {
		options.Until = now
	}
	if options.Since.IsZero() {
		options.Since = options.Until.AddDate(0, 0, -30)
	}
	options.Since = options.Since.UTC()
	options.Until = options.Until.UTC()
	if options.Since.After(options.Until) {
		return AnalyticsOptions{}, fmt.Errorf("%w: since must not be after until",
			ErrInvalidAnalytics)
	}
	if options.Until.Sub(options.Since) > 366*24*time.Hour {
		return AnalyticsOptions{}, fmt.Errorf("%w: analytics window cannot exceed 366 days",
			ErrInvalidAnalytics)
	}
	if options.Until.After(now.Add(5 * time.Minute)) {
		return AnalyticsOptions{}, fmt.Errorf("%w: until cannot be in the future",
			ErrInvalidAnalytics)
	}
	return options, nil
}

func percentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1) * quantile)
	return sorted[index]
}
