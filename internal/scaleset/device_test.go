package scaleset

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-retryablehttp"

	"github.com/GerardSmit/multirunner/internal/ghapp"
)

// writeToken seeds a token sidecar and returns its path.
func writeToken(t *testing.T, access, refresh string, expiry time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token.json")
	if err := ghapp.SaveUserToken(path, &ghapp.UserToken{
		AccessToken:  access,
		RefreshToken: refresh,
		Expiry:       expiry,
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

// newTestClient builds the device-flow retryable client the way newDeviceClient
// does, refreshing against refreshBaseURL and matching sentinelToken.
func newTestClient(t *testing.T, tokenPath, refreshBaseURL, sentinelToken string) *retryablehttp.Client {
	t.Helper()
	refresher, err := ghapp.NewTokenRefresher("cid", refreshBaseURL, tokenPath)
	if err != nil {
		t.Fatalf("NewTokenRefresher: %v", err)
	}
	return newDeviceRetryableClient(refresher, "Bearer "+sentinelToken)
}

// doGet drives one request through the retryable client, exercising the hook.
func doGet(t *testing.T, rc *retryablehttp.Client, url, auth string) *http.Response {
	t.Helper()
	req, err := retryablehttp.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := rc.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// TestDeviceClientKeepsConcreteTransport is the regression guard for the scale
// set library's runtime type assertion: httpClientOption.newRetryableHTTPClient
// asserts HTTPClient.Transport.(*http.Transport) and otherwise fails with
// "failed to get http transport from retryablehttp client". Wrapping the
// transport to refresh tokens broke every scale-set session; the refresh runs in
// RequestLogHook precisely so this assertion keeps holding.
func TestDeviceClientKeepsConcreteTransport(t *testing.T) {
	tokenPath := writeToken(t, "ghu_current", "ghr_current", time.Now().Add(time.Hour))
	rc := newTestClient(t, tokenPath, "http://127.0.0.1:1", "ghu_current")

	if _, ok := rc.HTTPClient.Transport.(*http.Transport); !ok {
		t.Fatalf("Transport is %T, want *http.Transport", rc.HTTPClient.Transport)
	}
}

// fakeAdminJWT builds an unsigned but well-formed JWT: the library parses the
// Actions admin token with ParseUnverified to learn when to renew it.
func fakeAdminJWT(t *testing.T) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return fmt.Sprintf("%s.%s.sig",
		enc(map[string]string{"alg": "none", "typ": "JWT"}),
		enc(map[string]int64{"exp": time.Now().Add(time.Hour).Unix()}),
	)
}

// TestDeviceClientDrivesFirstScaleSetCall runs a real scaleset.Client through the
// entire first-call path — token refresh, registration token, Actions admin
// connection (which re-derives the retryable client mid-flight, the step that a
// wrapped transport breaks), then the scale set request — against an httptest
// server shaped like GHES. It also proves the refreshed user token, not the
// expired construction-time one, authenticates the registration-token call.
func TestDeviceClientDrivesFirstScaleSetCall(t *testing.T) {
	var mu sync.Mutex
	auths := map[string]string{}
	record := func(step string, r *http.Request) {
		mu.Lock()
		auths[step] = r.Header.Get("Authorization")
		mu.Unlock()
	}

	adminJWT := fakeAdminJWT(t)

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"ghu_new","refresh_token":"ghr_new","expires_in":28800}`))
	})
	mux.HandleFunc("/api/v3/orgs/testorg/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		record("registration", r)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"regtoken","expires_at":"2099-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("/api/v3/actions/runner-registration", func(w http.ResponseWriter, r *http.Request) {
		record("admin", r)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"url":   srv.URL + "/tenant",
			"token": adminJWT,
		})
	})
	mux.HandleFunc("/tenant/", func(w http.ResponseWriter, r *http.Request) {
		record("scaleset", r)
		_, _ = w.Write([]byte(`{"count":1,"value":[{"id":7,"name":"pool"}]}`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	// Already expired, so the hook must refresh before the library's first
	// authenticated call. newDeviceClient derives the refresh host from the
	// target URL, which points refreshes at the stub above rather than github.com.
	tokenPath := writeToken(t, "ghu_old", "ghr_old", time.Now().Add(-time.Minute))

	client, err := newDeviceClient(ClientOptions{
		TargetURL: srv.URL + "/testorg",
		ClientID:  "cid",
		TokenPath: tokenPath,
	})
	if err != nil {
		t.Fatalf("newDeviceClient: %v", err)
	}

	set, err := client.GetRunnerScaleSet(context.Background(), 1, "pool")
	if err != nil {
		t.Fatalf("GetRunnerScaleSet: %v", err)
	}
	if set == nil || set.ID != 7 {
		t.Fatalf("GetRunnerScaleSet = %+v, want scale set id 7", set)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := auths["registration"]; got != "Bearer ghu_new" {
		t.Errorf("registration-token Authorization = %q, want the refreshed user token", got)
	}
	if got := auths["admin"]; got != "RemoteAuth regtoken" {
		t.Errorf("admin-connection Authorization = %q, want the registration token untouched", got)
	}
	if got := auths["scaleset"]; got != "Bearer "+adminJWT {
		t.Errorf("scale set Authorization = %q, want the Actions admin token untouched", got)
	}
}

// TestDeviceClientPassesCurrentTokenUnchanged proves a request carrying our
// (non-expired) user token reaches the API as Bearer <token> without a refresh.
func TestDeviceClientPassesCurrentTokenUnchanged(t *testing.T) {
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not refresh a valid token")
	}))
	defer refreshSrv.Close()

	var gotAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer apiSrv.Close()

	tokenPath := writeToken(t, "ghu_current", "ghr_current", time.Now().Add(time.Hour))
	rc := newTestClient(t, tokenPath, refreshSrv.URL, "ghu_current")
	doGet(t, rc, apiSrv.URL+"/x", "Bearer ghu_current").Body.Close()

	if gotAuth != "Bearer ghu_current" {
		t.Errorf("Authorization = %q, want the stored token verbatim", gotAuth)
	}
}

// TestDeviceClientRefreshesOnceUnderBurst proves an expired token triggers
// exactly one refresh across a burst of concurrent requests, the rotated token
// reaches the API, and it is persisted to the sidecar.
func TestDeviceClientRefreshesOnceUnderBurst(t *testing.T) {
	var refreshes int32
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshes, 1)
		time.Sleep(20 * time.Millisecond) // widen the race window
		_, _ = w.Write([]byte(`{"access_token":"ghu_new","refresh_token":"ghr_new","expires_in":28800}`))
	}))
	defer refreshSrv.Close()

	var mu sync.Mutex
	seen := map[string]int{}
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
	}))
	defer apiSrv.Close()

	tokenPath := writeToken(t, "ghu_old", "ghr_old", time.Now().Add(-time.Minute))
	// The scale set library re-sends the construction-time token, so the sentinel
	// stays "ghu_old" even after the refresher rotates to "ghu_new".
	rc := newTestClient(t, tokenPath, refreshSrv.URL, "ghu_old")

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doGet(t, rc, apiSrv.URL+"/x", "Bearer ghu_old").Body.Close()
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Errorf("refreshes = %d, want exactly 1 for a concurrent burst", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["Bearer ghu_new"] != 16 {
		t.Errorf("API saw refreshed token %d/16 times; distribution: %v", seen["Bearer ghu_new"], seen)
	}
	saved, err := ghapp.LoadUserToken(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "ghu_new" || saved.RefreshToken != "ghr_new" {
		t.Errorf("sidecar not updated: %+v", saved)
	}
}

// TestDeviceClientLeavesRemoteAuthUntouched is the regression guard for the other
// direction: the session, long-poll and job-acquire calls carry a RemoteAuth
// admin token, which must reach the server byte-for-byte. Rewriting it would
// silently break every scale-set session.
func TestDeviceClientLeavesRemoteAuthUntouched(t *testing.T) {
	var refreshes int32
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshes, 1)
		_, _ = w.Write([]byte(`{"access_token":"ghu_new","expires_in":28800}`))
	}))
	defer refreshSrv.Close()

	var gotAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer apiSrv.Close()

	// Expired, so a rewrite path would refresh.
	tokenPath := writeToken(t, "ghu_old", "ghr_old", time.Now().Add(-time.Minute))
	rc := newTestClient(t, tokenPath, refreshSrv.URL, "ghu_old")

	const admin = "RemoteAuth adminsessiontoken.value"
	doGet(t, rc, apiSrv.URL+"/session", admin).Body.Close()

	if gotAuth != admin {
		t.Errorf("Authorization = %q, want the RemoteAuth admin token untouched", gotAuth)
	}
	if got := atomic.LoadInt32(&refreshes); got != 0 {
		t.Errorf("refreshes = %d, want 0 (RemoteAuth must not trigger a token refresh)", got)
	}
}

// TestDeviceClientRefreshFailureSendsStaleToken documents the cost of running in
// RequestLogHook: the hook cannot fail a request, so a broken refresh leaves the
// stale token in place and the failure surfaces as the API's own 401 rather than
// as an error naming `multirunner connect`.
func TestDeviceClientRefreshFailureSendsStaleToken(t *testing.T) {
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"bad_refresh_token","error_description":"expired"}`))
	}))
	defer refreshSrv.Close()

	var gotAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer apiSrv.Close()

	tokenPath := writeToken(t, "ghu_old", "ghr_old", time.Now().Add(-time.Minute))
	rc := newTestClient(t, tokenPath, refreshSrv.URL, "ghu_old")
	rc.RetryMax = 0

	resp := doGet(t, rc, apiSrv.URL+"/x", "Bearer ghu_old")
	defer resp.Body.Close()

	if gotAuth != "Bearer ghu_old" {
		t.Errorf("Authorization = %q, want the stale token when the refresh fails", gotAuth)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want the API's own 401", resp.StatusCode)
	}
}
