package ghapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/GerardSmit/multirunner/internal/securefile"
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

	if err := securefile.Replace(path, data); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return securefile.CreateExclusive(path, data)
}

// WriteSecretFile writes a credential with access restricted to the owner
// (and LocalSystem on Windows), replacing any existing file. Use it for every
// credential connect persists: os.WriteFile's
// mode argument is ignored on Windows, so a plain 0600 write leaves the file
// readable by other local accounts.
func WriteSecretFile(path string, data []byte) error {
	if err := securefile.Replace(path, data); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return securefile.CreateExclusive(path, data)
}
