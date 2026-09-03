package ghapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TokenSkew refreshes a device-flow user token this long before it actually
// expires, so a request already in flight does not race the expiry.
const TokenSkew = 5 * time.Minute

// lockPoll is how often a process retries the on-disk refresh lock, and
// lockTimeout is how long it waits before assuming the holder died.
const (
	lockPoll    = 50 * time.Millisecond
	lockTimeout = 30 * time.Second
)

// TokenRefresher hands out a valid device-flow user access token, refreshing and
// persisting a rotated one when the current token is expired or within TokenSkew
// of expiry.
//
// Refreshing is a rotation: GitHub invalidates both the old access token and the
// old refresh token as soon as a refresh succeeds. Two refreshes racing on the
// same sidecar therefore leave one side holding a dead credential, so the
// coordination is deliberately broad:
//
//   - Refreshers are shared per token path (see NewTokenRefresher), so every pool
//     and every client in this process contends on one mutex rather than one each.
//   - The refresh runs under a lock file next to the sidecar, so a second process
//     (a concurrent `multirunner run`, or `doctor`) cannot rotate at the same time.
//   - The sidecar is re-read inside both locks, so a token another process just
//     rotated is picked up instead of being overwritten with a stale one.
//
// It exists so the refresh, skew and token-store rules live in exactly one place
// rather than being copied into each transport.
type TokenRefresher struct {
	clientID  string
	baseURL   string // web host serving the device/oauth endpoints (GHES-aware)
	tokenPath string

	mu sync.Mutex
	// tok is the token most recently loaded or refreshed. unsaved marks a token
	// that GitHub has already rotated to but that could not be written to disk;
	// every later call retries the write, because losing it means the sidecar
	// still names a refresh token GitHub has invalidated.
	tok     *UserToken
	unsaved bool
}

// refreshers keeps one TokenRefresher per (token path, client, host) so pools
// sharing a credential share the mutex that serialises rotation.
var (
	refreshersMu sync.Mutex
	refreshers   = map[string]*TokenRefresher{}
)

// NewTokenRefresher returns the refresher for tokenPath, loading the sidecar on
// first use. clientID and baseURL fall back to the shared App's defaults when
// empty. Callers that share a token path share the returned refresher.
func NewTokenRefresher(clientID, baseURL, tokenPath string) (*TokenRefresher, error) {
	clientID = orDefault(clientID, DefaultClientID)
	baseURL = strings.TrimRight(orDefault(baseURL, DefaultBaseURL), "/")

	key := refresherKey(clientID, baseURL, tokenPath)
	refreshersMu.Lock()
	defer refreshersMu.Unlock()
	if r, ok := refreshers[key]; ok {
		return r, nil
	}

	tok, err := loadValidToken(tokenPath)
	if err != nil {
		return nil, err
	}
	r := &TokenRefresher{
		clientID:  clientID,
		baseURL:   baseURL,
		tokenPath: tokenPath,
		tok:       tok,
	}
	refreshers[key] = r
	return r, nil
}

// refresherKey canonicalises the token path so two configs naming the same file
// differently still share a refresher. An unresolvable path is used as given
// rather than failing: worst case the two callers do not share.
func refresherKey(clientID, baseURL, tokenPath string) string {
	path := tokenPath
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return clientID + "\x00" + baseURL + "\x00" + strings.ToLower(filepath.Clean(path))
}

// loadValidToken reads the sidecar and rejects one with no access token, which
// would otherwise be handed out as an empty bearer credential and fail as an
// opaque 401 far from its cause.
func loadValidToken(path string) (*UserToken, error) {
	tok, err := LoadUserToken(path)
	if err != nil {
		return nil, fmt.Errorf("read user token %s: %w (re-run `multirunner connect`)", path, err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("user token %s has no access token; re-run `multirunner connect`", path)
	}
	return tok, nil
}

// Current returns the token value most recently loaded or refreshed, without
// triggering a refresh. The scale-set library captures this value at client
// construction and re-sends it verbatim, so the transport uses it to recognise
// the credential it must rewrite.
func (r *TokenRefresher) Current() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tok.AccessToken
}

// AccessToken returns a valid access token, refreshing and persisting a new one
// under the mutex when the current token is near expiry.
func (r *TokenRefresher) AccessToken(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.retrySave()
	if !needsRefresh(r.tok) {
		return r.tok.AccessToken, nil
	}
	if r.tok.RefreshToken == "" {
		// A non-expiring token has no refresh token, so there is nothing to
		// rotate and the current value is the best available.
		return r.tok.AccessToken, nil
	}

	unlock, err := lockTokenFile(ctx, r.tokenPath)
	if err != nil {
		return "", err
	}
	defer unlock()

	// Another process may have rotated while we waited for the lock; its token is
	// the live one and ours is already dead.
	if stored, err := LoadUserToken(r.tokenPath); err == nil && stored.AccessToken != "" && !needsRefresh(stored) {
		r.tok, r.unsaved = stored, false
		return r.tok.AccessToken, nil
	}

	refreshed, err := RefreshUserToken(ctx, r.clientID, r.baseURL, r.tok.RefreshToken)
	if err != nil {
		return "", err
	}
	// GitHub has already invalidated the old pair, so the new token is adopted
	// even if it cannot be written: failing here would strand a working process,
	// and retrySave keeps trying to persist it.
	r.tok, r.unsaved = refreshed, false
	if err := SaveUserToken(r.tokenPath, refreshed); err != nil {
		r.unsaved = true
		fmt.Fprintf(os.Stderr, "warning: refreshed the GitHub user token but could not save it to %s: %v\n"+
			"         it will be retried; if it keeps failing, re-run `multirunner connect` after fixing the path\n",
			r.tokenPath, err)
	}
	return r.tok.AccessToken, nil
}

// retrySave re-attempts a write that failed earlier. Callers hold r.mu.
func (r *TokenRefresher) retrySave() {
	if !r.unsaved {
		return
	}
	if err := SaveUserToken(r.tokenPath, r.tok); err == nil {
		r.unsaved = false
	}
}

// lockTokenFile takes an exclusive on-disk lock for path, so only one process
// rotates a given sidecar at a time. The lock is an O_EXCL sentinel rather than
// a byte-range lock because it must behave the same on Windows and Unix. A stale
// lock (holder crashed) is broken after lockTimeout.
func lockTokenFile(ctx context.Context, path string) (func(), error) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock token store %s: %w", path, err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > lockTimeout {
			os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for the token refresh lock %s; "+
				"remove it if no other multirunner process is running", lockPath)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// needsRefresh reports whether the token should be refreshed. A zero Expiry means
// the App does not expire user tokens, so it never needs refreshing.
func needsRefresh(tok *UserToken) bool {
	if tok.Expiry.IsZero() {
		return false
	}
	return time.Now().After(tok.Expiry.Add(-TokenSkew))
}
