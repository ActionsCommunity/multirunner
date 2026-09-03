package github

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/ghapp"
)

// TestDeviceTransportRefreshesExpiredToken proves an expired user token is
// refreshed against the oauth endpoint, the rotated token reaches the API as a
// bearer, and the new token is persisted to the sidecar.
func TestDeviceTransportRefreshesExpiredToken(t *testing.T) {
	var refreshes int32
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshes, 1)
		_, _ = w.Write([]byte(`{"access_token":"ghu_new","refresh_token":"ghr_new","expires_in":28800}`))
	}))
	defer refreshSrv.Close()

	var gotAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	tokenPath := filepath.Join(t.TempDir(), "token.json")
	if err := ghapp.SaveUserToken(tokenPath, &ghapp.UserToken{
		AccessToken:  "ghu_old",
		RefreshToken: "ghr_old",
		Expiry:       time.Now().Add(-time.Minute), // already expired
	}); err != nil {
		t.Fatal(err)
	}

	tr, err := newDeviceTransport(
		config.GitHub{URL: refreshSrv.URL},
		config.Auth{ClientID: "cid", TokenPath: tokenPath},
		originOf(t, apiSrv.URL),
	)
	if err != nil {
		t.Fatalf("newDeviceTransport: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, apiSrv.URL+"/x", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if gotAuth != "Bearer ghu_new" {
		t.Errorf("Authorization = %q, want the refreshed token", gotAuth)
	}
	if atomic.LoadInt32(&refreshes) != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
	saved, err := ghapp.LoadUserToken(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "ghu_new" || saved.RefreshToken != "ghr_new" {
		t.Errorf("sidecar not updated: %+v", saved)
	}
}

// TestDeviceTransportConcurrentBurstRefreshesOnce proves a burst of concurrent
// expired requests triggers a single refresh, not one per request.
func TestDeviceTransportConcurrentBurstRefreshesOnce(t *testing.T) {
	var refreshes int32
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshes, 1)
		time.Sleep(20 * time.Millisecond) // widen the race window
		_, _ = w.Write([]byte(`{"access_token":"ghu_new","refresh_token":"ghr_new","expires_in":28800}`))
	}))
	defer refreshSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	tokenPath := filepath.Join(t.TempDir(), "token.json")
	if err := ghapp.SaveUserToken(tokenPath, &ghapp.UserToken{
		AccessToken:  "ghu_old",
		RefreshToken: "ghr_old",
		Expiry:       time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	tr, err := newDeviceTransport(
		config.GitHub{URL: refreshSrv.URL},
		config.Auth{ClientID: "cid", TokenPath: tokenPath},
		originOf(t, apiSrv.URL),
	)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, apiSrv.URL+"/x", nil)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Errorf("RoundTrip: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Errorf("refreshes = %d, want exactly 1 for a concurrent burst", got)
	}
}

// TestDeviceTransportNonExpiringTokenIsNotRefreshed pins that a token with no
// expiry (an App that does not expire user tokens) is used as-is.
func TestDeviceTransportNonExpiringTokenIsNotRefreshed(t *testing.T) {
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not refresh a non-expiring token")
	}))
	defer refreshSrv.Close()

	var gotAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	tokenPath := filepath.Join(t.TempDir(), "token.json")
	if err := ghapp.SaveUserToken(tokenPath, &ghapp.UserToken{AccessToken: "ghu_forever"}); err != nil {
		t.Fatal(err)
	}
	tr, err := newDeviceTransport(config.GitHub{URL: refreshSrv.URL}, config.Auth{TokenPath: tokenPath}, originOf(t, apiSrv.URL))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, apiSrv.URL+"/x", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer ghu_forever" {
		t.Errorf("Authorization = %q, want the stored token verbatim", gotAuth)
	}
}

// TestDeviceTransportDropsAuthCrossOrigin proves the user token is not forwarded
// to a redirect target outside the configured API origin, even though the first
// hop is authenticated normally.
func TestDeviceTransportDropsAuthCrossOrigin(t *testing.T) {
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"ghu_new","refresh_token":"ghr_new","expires_in":28800}`))
	}))
	defer refreshSrv.Close()

	var apiAuth, otherAuth string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherAuth = r.Header.Get("Authorization")
	}))
	defer other.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, other.URL+"/stolen", http.StatusFound)
	}))
	defer api.Close()

	tokenPath := filepath.Join(t.TempDir(), "token.json")
	if err := ghapp.SaveUserToken(tokenPath, &ghapp.UserToken{
		AccessToken:  "ghu_old",
		RefreshToken: "ghr_old",
		Expiry:       time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	tr, err := newDeviceTransport(
		config.GitHub{URL: refreshSrv.URL},
		config.Auth{ClientID: "cid-xorigin", TokenPath: tokenPath},
		originOf(t, api.URL),
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := (&http.Client{Transport: tr}).Get(api.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if apiAuth != "Bearer ghu_new" {
		t.Errorf("first hop Authorization = %q, want the refreshed token", apiAuth)
	}
	if otherAuth != "" {
		t.Errorf("user token leaked to the redirect target: %q", otherAuth)
	}
}
