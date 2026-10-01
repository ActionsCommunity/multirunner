package github

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/ghapp"
)

type captureDeviceRequest func(*http.Request) (*http.Response, error)

func (f captureDeviceRequest) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDotComHTTPConfigRefreshesOnlyOverHTTPS(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var refreshURL string
	http.DefaultTransport = captureDeviceRequest(func(req *http.Request) (*http.Response, error) {
		body := "{}"
		if strings.HasSuffix(req.URL.Path, "/login/oauth/access_token") {
			refreshURL = req.URL.String()
			body = `{"access_token":"new","refresh_token":"new-refresh","expires_in":28800}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	path := filepath.Join(t.TempDir(), "token.json")
	if err := ghapp.SaveUserToken(path, &ghapp.UserToken{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	origin, err := apiOrigin("http://github.com")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := newDeviceTransport(config.GitHub{URL: "http://github.com"}, config.Auth{TokenPath: path}, origin)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "https://api.github.com/x", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if refreshURL != "https://github.com/login/oauth/access_token" {
		t.Fatalf("refresh sent to %s", refreshURL)
	}
}
