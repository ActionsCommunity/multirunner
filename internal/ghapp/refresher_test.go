package ghapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	// The sentinel is deliberately left on disk: the lock is the OS lock held on
	// an open handle, and unlinking the file is what let one waiter delete
	// another's lock. What matters is that it is no longer held.
	if err := assertLockFree(path); err != nil {
		t.Error(err)
	}
}

// assertLockFree reports whether the sidecar's lock can be taken right now,
// which is the only meaningful "released" check once the lock lives on a handle
// rather than on the file's existence.
func assertLockFree(path string) error {
	unlock, err := lockTokenFile(context.Background(), path)
	if err != nil {
		return fmt.Errorf("lock is still held after the refresh finished: %w", err)
	}
	unlock()
	return nil
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

// TestRefresherLockHoldsUnderHeavyContention is the regression test for a lock
// that broke itself. When the lock was a sentinel file whose age decided whether
// it was abandoned, every waiter independently removed the file it had just
// stat'd - including one another's fresh locks - so contenders both "held" it,
// unlock removed a lock its caller did not own, and on Windows a concurrent
// remove made the next create fail with a permission error rather than ErrExist.
// One refresh must survive a crowd, with no waiter erroring out.
func TestRefresherLockHoldsUnderHeavyContention(t *testing.T) {
	const contenders = 12

	var calls int32
	srv := rotatingServer(t, &calls)
	path := seedToken(t, &UserToken{AccessToken: "ghu_old", RefreshToken: "ghr_old", Expiry: time.Now().Add(-time.Minute)})

	// Distinct client ids stand in for distinct processes: the per-path cache
	// does not merge them, so each contends on the file lock rather than on
	// one shared mutex.
	refreshers := make([]*TokenRefresher, contenders)
	for i := range refreshers {
		r, err := NewTokenRefresher(fmt.Sprintf("cid-%02d", i), srv.URL, path)
		if err != nil {
			t.Fatal(err)
		}
		refreshers[i] = r
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	tokens := make([]string, contenders)
	errs := make([]error, contenders)
	for i, r := range refreshers {
		wg.Add(1)
		go func(i int, r *TokenRefresher) {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = r.AccessToken(context.Background())
		}(i, r)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("contender %d failed instead of waiting its turn: %v", i, err)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("refresh calls = %d, want 1: a second rotation invalidates the first pair", n)
	}
	for i, tok := range tokens {
		if tok != tokens[0] {
			t.Errorf("contender %d holds %q, want the single rotated token %q", i, tok, tokens[0])
		}
	}
	if err := assertLockFree(path); err != nil {
		t.Error(err)
	}
}

// TestRefreshOutlivesTheCallerThatTriggeredIt pins that a cancelled caller does
// not cancel the rotation. GitHub invalidates the old pair as soon as it
// processes the request, so abandoning it mid-flight leaves the new pair on
// GitHub's side only and this host holding a credential that is already dead.
func TestRefreshOutlivesTheCallerThatTriggeredIt(t *testing.T) {
	var calls int32
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		// Long enough that a request bound to the caller's context would be
		// cancelled before the response is written.
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fmtToken(n))
		close(served)
	}))
	defer srv.Close()

	path := seedToken(t, &UserToken{AccessToken: "ghu_old", RefreshToken: "ghr_old", Expiry: time.Now().Add(-time.Minute)})
	r, err := NewTokenRefresher("cid-cancel", srv.URL, path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	tok, err := r.AccessToken(ctx)
	if err != nil {
		t.Fatalf("refresh abandoned when the caller was cancelled: %v", err)
	}

	<-served
	if tok == "ghu_old" {
		t.Error("token was not rotated")
	}
	saved, err := LoadUserToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != tok {
		t.Errorf("sidecar holds %q, want the rotated token %q", saved.AccessToken, tok)
	}
}
