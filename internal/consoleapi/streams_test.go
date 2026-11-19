package consoleapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/operations"
)

type streamMetricRecorder struct {
	mu       sync.Mutex
	active   int
	rejected map[string]int
}

func (m *streamMetricRecorder) ObserveConsoleStreamOpened() {
	m.mu.Lock()
	m.active++
	m.mu.Unlock()
}

func (m *streamMetricRecorder) ObserveConsoleStreamClosed() {
	m.mu.Lock()
	m.active--
	m.mu.Unlock()
}

func (m *streamMetricRecorder) ObserveConsoleStreamRejected(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rejected == nil {
		m.rejected = map[string]int{}
	}
	m.rejected[reason]++
}

func TestStreamAdmissionLimitsReleaseAndReconnect(t *testing.T) {
	metrics := &streamMetricRecorder{}
	admission := newStreamAdmission(2, 1, metrics)
	releaseA, _, ok := admission.acquire("session-a")
	if !ok {
		t.Fatal("first session was rejected")
	}
	if _, reason, ok := admission.acquire("session-a"); ok || reason != "session_limit" {
		t.Fatalf("same-session admission = ok:%v reason:%q", ok, reason)
	}
	releaseB, _, ok := admission.acquire("session-b")
	if !ok {
		t.Fatal("second session was rejected")
	}
	if _, reason, ok := admission.acquire("session-c"); ok || reason != "host_limit" {
		t.Fatalf("host admission = ok:%v reason:%q", ok, reason)
	}
	releaseA()
	reconnected, _, ok := admission.acquire("session-a")
	if !ok {
		t.Fatal("released session could not reconnect")
	}
	reconnected()
	releaseB()
	if metrics.active != 0 || metrics.rejected["session_limit"] != 1 ||
		metrics.rejected["host_limit"] != 1 {
		t.Fatalf("metrics = active:%d rejected:%v", metrics.active, metrics.rejected)
	}
}

func TestEventStreamAdmissionReturnsTyped429(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	pairingHandler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
	})
	cookie := pairSession(t, pairingHandler, auth)
	sessionResponse := authenticatedRequest(t, pairingHandler, cookie, "/api/v1/session")
	var session consoleauth.Session
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	admission := newStreamAdmission(1, 1, nil)
	release, _, ok := admission.acquire(session.ActorID)
	if !ok {
		t.Fatal("failed to reserve stream admission")
	}
	defer release()
	handler := auth.Handler(eventStreamHandler(Options{
		HostEpoch: "epoch", Events: boundedEvents{},
	}, admission))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	addPairedSession(request, cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests ||
		response.Header().Get("Retry-After") != "1" ||
		!strings.Contains(response.Header().Get("Content-Type"), "application/json") ||
		!strings.Contains(response.Body.String(), `"code":"rate_limited"`) ||
		!strings.Contains(response.Body.String(), `"reason":"host_limit"`) {
		t.Fatalf("response = %d %v %s", response.Code, response.Header(), response.Body.String())
	}
}

func TestEventStreamLifetimeClosesAndReleasesAdmission(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryEvents{}
	journal, err := operations.NewJournal(t.Context(), store, "epoch", operations.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		HostEpoch: "epoch", Events: store, LiveEvents: journal,
		Heartbeat: time.Hour, StreamHostLimit: 1, StreamSessionLimit: 1,
		StreamMaximumLifetime: 30 * time.Millisecond,
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cookie := pairSession(t, handler, auth)
	open := func() *http.Response {
		request, err := http.NewRequestWithContext(
			context.Background(), http.MethodGet, server.URL+"/api/v1/events", nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		addPairedSession(request, cookie)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := open()
	reader := bufio.NewReader(first.Body)
	foundClose := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, "stream-close") {
			foundClose = true
		}
	}
	_ = first.Body.Close()
	if !foundClose {
		t.Fatal("bounded stream did not emit stream-close event")
	}
	second := open()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("reconnect status = %d", second.StatusCode)
	}
	_ = second.Body.Close()
}

func TestEventStreamClampsDeadlineToSessionExpiry(t *testing.T) {
	expiresAt := time.Now().Add(2 * time.Second).Truncate(time.Second)
	clockOffset := expiresAt.Add(-12 * time.Hour).Sub(time.Now())
	auth, err := consoleauth.NewWithClock(make([]byte, 32), func() time.Time {
		return time.Now().Add(clockOffset)
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryEvents{}
	journal, err := operations.NewJournal(t.Context(), store, "epoch", operations.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		HostEpoch: "epoch", Events: store, LiveEvents: journal,
		Heartbeat: time.Hour, StreamMaximumLifetime: 10 * time.Second,
	})
	paired := pairSession(t, handler, auth)
	clockOffset = 0
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	addPairedSession(request, paired)
	started := time.Now()
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	body := readStreamToEOF(t, response.Body, 4*time.Second)
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("stream outlived near-expiry session: %v", elapsed)
	}
	if strings.Contains(body, "stream_lifetime_exceeded") {
		t.Fatalf("session-expiry close emitted post-expiry event: %s", body)
	}
}

func TestEventStreamStopsOnSessionRevocation(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryEvents{}
	journal, err := operations.NewJournal(t.Context(), store, "epoch", operations.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		HostEpoch: "epoch", Events: store, LiveEvents: journal, Heartbeat: time.Hour,
	})
	paired := pairSession(t, handler, auth)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	addPairedSession(request, paired)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	readStreamUntil(t, reader, "event: replay-complete", 2*time.Second)

	auth.RevokeSessions()
	if _, err := journal.Append(t.Context(), operations.EventInput{
		Type: "runner.stopped", EntityType: "runner_session", EntityID: "revoked",
		ActorKind: operations.ActorSystem,
	}); err != nil {
		t.Fatal(err)
	}
	body := readStreamToEOF(t, reader, 2*time.Second)
	if strings.Contains(body, "event: operational-event") {
		t.Fatalf("revoked session received an event: %s", body)
	}
}

func readStreamUntil(t *testing.T, reader *bufio.Reader, marker string, timeout time.Duration) {
	t.Helper()
	found := make(chan bool, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				found <- false
				return
			}
			if strings.Contains(line, marker) {
				found <- true
				return
			}
		}
	}()
	select {
	case ok := <-found:
		if !ok {
			t.Fatalf("stream ended before %q", marker)
		}
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %q", marker)
	}
}

func readStreamToEOF(t *testing.T, reader io.Reader, timeout time.Duration) string {
	t.Helper()
	type result struct {
		body string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(reader)
		results <- result{body: string(data), err: err}
	}()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.body
	case <-time.After(timeout):
		t.Fatal("timed out waiting for stream to close")
		return ""
	}
}
