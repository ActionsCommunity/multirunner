package ghapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seedToken writes a sidecar in a fresh directory and returns its path.
func seedToken(t *testing.T, tok *UserToken) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token.json")
	if err := SaveUserToken(path, tok); err != nil {
		t.Fatal(err)
	}
	return path
}

// rotatingServer serves the refresh endpoint, handing out a distinct token pair
// per call so a double refresh is visible, and counts the calls.
func rotatingServer(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(calls, 1)
		time.Sleep(20 * time.Millisecond) // widen the race window
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fmtToken(n))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fmtToken(n int32) []byte {
	return []byte(`{"access_token":"ghu_new` + string(rune('0'+n)) +
		`","refresh_token":"ghr_new` + string(rune('0'+n)) + `","expires_in":28800}`)
}

// TestRefresherIsSharedPerTokenPath proves two callers naming the same sidecar
// get the same refresher, so pools contend on one mutex instead of rotating a
// single credential in parallel.
func TestRefresherIsSharedPerTokenPath(t *testing.T) {
	path := seedToken(t, &UserToken{AccessToken: "ghu", RefreshToken: "ghr", Expiry: time.Now().Add(time.Hour)})

	a, err := NewTokenRefresher("cid", "https://example.test", path)
	if err != nil {
		t.Fatal(err)
	}
	// The same file reached by a relative path must still resolve to one instance.
	b, err := NewTokenRefresher("cid", "https://example.test/", filepath.Join(filepath.Dir(path), ".", "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("refreshers for the same token path are not shared")
	}
}

// TestRefresherRefreshesOnceAcrossPools proves a burst from many pools sharing a
// credential rotates it exactly once.
func TestRefresherRefreshesOnceAcrossPools(t *testing.T) {
	var calls int32
	srv := rotatingServer(t, &calls)
	path := seedToken(t, &UserToken{AccessToken: "ghu_old", RefreshToken: "ghr_old", Expiry: time.Now().Add(-time.Minute)})

	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := NewTokenRefresher("cid", srv.URL, path)
			if err != nil {
				t.Error(err)
				return
			}
			tok, err := r.AccessToken(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = tok
		}(i)
	}
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("refresh calls = %d, want 1 (a rotation invalidates the previous pair)", n)
	}
	for i, tok := range got {
		if tok != got[0] {
			t.Errorf("pool %d got %q, want the same token as pool 0 (%q)", i, tok, got[0])
		}
	}
}

// TestRefresherLockSerialisesSeparateProcesses stands in for two multirunner
// processes sharing a sidecar: distinct refreshers (different client ids, so the
// per-path cache does not merge them) must serialise on the lock file, and the
// loser must adopt the token the winner persisted rather than rotating again
// with a refresh token GitHub has already invalidated.
func TestRefresherLockSerialisesSeparateProcesses(t *testing.T) {
	var calls int32
	srv := rotatingServer(t, &calls)
	path := seedToken(t, &UserToken{AccessToken: "ghu_old", RefreshToken: "ghr_old", Expiry: time.Now().Add(-time.Minute)})

	first, err := NewTokenRefresher("cid-a", srv.URL, path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewTokenRefresher("cid-b", srv.URL, path)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("distinct client ids must not share a refresher")
	}

	var wg sync.WaitGroup
	tokens := make([]string, 2)
	for i, r := range []*TokenRefresher{first, second} {
		wg.Add(1)
		go func(i int, r *TokenRefresher) {
			defer wg.Done()
			tok, err := r.AccessToken(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			tokens[i] = tok
		}(i, r)
	}
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("refresh calls = %d, want 1 across processes", n)
	}
	if tokens[0] != tokens[1] {
		t.Errorf("processes hold different tokens: %q vs %q", tokens[0], tokens[1])
	}
	saved, err := LoadUserToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != tokens[0] {
		t.Errorf("sidecar holds %q, want the token both processes use (%q)", saved.AccessToken, tokens[0])
	}
	if _, err := os.Stat(path + ".lock"); err == nil {
		t.Error("lock file was left behind")
	}
}

// TestRefresherAdoptsTokenRotatedByAnotherProcess proves a refresher whose
// in-memory token is stale picks up the sidecar another process rewrote instead
// of spending its dead refresh token.
func TestRefresherAdoptsTokenRotatedByAnotherProcess(t *testing.T) {
	var calls int32
	srv := rotatingServer(t, &calls)
	path := seedToken(t, &UserToken{AccessToken: "ghu_old", RefreshToken: "ghr_old", Expiry: time.Now().Add(-time.Minute)})

	r, err := NewTokenRefresher("cid-adopt", srv.URL, path)
	if err != nil {
		t.Fatal(err)
	}
	// Another process rotates and persists while we hold only the old token.
	if err := SaveUserToken(path, &UserToken{
		AccessToken:  "ghu_from_other",
		RefreshToken: "ghr_from_other",
		Expiry:       time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	tok, err := r.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghu_from_other" {
		t.Errorf("token = %q, want the one the other process persisted", tok)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("refresh calls = %d, want 0 (the stored token is still valid)", n)
	}
}

// TestRefresherRejectsEmptyAccessToken proves an empty sidecar fails at load with
// an actionable message instead of authenticating with an empty bearer token.
func TestRefresherRejectsEmptyAccessToken(t *testing.T) {
	path := seedToken(t, &UserToken{})
	_, err := NewTokenRefresher("cid-empty", "https://example.test", path)
	if err == nil {
		t.Fatal("expected an error for a sidecar with no access token")
	}
	if !strings.Contains(err.Error(), "multirunner connect") {
		t.Errorf("error = %v, want it to name `multirunner connect`", err)
	}
}
