package ghapp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSaveLoadUserTokenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	want := &UserToken{
		AccessToken:   "ghu_abc",
		RefreshToken:  "ghr_def",
		Expiry:        time.Now().Add(8 * time.Hour).Round(time.Second),
		RefreshExpiry: time.Now().Add(180 * 24 * time.Hour).Round(time.Second),
	}
	if err := SaveUserToken(path, want); err != nil {
		t.Fatalf("SaveUserToken: %v", err)
	}
	got, err := LoadUserToken(path)
	if err != nil {
		t.Fatalf("LoadUserToken: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("token = %+v, want %+v", got, want)
	}
	if !got.Expiry.Equal(want.Expiry) || !got.RefreshExpiry.Equal(want.RefreshExpiry) {
		t.Errorf("expiries = %v / %v, want %v / %v", got.Expiry, got.RefreshExpiry, want.Expiry, want.RefreshExpiry)
	}
}

// TestSaveUserTokenAtomicOverwrite proves a rotated token replaces the previous
// sidecar in place (the refresh path), which relies on os.Rename replacing an
// existing file on this OS.
func TestSaveUserTokenAtomicOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := SaveUserToken(path, &UserToken{AccessToken: "ghu_old", RefreshToken: "ghr_old"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserToken(path, &UserToken{AccessToken: "ghu_new", RefreshToken: "ghr_new"}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err := LoadUserToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "ghu_new" || got.RefreshToken != "ghr_new" {
		t.Errorf("token after overwrite = %+v", got)
	}

	// No temp files should be left behind next to the sidecar.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "token.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("stray files left in token dir: %v", names)
	}
}

func TestSaveUserTokenMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file mode bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "token.json")
	if err := SaveUserToken(path, &UserToken{AccessToken: "ghu_abc"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadUserTokenMissing(t *testing.T) {
	if _, err := LoadUserToken(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("want error for missing token store")
	}
}
