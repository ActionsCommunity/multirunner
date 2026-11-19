package vmview

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandlerServesLandingPageAndEmbeddedClient(t *testing.T) {
	h, err := handler(5701)
	if err != nil {
		t.Fatal(err)
	}

	landing := httptest.NewRecorder()
	h.ServeHTTP(landing, httptest.NewRequest(http.MethodGet, "/", nil))
	if landing.Code != http.StatusOK {
		t.Fatalf("landing status = %d", landing.Code)
	}
	for _, want := range []string{"vnc_lite.html", "location.hostname", "5701"} {
		if !strings.Contains(landing.Body.String(), want) {
			t.Errorf("landing page missing %q: %s", want, landing.Body.String())
		}
	}

	client := httptest.NewRecorder()
	h.ServeHTTP(client, httptest.NewRequest(http.MethodGet, "/novnc/vnc_lite.html", nil))
	if client.Code != http.StatusOK || !strings.Contains(client.Body.String(), "noVNC") {
		t.Fatalf("embedded client status=%d body=%q", client.Code, client.Body.String())
	}

	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing route status = %d", missing.Code)
	}
}

func TestServeReturnsBindError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = Serve(ctx, listener.Addr().String(), 5701, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("Serve error = %v, want bind failure", err)
	}
}

func TestServeStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, "127.0.0.1:0", 5701, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Serve canceled = %v", err)
	}
}
