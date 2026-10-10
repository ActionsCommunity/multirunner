// Package webhook receives GitHub App workflow_job events and dispatches them
// to queued-work and lifecycle observers. Requires GitHub to reach this endpoint
// (public IP or a tunnel such as smee.io / cloudflared / ngrok). Behind NAT,
// prefer autoscale polling.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxPayloadSize        = 1 << 20
	maxConcurrentDispatch = 32
	maxRememberedDelivery = 10_000
	deliveryReplayWindow  = 24 * time.Hour
)

// QueuedObserver receives queued workflow jobs for demand-driven scaling.
type QueuedObserver interface {
	OnQueued(WorkflowJobEvent)
}

// Observer receives every workflow_job action.
type Observer interface {
	OnWorkflowJob(context.Context, WorkflowJobEvent)
}

// WorkflowJobEvent is the normalized workflow_job webhook envelope. DeliveryID
// is GitHub's stable delivery GUID and can be used for deduplication.
type WorkflowJobEvent struct {
	DeliveryID  string
	Action      string
	Repository  string
	Sender      string
	WorkflowJob WorkflowJob
}

// WorkflowJob contains identity and lifecycle fields present across queued,
// in_progress, and completed workflow_job deliveries.
type WorkflowJob struct {
	ID              int64          `json:"id"`
	NodeID          string         `json:"node_id"`
	RunID           int64          `json:"run_id"`
	RunAttempt      int            `json:"run_attempt"`
	Name            string         `json:"name"`
	WorkflowName    string         `json:"workflow_name"`
	HeadBranch      string         `json:"head_branch"`
	HeadSHA         string         `json:"head_sha"`
	URL             string         `json:"url"`
	HTMLURL         string         `json:"html_url"`
	RunURL          string         `json:"run_url"`
	CheckRunURL     string         `json:"check_run_url"`
	Status          string         `json:"status"`
	Conclusion      string         `json:"conclusion"`
	Labels          []string       `json:"labels"`
	RunnerID        int64          `json:"runner_id"`
	RunnerName      string         `json:"runner_name"`
	RunnerGroupID   int64          `json:"runner_group_id"`
	RunnerGroupName string         `json:"runner_group_name"`
	CreatedAt       *time.Time     `json:"created_at"`
	StartedAt       *time.Time     `json:"started_at"`
	CompletedAt     *time.Time     `json:"completed_at"`
	Steps           []WorkflowStep `json:"steps"`
}

