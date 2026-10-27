// Package transientlog fetches bounded GitHub job logs without persistence.
package transientlog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/secretmask"
)

var (
	ErrUnavailable      = errors.New("workflow job log is unavailable")
	ErrAuditUnavailable = errors.New("workflow job log audit is unavailable")
)

type JobResolver interface {
	WorkflowJob(context.Context, int64) (history.WorkflowJob, error)
}

type Auditor interface {
	RecordAccessAudit(context.Context, history.AccessAudit) error
}

type LogFetcher interface {
	WorkflowJobLog(context.Context, int64, int64) ([]byte, github.ResponseMetadata, error)
}

type Options struct {
	Jobs      JobResolver
	Auditor   Auditor
	ClientFor func(string) LogFetcher
	Secrets   []string
	MaxBytes  int64
	Timeout   time.Duration
	Now       func() time.Time
}

type Result struct {
	JobID      int64
	Repository string
	Content    []byte
	Duration   time.Duration
}

type Service struct {
	jobs      JobResolver
	auditor   Auditor
	clientFor func(string) LogFetcher
	secrets   []string
	maxBytes  int64
	timeout   time.Duration
	now       func() time.Time
}

func New(options Options) (*Service, error) {
	if options.Jobs == nil || options.Auditor == nil || options.ClientFor == nil {
		return nil, errors.New("transient log dependencies are required")
	}
	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	if maxBytes > 16<<20 {
		maxBytes = 16 << 20
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		jobs: options.Jobs, auditor: options.Auditor, clientFor: options.ClientFor,
		secrets:  append([]string(nil), options.Secrets...),
		maxBytes: maxBytes, timeout: timeout, now: now,
	}, nil
}

func (s *Service) Fetch(
	ctx context.Context, actorID, correlationID string, jobID int64,
) (Result, error) {
	if actorID == "" || jobID <= 0 {
		return Result{}, errors.New("operator actor and workflow job ID are required")
	}
	started := s.now()
	job, err := s.jobs.WorkflowJob(ctx, jobID)
	if err != nil {
		return Result{}, s.fail(ctx, started, actorID, correlationID, jobID,
			"not_found", err)
	}
	client := s.clientFor(job.Repository)
	if client == nil {
		return Result{}, s.fail(ctx, started, actorID, correlationID, jobID,
			"client_unavailable", ErrUnavailable)
	}
	fetchContext, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	content, _, err := client.WorkflowJobLog(fetchContext, jobID, s.maxBytes)
	if err != nil {
		code := "upstream_error"
		if errors.Is(err, github.ErrLogTooLarge) {
			code = "too_large"
		} else if errors.Is(fetchContext.Err(), context.DeadlineExceeded) {
			code = "timeout"
		}
		return Result{}, s.fail(ctx, started, actorID, correlationID, jobID, code, err)
	}
	masked := []byte(secretmask.Text(string(content), s.secrets))
	duration := s.now().Sub(started)
	auditContext, auditCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer auditCancel()
	if err := s.auditor.RecordAccessAudit(auditContext, history.AccessAudit{
		OccurredAt: started, ActorKind: "operator", ActorID: actorID,
		Action: "job.log.read", TargetType: "workflow_job",
		TargetID: strconv.FormatInt(jobID, 10), CorrelationID: correlationID,
		Outcome: "succeeded", ByteCount: int64(len(masked)), Duration: duration,
	}); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrAuditUnavailable, err)
	}
	return Result{
		JobID: jobID, Repository: job.Repository,
		Content: masked, Duration: duration,
	}, nil
}

func (s *Service) fail(
	ctx context.Context, started time.Time, actorID, correlationID string,
	jobID int64, code string, cause error,
) error {
	auditContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	auditErr := s.auditor.RecordAccessAudit(auditContext, history.AccessAudit{
		OccurredAt: started, ActorKind: "operator", ActorID: actorID,
		Action: "job.log.read", TargetType: "workflow_job",
		TargetID: strconv.FormatInt(jobID, 10), CorrelationID: correlationID,
		Outcome: "failed", Duration: s.now().Sub(started), ErrorCode: code,
	})
	if auditErr != nil {
		return fmt.Errorf("%w: %v", ErrAuditUnavailable, auditErr)
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, cause)
}
