package transientlog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/history"
)

type fakeJobs struct {
	job history.WorkflowJob
	err error
}

func (j fakeJobs) WorkflowJob(context.Context, int64) (history.WorkflowJob, error) {
	return j.job, j.err
}

type fakeAuditor struct {
	audits []history.AccessAudit
	err    error
}

func (a *fakeAuditor) RecordAccessAudit(
	_ context.Context, audit history.AccessAudit,
) error {
	a.audits = append(a.audits, audit)
	return a.err
}

type fakeFetcher struct {
	content []byte
	err     error
}

func (f fakeFetcher) WorkflowJobLog(
	context.Context, int64, int64,
) ([]byte, github.ResponseMetadata, error) {
	return f.content, github.ResponseMetadata{}, f.err
}

func TestFetchMasksSecretsAndAuditsMetadataOnly(t *testing.T) {
	now := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
	auditor := &fakeAuditor{}
	service, err := New(Options{
		Jobs: fakeJobs{job: history.WorkflowJob{
			ID: 42, Repository: "actionscommunity/multirunner",
		}},
		Auditor: auditor,
		ClientFor: func(string) LogFetcher {
			return fakeFetcher{content: []byte(
				"configured-value\nghp_123456789012345678901234567890\n",
			)}
		},
		Secrets: []string{"configured-value"},
		Now: func() time.Time {
			value := now
			now = now.Add(25 * time.Millisecond)
			return value
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Fetch(t.Context(), "local-operator", "correlation", 42)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Content) != "***\n***\n" {
		t.Fatalf("masked log = %q", result.Content)
	}
	if len(auditor.audits) != 1 {
		t.Fatalf("audits = %+v", auditor.audits)
	}
	audit := auditor.audits[0]
	if audit.Outcome != "succeeded" || audit.ByteCount != int64(len(result.Content)) ||
		audit.TargetID != "42" || audit.ErrorCode != "" {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestFetchFailsClosedWhenAuditCannotBePersisted(t *testing.T) {
	auditor := &fakeAuditor{err: errors.New("database unavailable")}
	service, err := New(Options{
		Jobs:    fakeJobs{job: history.WorkflowJob{ID: 42, Repository: "o/r"}},
		Auditor: auditor,
		ClientFor: func(string) LogFetcher {
			return fakeFetcher{content: []byte("log bytes")}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Fetch(t.Context(), "operator", "", 42)
	if !errors.Is(err, ErrAuditUnavailable) || len(result.Content) != 0 {
		t.Fatalf("Fetch = %+v, %v", result, err)
	}
}

func TestFetchAuditsUpstreamFailureWithoutLogBytes(t *testing.T) {
	auditor := &fakeAuditor{}
	service, err := New(Options{
		Jobs:    fakeJobs{job: history.WorkflowJob{ID: 42, Repository: "o/r"}},
		Auditor: auditor,
		ClientFor: func(string) LogFetcher {
			return fakeFetcher{err: github.ErrLogTooLarge}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Fetch(t.Context(), "operator", "", 42); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Fetch error = %v", err)
	}
	if len(auditor.audits) != 1 || auditor.audits[0].ErrorCode != "too_large" ||
		auditor.audits[0].ByteCount != 0 {
		t.Fatalf("audit = %+v", auditor.audits)
	}
}
