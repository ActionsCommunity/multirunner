package ghapp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefresherPreservesSymlinkAndSharesCanonicalLock(t *testing.T) {
	var calls int32
	srv := rotatingServer(t, &calls)
	old := &UserToken{AccessToken: "old", RefreshToken: "old-refresh", Expiry: time.Now().Add(-time.Minute)}
	real := seedToken(t, old)
	alias := filepath.Join(filepath.Dir(real), "alias.json")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	first, err := NewTokenRefresher("cid", srv.URL, alias)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := NewTokenRefresher("cid", srv.URL, real)
	if err != nil {
		t.Fatal(err)
	}
	if first != shared {
		t.Fatal("alias and target have separate refreshers")
	}
	// An independent refresher models another process reading the real path.
	second := &TokenRefresher{clientID: "cid", baseURL: srv.URL, tokenPath: first.tokenPath, tok: old}
	var wg sync.WaitGroup
	for _, r := range []*TokenRefresher{first, second} {
		wg.Add(1)
		go func(r *TokenRefresher) {
			defer wg.Done()
			if _, err := r.AccessToken(context.Background()); err != nil {
				t.Error(err)
			}
		}(r)
	}
	wg.Wait()
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("refreshes=%d, want 1", calls)
	}
	fi, err := os.Lstat(alias)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v", err)
	}
	tok, err := LoadUserToken(real)
	if err != nil || tok.AccessToken != "ghu_new1" {
		t.Fatalf("canonical token stale: %+v, %v", tok, err)
	}
	if _, err := os.Stat(alias + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("alias has its own lock: %v", err)
	}
}

func TestRefresherKeysPreserveCase(t *testing.T) {
	dir := t.TempDir()
	if refresherKey("cid", "url", filepath.Join(dir, "UPPER.json")) == refresherKey("cid", "url", filepath.Join(dir, "upper.json")) {
		t.Fatal("case-distinct credential paths share a key")
	}
}

func TestRefresherLoadsCaseDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	upper, lower := filepath.Join(dir, "UPPER.json"), filepath.Join(dir, "upper.json")
	if err := SaveUserToken(upper, &UserToken{AccessToken: "upper"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserToken(lower, &UserToken{AccessToken: "lower"}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(upper)
	b, _ := os.Stat(lower)
	if os.SameFile(a, b) {
		t.Skip("case-insensitive filesystem")
	}
	for _, tc := range []struct{ path, token string }{{upper, "upper"}, {lower, "lower"}} {
		r, err := NewTokenRefresher("cid", "https://example.test", tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if r.Current() != tc.token {
			t.Fatalf("%s uses another file's credential", tc.path)
		}
	}
}
