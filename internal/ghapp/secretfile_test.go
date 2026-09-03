package ghapp

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSecretFilesAreOwnerOnly guards a platform trap: os.WriteFile's mode is
// ignored on Windows, so a credential written with 0600 stays readable by every
// local account unless an explicit DACL is applied.
func TestSecretFilesAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()

	secret := filepath.Join(dir, "secret")
	if err := WriteSecretFile(secret, []byte("credential")); err != nil {
		t.Fatalf("WriteSecretFile: %v", err)
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != "credential" {
		t.Fatalf("readback = %q, %v", got, err)
	}

	tokenPath := filepath.Join(dir, "token.json")
	tok := &UserToken{AccessToken: "ghu_x", RefreshToken: "ghr_y", Expiry: time.Now().Add(time.Hour)}
	if err := SaveUserToken(tokenPath, tok); err != nil {
		t.Fatalf("SaveUserToken: %v", err)
	}
	back, err := LoadUserToken(tokenPath)
	if err != nil || back.AccessToken != "ghu_x" || back.RefreshToken != "ghr_y" {
		t.Fatalf("round trip = %+v, %v", back, err)
	}

	// Overwriting must keep the restriction, not inherit the directory's DACL.
	if err := WriteSecretFile(secret, []byte("rotated")); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	for _, p := range []string{secret, tokenPath} {
		if err := checkOwnerOnly(p); err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
		}
	}
}
