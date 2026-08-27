// Package webhook receives GitHub App workflow_job events and triggers the
// autoscaler. Requires GitHub to reach this endpoint (public IP or a tunnel such
// as smee.io / cloudflared / ngrok). Behind NAT, prefer autoscale polling.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/GerardSmit/multirunner/internal/autoscale"
)

// Server is the webhook HTTP receiver.
type Server struct {
	secret string
	scaler *autoscale.Scaler
	srv    *http.Server
	logger *slog.Logger
}

// New builds the webhook server.
func New(listen, path, secret string, scaler *autoscale.Scaler, logger *slog.Logger) *Server {
	s := &Server{secret: secret, scaler: scaler, logger: logger.With("component", "webhook")}
	mux := http.NewServeMux()
	mux.HandleFunc(path, s.handle)
	s.srv = &http.Server{Addr: listen, Handler: mux}
	return s
}

// Start runs the server until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if s.secret != "" && !validSignature(s.secret, r.Header.Get("X-Hub-Signature-256"), body) {
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
		Action      string `json:"action"`
		WorkflowJob struct {
			Labels []string `json:"labels"`
		} `json:"workflow_job"`
		// Repository identifies where the job is queued. A repo-scoped runner
		// binds to one repo, so the scaler needs this to register the runner
		// somewhere it can actually pick the job up.
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if payload.Action == "queued" {
		s.logger.Info("workflow_job queued",
			"repo", payload.Repository.FullName, "labels", payload.WorkflowJob.Labels)
		s.scaler.OnQueued(payload.Repository.FullName, payload.WorkflowJob.Labels)
	}
	w.WriteHeader(http.StatusOK)
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