// WorkflowStep is one step reported with an in-progress or completed job.
type WorkflowStep struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	Number      int64      `json:"number"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// Server is the webhook HTTP receiver.
type Server struct {
	secret    string
	queued    QueuedObserver
	srv       *http.Server
	logger    *slog.Logger
	observers []Observer
	mu        sync.RWMutex

	dispatch chan struct{}

	deliveryMu sync.Mutex
	deliveries map[string]time.Time
	now        func() time.Time
}

// New builds the webhook server.
func New(listen, path, secret string, queued QueuedObserver, logger *slog.Logger) *Server {
	s := &Server{
		secret: secret, queued: queued, logger: logger.With("component", "webhook"),
		dispatch:   make(chan struct{}, maxConcurrentDispatch),
		deliveries: make(map[string]time.Time),
		now:        time.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, s.handle)
	s.srv = &http.Server{Addr: listen, Handler: mux}
	return s
}

// AddObserver registers a receiver for all workflow_job actions.
func (s *Server) AddObserver(observer Observer) {
	if observer == nil {
		return
	}
	s.mu.Lock()
	s.observers = append(s.observers, observer)
	s.mu.Unlock()
}

// Start runs the server until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	if strings.TrimSpace(s.secret) == "" {
		return errors.New("webhook secret is required")
	}
	go func() {
		s.logger.Info("webhook listening", "addr", s.srv.Addr)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("webhook server stopped", "err", err)
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(shutCtx)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadSize+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > maxPayloadSize {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if strings.TrimSpace(s.secret) == "" {
		s.logger.Error("rejected webhook because no signing secret is configured")
		http.Error(w, "webhook unavailable", http.StatusServiceUnavailable)
		return
	}
	if !validSignature(s.secret, r.Header.Get("X-Hub-Signature-256"), body) {
		s.logger.Warn("rejected webhook with bad signature")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		w.WriteHeader(http.StatusOK)
		return
	case "workflow_job":
		// handled below
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var payload struct {
		Action      string      `json:"action"`
		WorkflowJob WorkflowJob `json:"workflow_job"`
		Repository  struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	event := WorkflowJobEvent{
		DeliveryID:  r.Header.Get("X-GitHub-Delivery"),
		Action:      payload.Action,
		Repository:  payload.Repository.FullName,
		Sender:      payload.Sender.Login,
		WorkflowJob: payload.WorkflowJob,
	}
	event.DeliveryID = strings.TrimSpace(event.DeliveryID)
	if event.DeliveryID == "" || len(event.DeliveryID) > 128 {
		http.Error(w, "invalid delivery ID", http.StatusBadRequest)
		return
	}
	select {
	case s.dispatch <- struct{}{}:
		if !s.reserveDelivery(event.DeliveryID) {
			<-s.dispatch
			w.WriteHeader(http.StatusOK)
			return
		}
		defer func() { <-s.dispatch }()
		s.dispatchEvent(r.Context(), event)
	default:
		if s.hasDelivery(event.DeliveryID) {
			w.WriteHeader(http.StatusOK)
			return
		}
		s.logger.Warn("webhook observer dispatch capacity exhausted")
		http.Error(w, "webhook dispatch unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) dispatchEvent(ctx context.Context, event WorkflowJobEvent) {
	if event.Action == "queued" && s.queued != nil {
		s.logger.Info("workflow_job queued",
			"repo", event.Repository, "run_id", event.WorkflowJob.RunID,
			"labels", event.WorkflowJob.Labels)
		callQueuedObserver(s.logger, s.queued, event)
	}
	s.mu.RLock()
	observers := append([]Observer(nil), s.observers...)
	s.mu.RUnlock()
	for _, observer := range observers {
		callObserver(ctx, s.logger, observer, event)
	}
}

func callQueuedObserver(logger *slog.Logger, observer QueuedObserver, event WorkflowJobEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("queued webhook observer panicked", "panic", recovered)
		}
	}()
	observer.OnQueued(event)
}

func callObserver(
	ctx context.Context, logger *slog.Logger,
	observer Observer, event WorkflowJobEvent,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("webhook observer panicked", "panic", recovered)
		}
	}()
	observer.OnWorkflowJob(ctx, event)
}

func (s *Server) hasDelivery(id string) bool {
	now := s.now()
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	expires, ok := s.deliveries[id]
	if ok && !now.Before(expires) {
		delete(s.deliveries, id)
		return false
	}
	return ok
}

func (s *Server) reserveDelivery(id string) bool {
	now := s.now()
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if expires, ok := s.deliveries[id]; ok && now.Before(expires) {
		return false
	}
	delete(s.deliveries, id)
	if len(s.deliveries) >= maxRememberedDelivery {
		var oldestID string
		var oldest time.Time
		for candidate, expires := range s.deliveries {
			if !now.Before(expires) {
				delete(s.deliveries, candidate)
				continue
			}
			if oldestID == "" || expires.Before(oldest) {
				oldestID, oldest = candidate, expires
			}
		}
		if len(s.deliveries) >= maxRememberedDelivery && oldestID != "" {
			delete(s.deliveries, oldestID)
		}
	}
	s.deliveries[id] = now.Add(deliveryReplayWindow)
	return true
}

// validSignature verifies the HMAC-SHA256 signature GitHub sends.
func validSignature(secret, header string, body []byte) bool {
	const prefix = "sha256="
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	got, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	return hmac.Equal(expected, got)
}
