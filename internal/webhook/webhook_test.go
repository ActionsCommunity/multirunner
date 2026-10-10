package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingQueued struct {
	mu     sync.Mutex
	events []WorkflowJobEvent
}

func (r *recordingQueued) OnQueued(event WorkflowJobEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

type recordingObserver struct {
	mu     sync.Mutex
	events []WorkflowJobEvent
}

func (r *recordingObserver) OnWorkflowJob(
	_ context.Context, event WorkflowJobEvent,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingQueued) snapshot() []WorkflowJobEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WorkflowJobEvent(nil), r.events...)
}

func (r *recordingObserver) snapshot() []WorkflowJobEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WorkflowJobEvent(nil), r.events...)
}

func testServerWith(secret string, queued QueuedObserver) *Server {
	return New("127.0.0.1:0", "/webhook", secret, queued, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testServer(secret string) *Server {
	return testServerWith(secret, &recordingQueued{})
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	if !validSignature("s3cret", sign("s3cret", body), body) {
		t.Error("valid signature rejected")
	}
	if validSignature("s3cret", sign("wrong", body), body) {
		t.Error("bad signature accepted")
	}
	if validSignature("s3cret", "garbage", body) {
		t.Error("malformed header accepted")
	}
}

func do(t *testing.T, s *Server, event, sig string, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	s.handle(rec, req)
	return rec.Code
}

func doWithDelivery(t *testing.T, s *Server, event, delivery, sig, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	s.handle(rec, req)
	return rec.Code
}

func TestHandlePing(t *testing.T) {
	secret := "s3cret"
	s := testServer(secret)
	body := "{}"
	if code := do(t, s, "ping", sign(secret, []byte(body)), body); code != http.StatusOK {
		t.Errorf("ping = %d", code)
	}
}

func TestHandleWorkflowJobQueued(t *testing.T) {
	secret := "s3cret"
	s := testServer(secret)
	body := `{"action":"queued","workflow_job":{"labels":["self-hosted","linux","x64"]}}`
	if code := doWithDelivery(t, s, "workflow_job", "delivery-queued", sign(secret, []byte(body)), body); code != http.StatusOK {
		t.Errorf("queued = %d", code)
	}
}

// TestHandleWorkflowJobQueuedRoutesRepo proves the repo that queued the job
// reaches the scaler. A repo-scoped runner binds to exactly one repo, so losing
// this value means the runner can be registered somewhere with no work.
func TestHandleWorkflowJobQueuedRoutesRepo(t *testing.T) {
	secret := "s3cret"
	queued := &recordingQueued{}
	s := testServerWith(secret, queued)
	body := `{"action":"queued","repository":{"full_name":"o/repoB"},"workflow_job":{"labels":["self-hosted","windows"]}}`
	if code := doWithDelivery(t, s, "workflow_job", "delivery-repo", sign(secret, []byte(body)), body); code != http.StatusOK {
		t.Fatalf("queued = %d", code)
	}
	events := waitQueuedEvents(t, queued, 1)
	if events[0].Repository != "o/repoB" {
		t.Errorf("queued repository = %q, want o/repoB", events[0].Repository)
	}
}

func TestHandleWorkflowJobObserversReceiveAllActionsAndIdentity(t *testing.T) {
	secret := "s3cret"
	queued := &recordingQueued{}
	observer := &recordingObserver{}
	s := testServerWith(secret, queued)
	s.AddObserver(observer)

	for _, action := range []string{"queued", "in_progress", "completed"} {
		body := `{
			"action":"` + action + `",
			"repository":{"full_name":"octo/hello"},
			"sender":{"login":"hubot"},
			"workflow_job":{
				"id":501,"run_id":101,"run_attempt":2,"name":"compile",
				"workflow_name":"Build","head_branch":"main","head_sha":"abc123",
				"html_url":"https://github.example/jobs/501","status":"` + action + `",
				"conclusion":"success","labels":["self-hosted","linux"],
				"runner_id":7,"runner_name":"runner-7","runner_group_id":8,
				"runner_group_name":"Default","created_at":"2026-10-07T20:00:00Z",
				"started_at":"2026-10-07T20:01:00Z","completed_at":"2026-10-07T20:02:00Z",
				"steps":[{
					"name":"checkout","status":"completed","conclusion":"success","number":1,
					"started_at":"2026-10-07T20:01:00Z","completed_at":"2026-10-07T20:01:30Z"
				}]
			}
		}`
		if code := doWithDelivery(
			t, s, "workflow_job", "delivery-"+action, sign(secret, []byte(body)), body,
		); code != http.StatusOK {
			t.Fatalf("%s = %d", action, code)
		}
	}

	waitQueuedEvents(t, queued, 1)
	events := waitObserverEvents(t, observer, 3)
	event := events[2]
	if event.DeliveryID != "delivery-completed" || event.Action != "completed" ||
		event.Repository != "octo/hello" || event.Sender != "hubot" {
		t.Fatalf("event envelope = %#v", event)
	}
	job := event.WorkflowJob
	if job.ID != 501 || job.RunID != 101 || job.RunAttempt != 2 ||
		job.Name != "compile" || job.WorkflowName != "Build" ||
		job.HeadBranch != "main" || job.HeadSHA != "abc123" ||
		job.RunnerID != 7 || job.RunnerGroupID != 8 ||
		job.CreatedAt == nil || job.StartedAt == nil || job.CompletedAt == nil ||
		len(job.Steps) != 1 || job.Steps[0].Name != "checkout" ||
		job.Steps[0].StartedAt == nil || job.Steps[0].CompletedAt == nil {
		t.Fatalf("workflow job = %#v", job)
	}
}

func TestHandleBadSignature(t *testing.T) {
	s := testServer("s3cret")
	body := `{"action":"queued","workflow_job":{"labels":[]}}`
	if code := do(t, s, "workflow_job", "sha256=deadbeef", body); code != http.StatusUnauthorized {
		t.Errorf("bad sig = %d, want 401", code)
	}
}

func TestHandleRejectsOversizedPayload(t *testing.T) {
	s := testServer("s3cret")
	body := strings.Repeat(" ", maxPayloadSize+1)
	if code := do(t, s, "workflow_job", "", body); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized payload = %d, want 413", code)
	}
}

func TestHandleUnknownEvent(t *testing.T) {
	secret := "s3cret"
	s := testServer(secret)
	body := "{}"
	if code := do(t, s, "push", sign(secret, []byte(body)), body); code != http.StatusNoContent {
		t.Errorf("unknown event = %d, want 204", code)
	}
}

func TestStartAndHandlerFailClosedWithoutSecret(t *testing.T) {
	s := testServer("")
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start accepted an empty webhook secret")
	}
	if code := do(t, s, "ping", "", "{}"); code != http.StatusServiceUnavailable {
		t.Fatalf("unsigned request without configured secret = %d, want 503", code)
	}
}

