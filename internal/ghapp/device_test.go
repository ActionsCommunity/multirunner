package ghapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// noPollDelay removes the between-poll sleep for the duration of a test.
func noPollDelay(t *testing.T) {
	t.Helper()
	orig := pollWait
	pollWait = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	t.Cleanup(func() { pollWait = orig })
}

func TestRequestDeviceCode(t *testing.T) {
	var gotClientID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/device/code" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		_ = r.ParseForm()
		gotClientID = r.Form.Get("client_id")
		_, _ = w.Write([]byte(`{"device_code":"dev123","user_code":"6AC9-ABAD","verification_uri":"https://github.com/login/device","expires_in":899,"interval":5}`))
	}))
	defer srv.Close()

	dc, err := RequestDeviceCode(context.Background(), "", srv.URL)
	if err != nil {
		t.Fatalf("RequestDeviceCode: %v", err)
	}
	if gotClientID != DefaultClientID {
		t.Errorf("client_id = %q, want default %q", gotClientID, DefaultClientID)
	}
	if dc.DeviceCode != "dev123" || dc.UserCode != "6AC9-ABAD" || dc.Interval != 5 || dc.ExpiresIn != 899 {
		t.Errorf("device code = %+v", dc)
	}
}

func TestRequestDeviceCodeDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"device_flow_disabled"}`))
	}))
	defer srv.Close()

	_, err := RequestDeviceCode(context.Background(), "cid", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "device_flow_disabled") {
		t.Fatalf("error = %v, want device_flow_disabled surfaced", err)
	}
	if !strings.Contains(err.Error(), "--own-app") {
		t.Errorf("error should suggest --own-app fallback: %v", err)
	}
}

func TestPollDeviceTokenPendingThenSlowDownThenSuccess(t *testing.T) {
	noPollDelay(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/oauth/access_token" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = r.ParseForm()
		if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("grant_type = %q", got)
		}
		calls++
		switch calls {
		case 1:
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		case 2:
			_, _ = w.Write([]byte(`{"error":"slow_down","interval":10}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"ghu_abc","refresh_token":"ghr_def","expires_in":28800,"refresh_token_expires_in":15638400,"token_type":"bearer","scope":""}`))
		}
	}))
	defer srv.Close()

	dc := &DeviceCode{DeviceCode: "dev123", Interval: 1, ExpiresIn: 899}
	start := time.Now()
	tok, err := PollDeviceToken(context.Background(), "cid", srv.URL, dc)
	if err != nil {
		t.Fatalf("PollDeviceToken: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (pending, slow_down, success)", calls)
	}
	if tok.AccessToken != "ghu_abc" || tok.RefreshToken != "ghr_def" {
		t.Errorf("token = %+v", tok)
	}
	if tok.Expiry.IsZero() || !tok.Expiry.After(start) {
		t.Errorf("expiry not set from expires_in: %v", tok.Expiry)
	}
	if tok.RefreshExpiry.IsZero() {
		t.Errorf("refresh expiry not set from refresh_token_expires_in")
	}
}

func TestPollDeviceTokenExpired(t *testing.T) {
	noPollDelay(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"expired_token"}`))
	}))
	defer srv.Close()

	_, err := PollDeviceToken(context.Background(), "cid", srv.URL, &DeviceCode{DeviceCode: "d", Interval: 1, ExpiresIn: 899})
	if err == nil || !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("error = %v, want an actionable expired message", err)
	}
}

func TestPollDeviceTokenDenied(t *testing.T) {
	noPollDelay(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}))
	defer srv.Close()

	_, err := PollDeviceToken(context.Background(), "cid", srv.URL, &DeviceCode{DeviceCode: "d", Interval: 1, ExpiresIn: 899})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error = %v, want denied", err)
	}
}

func TestPollDeviceTokenHonorsContextCancel(t *testing.T) {
	// A never-resolving wait plus a cancelled context must return promptly.
	orig := pollWait
	pollWait = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	t.Cleanup(func() { pollWait = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PollDeviceToken(ctx, "cid", srv.URL, &DeviceCode{DeviceCode: "d", Interval: 1, ExpiresIn: 899})
	if err == nil {
		t.Fatal("want context error, got nil")
	}
}

func TestRefreshUserToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "ghr_old" {
			t.Errorf("refresh_token = %q", got)
		}
		// Device flow uses no client secret.
		if r.Form.Has("client_secret") {
			t.Errorf("refresh must not send a client secret")
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_new","refresh_token":"ghr_new","expires_in":28800,"refresh_token_expires_in":15638400}`))
	}))
	defer srv.Close()

	tok, err := RefreshUserToken(context.Background(), "cid", srv.URL, "ghr_old")
	if err != nil {
		t.Fatalf("RefreshUserToken: %v", err)
	}
	if tok.AccessToken != "ghu_new" || tok.RefreshToken != "ghr_new" {
		t.Errorf("token = %+v", tok)
	}
}

func TestRefreshUserTokenErrorIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"bad_refresh_token","error_description":"The refresh token is incorrect or expired."}`))
	}))
	defer srv.Close()

	_, err := RefreshUserToken(context.Background(), "cid", srv.URL, "ghr_old")
	if err == nil || !strings.Contains(err.Error(), "multirunner connect") {
		t.Fatalf("error = %v, want a re-run instruction", err)
	}
}

func TestUserInstallationsAndMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/installations" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ghu_abc" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"total_count":1,"installations":[{"id":4242,"account":{"login":"my-org"},"app_slug":"multirunner-connect"}]}`))
	}))
	defer srv.Close()

	installs, err := UserInstallations(context.Background(), srv.URL, "ghu_abc")
	if err != nil {
		t.Fatalf("UserInstallations: %v", err)
	}
	inst, slug, ok := MatchInstallation(installs, "MY-ORG")
	if !ok || inst.ID != 4242 || slug != "multirunner-connect" {
		t.Errorf("match = %+v slug=%q ok=%v", inst, slug, ok)
	}

	_, slug, ok = MatchInstallation(installs, "other")
	if ok {
		t.Error("must not match a different org")
	}
	if slug != "multirunner-connect" {
		t.Errorf("unmatched slug = %q, want the App's slug for the install URL", slug)
	}
}

func TestMatchInstallationEmptyFallsBackToDefaultSlug(t *testing.T) {
	_, slug, ok := MatchInstallation(nil, "my-org")
	if ok {
		t.Error("no installations must not match")
	}
	if slug != DefaultAppSlug {
		t.Errorf("slug = %q, want DefaultAppSlug %q", slug, DefaultAppSlug)
	}
}

// TestPostFormEncodesBody guards that the device code travels in the form body,
// not the URL, so it never lands in a request line or server log.
func TestPostFormEncodesBody(t *testing.T) {
	var gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var out map[string]any
	if err := postForm(context.Background(), srv.URL, url.Values{"device_code": {"secret"}}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotRawQuery, "secret") {
		t.Errorf("device code leaked into the URL query: %q", gotRawQuery)
	}
}
