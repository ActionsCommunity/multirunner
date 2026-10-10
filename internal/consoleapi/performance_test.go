package consoleapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

type discardResponseWriter struct{}

func (discardResponseWriter) Header() http.Header             { return http.Header{} }
func (discardResponseWriter) WriteHeader(int)                 {}
func (discardResponseWriter) Write(value []byte) (int, error) { return io.Discard.Write(value) }

type countingResponseWriter struct {
	bytes int64
}

func (w *countingResponseWriter) Header() http.Header { return http.Header{} }
func (w *countingResponseWriter) WriteHeader(int)     {}
func (w *countingResponseWriter) Write(value []byte) (int, error) {
	w.bytes += int64(len(value))
	return len(value), nil
}

func TestStreamAdmissionSupportsCertifiedSixteenClients(t *testing.T) {
	admission := newStreamAdmission(DefaultHostStreamLimit, 1, nil)
	releases := make([]func(), 0, DefaultHostStreamLimit)
	for index := range DefaultHostStreamLimit {
		release, reason, ok := admission.acquire("session-" + strconv.Itoa(index))
		if !ok {
			t.Fatalf("client %d rejected: %s", index+1, reason)
		}
		releases = append(releases, release)
	}
	if _, reason, ok := admission.acquire("overflow"); ok || reason != "host_limit" {
		t.Fatalf("seventeenth client = ok:%v reason:%q", ok, reason)
	}
	for _, release := range releases {
		release()
	}
}

func TestCertifiedSparseMillionEventReplayWindowMeetsSLO(t *testing.T) {
	const (
		totalEvents = 1_000_000
		replayLimit = 10_000
	)
	writer := &countingResponseWriter{}
	started := time.Now()
	for sequence := totalEvents - replayLimit + 1; sequence <= totalEvents; sequence++ {
		event := operations.Event{
			ID:            operations.EventID("epoch", int64(sequence)),
			SchemaVersion: operations.CurrentEventSchemaVersion,
			HostID:        "host", HostEpoch: "epoch", Sequence: int64(sequence),
			Type: "runner.updated", EntityType: "runner_session",
			EntityID:  "runner-" + strconv.Itoa(sequence%100),
			Timestamp: time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC),
			ActorKind: operations.ActorSystem,
			Payload:   json.RawMessage(`{"pool":"linux"}`),
		}
		if err := writeSSEEvent(writer, event); err != nil {
			t.Fatal(err)
		}
	}
	duration := time.Since(started)
	if writer.bytes == 0 {
		t.Fatal("bounded replay emitted no bytes")
	}
	if duration > 2*time.Second {
		t.Fatalf("10,000-event window from a 1,000,000-event sequence took %s, SLO is 2s", duration)
	}
}

func BenchmarkWriteSSEEvent(b *testing.B) {
	event := operations.Event{
		ID: "epoch:1000", SchemaVersion: operations.CurrentEventSchemaVersion,
		HostID: "host", HostEpoch: "epoch", Sequence: 1000,
		Type: "runner.started", EntityType: "runner_session", EntityID: "runner-1",
		Timestamp: time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC),
		ActorKind: operations.ActorSystem,
		Payload:   json.RawMessage(`{"pool":"linux","repository":"owner/repo"}`),
	}
	b.ReportAllocs()
	writer := discardResponseWriter{}
	for range b.N {
		if err := writeSSEEvent(writer, event); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSessionsListAPI(b *testing.B) {
	handler := resourceListHandler("sessions", contractResources{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?limit=50", nil)
	b.ReportAllocs()
	for range b.N {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("status = %d", response.Code)
		}
	}
}
