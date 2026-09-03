package ghapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// storedToken is the on-disk JSON shape of a user access token sidecar. The
// OAuth scope is deliberately absent: GitHub Apps always return an empty scope
// for user tokens, so there is nothing to persist.
type storedToken struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token"`
	Expiry        time.Time `json:"expiry"`
	RefreshExpiry time.Time `json:"refresh_expiry"`
}

// LoadUserToken reads a user access token sidecar written by SaveUserToken.
//
// A sidecar other accounts can read is reported, not rejected: the token is
// already on disk by then, and refusing to start would take a host down over a
// file mode that drifted rather than protect anything still secret.
func LoadUserToken(path string) (*UserToken, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	warnIfPermissive(path)
	var s storedToken
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse token store %s: %w", path, err)
	}
	return &UserToken{
		AccessToken:   s.AccessToken,
		RefreshToken:  s.RefreshToken,
		Expiry:        s.Expiry,
		RefreshExpiry: s.RefreshExpiry,
	}, nil
}

// SaveUserToken writes tok to path at mode 0600. The write goes to a temp file
// in the same directory and is renamed over path, so a crash mid-write cannot
// truncate an existing sidecar and lose the refresh token. os.Rename replaces an
// existing file on both Unix and Windows; the error is surfaced rather than
// assumed away.
func SaveUserToken(path string, tok *UserToken) error {
	data, err := json.MarshalIndent(storedToken{
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
		Expiry:        tok.Expiry,
		RefreshExpiry: tok.RefreshExpiry,
	}, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mr-token-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp token file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename below succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp token file: %w", err)
	}
	// Restrict before the token is written, so the bytes never sit in a
	// world-readable file even briefly.
	if err := restrictToOwner(tmpName); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp token file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace token store %s: %w", path, err)
	}
	return nil
}

// WriteSecretFile writes a credential to path with owner-only access, replacing
// any existing file. Use it for every credential connect persists: os.WriteFile's
// mode argument is ignored on Windows, so a plain 0600 write leaves the file
// readable by other local accounts.
func WriteSecretFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := restrictToOwner(path); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Close()
}