func TestHandleRejectsUnsignedRequest(t *testing.T) {
	s := testServer("s3cret")
	if code := do(t, s, "ping", "", "{}"); code != http.StatusUnauthorized {
		t.Fatalf("unsigned ping = %d, want 401", code)
	}
}

func TestHandleSuppressesReplayedDelivery(t *testing.T) {
	secret := "s3cret"
	observer := &recordingObserver{}
	s := testServer(secret)
	s.AddObserver(observer)
	body := `{"action":"completed","workflow_job":{"id":42}}`
	signature := sign(secret, []byte(body))
	for range 2 {
		if code := doWithDelivery(
			t, s, "workflow_job", "delivery-replay", signature, body,
		); code != http.StatusOK {
			t.Fatalf("delivery status = %d", code)
		}
	}
	events := waitObserverEvents(t, observer, 1)
	time.Sleep(20 * time.Millisecond)
	if got := len(observer.snapshot()); got != len(events) {
		t.Fatalf("observer received replay: %d events", got)
	}
}

type blockingObserver struct {
	started chan struct{}
	release chan struct{}
}

func (o *blockingObserver) OnWorkflowJob(context.Context, WorkflowJobEvent) {
	select {
	case o.started <- struct{}{}:
	default:
	}
	<-o.release
}

func TestHandleBoundsObserverDispatch(t *testing.T) {
	secret := "s3cret"
	blocker := &blockingObserver{started: make(chan struct{}, 1), release: make(chan struct{})}
	s := testServer(secret)
	s.dispatch = make(chan struct{}, 1)
	s.AddObserver(blocker)
	body := `{"action":"completed","workflow_job":{"id":42}}`
	signature := sign(secret, []byte(body))
	first := make(chan int, 1)
	go func() {
		first <- doWithDelivery(
			t, s, "workflow_job", "delivery-first", signature, body,
		)
	}()
	select {
	case <-blocker.started:
	case <-time.After(time.Second):
		t.Fatal("observer did not start")
	}
	if code := doWithDelivery(
		t, s, "workflow_job", "delivery-second", signature, body,
	); code != http.StatusServiceUnavailable {
		t.Fatalf("second delivery = %d, want 503", code)
	}
	close(blocker.release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first delivery = %d", code)
	}
}

func waitQueuedEvents(t *testing.T, recorder *recordingQueued, count int) []WorkflowJobEvent {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		events := recorder.snapshot()
		if len(events) >= count {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued observer received %d events, want %d", len(events), count)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitObserverEvents(t *testing.T, recorder *recordingObserver, count int) []WorkflowJobEvent {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		events := recorder.snapshot()
		if len(events) >= count {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("observer received %d events, want %d", len(events), count)
		}
		time.Sleep(time.Millisecond)
	}
}
