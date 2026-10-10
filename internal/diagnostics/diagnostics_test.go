package diagnostics

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestServiceIsolatesTimeoutErrorAndPanic(t *testing.T) {
	service := NewService()
	err := service.SetChecks([]Check{
		{
			ID: "pass", Version: 1, Category: "runtime", Severity: "info",
			Run: func(context.Context) (Observation, error) {
				return Observation{Status: StatusPass, Observed: "ready", Expected: "ready"}, nil
			},
		},
		{
			ID: "error", Version: 1, Category: "runtime", Severity: "high",
			Run: func(context.Context) (Observation, error) {
				return Observation{Status: StatusFail}, errors.New("unavailable")
			},
		},
		{
			ID: "panic", Version: 1, Category: "runtime", Severity: "high",
			Run: func(context.Context) (Observation, error) {
				panic("broken")
			},
		},
		{
			ID: "timeout", Version: 1, Category: "runtime", Severity: "high",
			Timeout: time.Millisecond,
			Run: func(ctx context.Context) (Observation, error) {
				<-ctx.Done()
				return Observation{Status: StatusFail}, ctx.Err()
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context())
	if report.Status != StatusFail || len(report.Results) != 4 {
		t.Fatalf("report = %+v", report)
	}
	statuses := make(map[string]Result)
	for _, result := range report.Results {
		statuses[result.ID] = result
	}
	if statuses["pass"].Status != StatusPass ||
		statuses["error"].Error != "unavailable" ||
		statuses["panic"].Error != "broken" ||
		statuses["timeout"].Error != "diagnostic check timed out" {
		t.Fatalf("results = %+v", report.Results)
	}
}
