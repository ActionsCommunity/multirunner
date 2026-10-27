package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type staticResolver []net.IP

func (r staticResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	result := make([]net.IPAddr, 0, len(r))
	for _, ip := range r {
		result = append(result, net.IPAddr{IP: ip})
	}
	return result, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNotificationEndpointValidation(t *testing.T) {
	if _, err := NewAiTextAdapter("https://example.com/alerts", nil); err == nil {
		t.Fatal("external AiText endpoint was accepted")
	}
	if _, err := NewWebhookAdapter("http://example.com/alerts", "secret", nil); err == nil {
		t.Fatal("HTTP webhook endpoint was accepted")
	}
	if _, err := NewWebhookAdapter("https://user@example.com/alerts", "secret", nil); err == nil {
		t.Fatal("webhook user info was accepted")
	}
	if _, err := NewWebhookAdapter("https://[fe80::1%25eth0]/alerts", "secret", nil); err == nil {
		t.Fatal("zoned webhook address was accepted")
	}
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "169.254.169.254", "0.0.0.0",
		"100.64.0.1", "192.0.2.1", "198.18.0.1", "203.0.113.1",
		"240.0.0.1", "::1", "fc00::1", "fe80::1", "ff02::1", "2001:db8::1",
	}
	for _, address := range blocked {
		if err := validateResolvedAddress(net.ParseIP(address), false); err == nil {
			t.Errorf("webhook address %s was accepted", address)
		}
	}
	if err := validateResolvedAddress(net.ParseIP("8.8.8.8"), false); err != nil {
		t.Fatalf("public webhook address rejected: %v", err)
	}
}

func TestWebhookDNSRevalidationBlocksMixedAnswers(t *testing.T) {
	endpoint, err := url.Parse("https://hooks.example.test/alerts")
	if err != nil {
		t.Fatal(err)
	}
	client, err := restrictedHTTPClient(endpoint, staticResolver{
		net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1"),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	_, err = transport.DialContext(t.Context(), "tcp", "hooks.example.test:443")
	if err == nil || !strings.Contains(err.Error(), "blocked address") {
		t.Fatalf("mixed DNS answer error = %v", err)
	}
}

func TestAiTextDeliveryUsesConfiguredLoopbackEndpoint(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)
		body = string(data)
		if request.Header.Get("X-Multirunner-Delivery") != "delivery-1" {
			t.Errorf("delivery header = %q", request.Header.Get("X-Multirunner-Delivery"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	adapter, err := NewAiTextAdapter(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.Deliver(t.Context(), Delivery{
		ID: "delivery-1", AlertID: "alert-1",
		Payload: []byte(`{"summary":"runner failed"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"summary":"runner failed"}` {
		t.Fatalf("AiText body = %q", body)
	}
}

func TestWebhookDeliverySignsPayloadAndRejectsRedirect(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{"summary":"runner failed"}`)
	adapter := &httpDeliveryAdapter{
		endpoint: &url.URL{Scheme: "https", Host: "hooks.example.test", Path: "/alerts"},
		secret:   "secret", webhook: true, now: func() time.Time { return now },
		client: &http.Client{
			Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				timestamp := request.Header.Get("X-Multirunner-Timestamp")
				mac := hmac.New(sha256.New, []byte("secret"))
				_, _ = mac.Write([]byte(timestamp + "."))
				_, _ = mac.Write(payload)
				want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
				if got := request.Header.Get("X-Multirunner-Signature"); got != want {
					t.Errorf("signature = %q, want %q", got, want)
				}
				return &http.Response{
					StatusCode: http.StatusNoContent, Status: "204 No Content",
					Body:   io.NopCloser(strings.NewReader("")),
					Header: make(http.Header), Request: request,
				}, nil
			}),
		},
	}
	if err := adapter.Deliver(t.Context(), Delivery{
		ID: "delivery-1", AlertID: "alert-1", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	adapter.client = &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusFound, Status: "302 Found",
				Header: http.Header{"Location": []string{"https://other.example/"}},
				Body:   io.NopCloser(strings.NewReader("")), Request: request,
				TLS: &tls.ConnectionState{},
			}, nil
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	err := adapter.Deliver(t.Context(), Delivery{
		ID: "delivery-2", AlertID: "alert-1", Payload: payload,
	})
	var permanent *PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("redirect error = %v, want permanent", err)
	}
}
