// Package consoleauth provides local pairing and session authentication for
// the Multirunner Operations Console.
package consoleauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GerardSmit/multirunner/internal/securefile"
)

const (
	secretBytes           = 32
	pairingLifetime       = 2 * time.Minute
	sessionLifetime       = 12 * time.Hour
	recentPairingLifetime = 10 * time.Minute
	maxUsedPairingNonces  = 4096
	maxPairingAttempts    = 5
	sessionCookie         = "multirunner_console_session"
	pairingProofCookie    = "multirunner_console_pairing_proof"
	ProofHeader           = "X-Multirunner-Console-Proof"
)

type sessionContextKey struct{}

type Session struct {
	ActorID              string    `json:"actor_id"`
	CSRFToken            string    `json:"csrf_token"`
	IssuedAt             time.Time `json:"issued_at"`
	PairedAt             time.Time `json:"paired_at"`
	ExpiresAt            time.Time `json:"expires_at"`
	RevocationGeneration uint64    `json:"revocation_generation"`
	nonce                string
}

// SecretPath returns the console secret path associated with a history
// database.
func SecretPath(databasePath string) string {
	return filepath.Join(filepath.Dir(databasePath), "console.secret")
}

// PairingStatePath returns the durable replay state associated with a secret.
func PairingStatePath(secretPath string) string {
	return secretPath + ".pairing-used"
}

// EnsureSecret loads an existing secret or creates one for first use.
func EnsureSecret(path string) ([]byte, error) {
	data, err := securefile.Read(path)
	if err == nil {
		return decodeSecret(data)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read console secret: %w", err)
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate console secret: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create console secret directory: %w", err)
	}
	encoded := []byte(base64.RawURLEncoding.EncodeToString(secret) + "\n")
	err = securefile.CreateExclusive(path, encoded)
	if errors.Is(err, os.ErrExist) {
		data, err = securefile.Read(path)
		if err != nil {
			return nil, fmt.Errorf("read concurrently created console secret: %w", err)
		}
		return decodeSecret(data)
	}
	if err != nil {
		return nil, fmt.Errorf("create console secret: %w", err)
	}
	return secret, nil
}

// LoadSecret reads an existing console secret.
func LoadSecret(path string) ([]byte, error) {
	data, err := securefile.Read(path)
	if err != nil {
		return nil, fmt.Errorf("read console secret: %w", err)
	}
	return decodeSecret(data)
}

// RotateSecret atomically replaces an existing protected console secret.
func RotateSecret(path string) ([]byte, error) {
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate console secret: %w", err)
	}
	encoded := []byte(base64.RawURLEncoding.EncodeToString(secret) + "\n")
	if err := securefile.Replace(path, encoded); err != nil {
		return nil, fmt.Errorf("replace console secret: %w", err)
	}
	return secret, nil
}

func decodeSecret(data []byte) ([]byte, error) {
	secret, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(secret) != secretBytes {
		return nil, errors.New("console secret is invalid")
	}
	return secret, nil
}

// Authenticator validates pairing tokens and local browser sessions.
type Authenticator struct {
	secret     []byte
	now        func() time.Time
	usedPath   string
	mu         sync.Mutex
	used       map[string]time.Time
	attempts   []time.Time
	generation uint64
	revoked    chan struct{}
}

type pairingUseState struct {
	Version int              `json:"version"`
	Used    map[string]int64 `json:"used"`
}

// New creates an authenticator from a 256-bit installation secret.
func New(secret []byte) (*Authenticator, error) {
	return NewWithClock(secret, time.Now)
}

// NewWithClock creates an authenticator with an injected clock.
func NewWithClock(secret []byte, now func() time.Time) (*Authenticator, error) {
	return newAuthenticator(secret, now, "")
}

// NewPersistent creates an authenticator with durable pairing replay state.
func NewPersistent(secret []byte, usedPath string) (*Authenticator, error) {
	return newAuthenticator(secret, time.Now, usedPath)
}

func newAuthenticator(secret []byte, now func() time.Time, usedPath string) (*Authenticator, error) {
	if len(secret) != secretBytes {
		return nil, errors.New("console secret must be 32 bytes")
	}
	if now == nil {
		return nil, errors.New("console authentication clock is required")
	}
	authenticator := &Authenticator{
		secret:     append([]byte(nil), secret...),
		now:        now,
		usedPath:   usedPath,
		used:       make(map[string]time.Time),
		generation: 1,
		revoked:    make(chan struct{}),
	}
	if usedPath != "" {
		if err := authenticator.loadUsed(); err != nil {
			return nil, err
		}
	}
	return authenticator, nil
}

// PairingToken creates a short-lived, single-use browser pairing token.
func (a *Authenticator) PairingToken() (string, error) {
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate pairing nonce: %w", err)
	}
	return a.sign("pair", base64.RawURLEncoding.EncodeToString(nonce), a.now().Add(pairingLifetime)), nil
}

// Handler exposes pairing, protects console routes with the session cookie,
// and requires the independent origin proof for every API route.
func (a *Authenticator) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/pair" {
			a.pair(w, r)
			return
		}
		session, ok := a.session(r)
		if !ok {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				if r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/") {
					next.ServeHTTP(w, r)
					return
				}
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("Console pairing required. Run `multirunner console open` on this host.\n"))
			return
		}
		if (r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")) &&
			!a.validateOriginProof(r, session) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("Console pairing proof is required. Run `multirunner console open` on this host.\n"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(
			r.Context(), sessionContextKey{}, session,
		)))
	})
}

func (a *Authenticator) pair(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validPairingOrigin(r) {
		http.Error(w, "pairing request origin is invalid", http.StatusForbidden)
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		http.Error(w, "pairing request must be JSON", http.StatusUnsupportedMediaType)
		return
	}
	if allowed, retryAfter := a.reservePairingAttempt(); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Round(time.Second).Seconds())))
		http.Error(w, "too many pairing attempts", http.StatusTooManyRequests)
		return
	}
	var request struct {
		Token string `json:"token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "pairing token is invalid or expired", http.StatusUnauthorized)
		return
	}
	token := strings.TrimSpace(request.Token)
	if len(token) > 512 {
		http.Error(w, "pairing token is invalid or expired", http.StatusUnauthorized)
		return
	}
	nonce, ok := a.verify("pair", token)
	if !ok {
		http.Error(w, "pairing token is invalid or expired", http.StatusUnauthorized)
		return
	}
	used, err := a.useNonce(nonce)
	if err != nil {
		http.Error(w, "pairing is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	if !used {
		http.Error(w, "pairing token is invalid or expired", http.StatusUnauthorized)
		return
	}
	proof, err := a.issueSession(w)
	if err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     pairingProofCookie,
		Value:    proof,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(pairingLifetime.Seconds()),
		Expires:  a.now().Add(pairingLifetime),
	})
	a.clearPairingAttempts()
	w.WriteHeader(http.StatusNoContent)
}

func (a *Authenticator) issueSession(w http.ResponseWriter) (string, error) {
	sessionNonce := make([]byte, 24)
	if _, err := rand.Read(sessionNonce); err != nil {
		return "", err
	}
	expires := a.now().Add(sessionLifetime)
	issuedAt := a.now().UTC()
	a.mu.Lock()
	generation := a.generation
	a.mu.Unlock()
	sessionNonceValue := base64.RawURLEncoding.EncodeToString(sessionNonce)
	sessionValue := sessionNonceValue + ":" + strconv.FormatInt(issuedAt.Unix(), 10) + ":" + strconv.FormatUint(generation, 10)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.sign("session", sessionValue, expires),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  expires,
		MaxAge:   int(sessionLifetime.Seconds()),
	})
	return a.mac("origin-proof", sessionNonceValue), nil
}

func validPairingOrigin(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Origin")), "http://"+r.Host) {
		return false
	}
	fetchSite := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
	return fetchSite == "" || fetchSite == "same-origin"
}

func (a *Authenticator) reservePairingAttempt() (bool, time.Duration) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.attempts[:0]
	for _, attemptedAt := range a.attempts {
		if attemptedAt.Add(pairingLifetime).After(now) {
			kept = append(kept, attemptedAt)
		}
	}
	a.attempts = kept
	if len(a.attempts) >= maxPairingAttempts {
		return false, a.attempts[0].Add(pairingLifetime).Sub(now)
	}
	a.attempts = append(a.attempts, now)
	return true, 0
}

func (a *Authenticator) clearPairingAttempts() {
	a.mu.Lock()
	a.attempts = nil
	a.mu.Unlock()
}

func (a *Authenticator) session(r *http.Request) (Session, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return Session{}, false
	}
	value, expiresAt, ok := a.verifyWithExpiry("session", cookie.Value)
	if !ok {
		return Session{}, false
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return Session{}, false
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil || len(decoded) != 24 {
		return Session{}, false
	}
	issuedUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Session{}, false
	}
	generation, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return Session{}, false
	}
	a.mu.Lock()
	currentGeneration := a.generation
	a.mu.Unlock()
	if generation != currentGeneration {
		return Session{}, false
	}
	issuedAt := time.Unix(issuedUnix, 0).UTC()
	return Session{
		ActorID:              "local-" + a.mac("actor", parts[0])[:16],
		CSRFToken:            a.mac("csrf", parts[0]),
		IssuedAt:             issuedAt,
		PairedAt:             issuedAt,
		ExpiresAt:            expiresAt,
		RevocationGeneration: generation,
		nonce:                parts[0],
	}, true
}

func SessionFromContext(ctx context.Context) (Session, bool) {
	session, ok := ctx.Value(sessionContextKey{}).(Session)
	return session, ok
}

func (a *Authenticator) ValidateCSRF(r *http.Request) bool {
	session, ok := SessionFromContext(r.Context())
	if !ok {
		return false
	}
	return hmac.Equal(
		[]byte(session.CSRFToken),
		[]byte(r.Header.Get("X-CSRF-Token")),
	)
}

// ValidateRecentPairing reports whether the authenticated session was paired
// recently enough to authorize a sensitive operator action.
func (a *Authenticator) ValidateRecentPairing(r *http.Request) bool {
	session, ok := SessionFromContext(r.Context())
	if !ok {
		return false
	}
	age := a.now().Sub(session.PairedAt)
	return age >= 0 && age <= recentPairingLifetime
}

// SessionRevocation returns a signal that closes when the authenticated
// session's revocation generation is invalidated.
func (a *Authenticator) SessionRevocation(session Session) (<-chan struct{}, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if session.RevocationGeneration != a.generation || !a.now().Before(session.ExpiresAt) {
		return nil, false
	}
	return a.revoked, true
}

// ValidateSession reports whether an authenticated session remains current.
func (a *Authenticator) ValidateSession(session Session) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return session.RevocationGeneration == a.generation && a.now().Before(session.ExpiresAt)
}

// RevokeSessions invalidates every session issued by this authenticator.
func (a *Authenticator) RevokeSessions() {
	a.mu.Lock()
	a.generation++
	close(a.revoked)
	a.revoked = make(chan struct{})
	a.mu.Unlock()
}

func (a *Authenticator) validateOriginProof(r *http.Request, session Session) bool {
	proof := strings.TrimSpace(r.Header.Get(ProofHeader))
	if proof == "" || len(proof) > 128 {
		return false
	}
	provided, err := base64.RawURLEncoding.DecodeString(proof)
	if err != nil {
		return false
	}
	expected, err := base64.RawURLEncoding.DecodeString(a.mac("origin-proof", session.nonce))
	return err == nil && hmac.Equal(provided, expected)
}

func (a *Authenticator) useNonce(nonce string) (bool, error) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for value, expiry := range a.used {
		if !expiry.After(now) {
			delete(a.used, value)
		}
	}
	if _, exists := a.used[nonce]; exists {
		return false, nil
	}
	if len(a.used) >= maxUsedPairingNonces {
		return false, errors.New("pairing replay state is full")
	}
	a.used[nonce] = now.Add(pairingLifetime)
	if err := a.persistUsedLocked(); err != nil {
		delete(a.used, nonce)
		return false, err
	}
	return true, nil
}

func (a *Authenticator) loadUsed() error {
	data, err := securefile.Read(a.usedPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pairing replay state: %w", err)
	}
	var state pairingUseState
	if err := json.Unmarshal(data, &state); err != nil || state.Version != 1 || state.Used == nil {
		return errors.New("pairing replay state is invalid")
	}
	if len(state.Used) > maxUsedPairingNonces {
		return errors.New("pairing replay state exceeds its bounded capacity")
	}
	now := a.now()
	for nonce, expiryUnix := range state.Used {
		if nonce == "" {
			return errors.New("pairing replay state is invalid")
		}
		expiry := time.Unix(expiryUnix, 0)
		if expiry.After(now) {
			a.used[nonce] = expiry
		}
	}
	return nil
}

func (a *Authenticator) persistUsedLocked() error {
	if a.usedPath == "" {
		return nil
	}
	state := pairingUseState{
		Version: 1,
		Used:    make(map[string]int64, len(a.used)),
	}
	for nonce, expiry := range a.used {
		state.Used[nonce] = expiry.Unix()
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode pairing replay state: %w", err)
	}
	data = append(data, '\n')
	if err := securefile.Replace(a.usedPath, data); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace pairing replay state: %w", err)
	}
	if err := securefile.CreateExclusive(a.usedPath, data); err != nil {
		return fmt.Errorf("create pairing replay state: %w", err)
	}
	return nil
}

func (a *Authenticator) sign(kind, nonce string, expiry time.Time) string {
	payload := kind + "." + nonce + "." + strconv.FormatInt(expiry.Unix(), 10)
	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Authenticator) mac(kind, value string) string {
	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(kind + "." + value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Authenticator) verify(kind, token string) (string, bool) {
	value, _, ok := a.verifyWithExpiry(kind, token)
	return value, ok
}

func (a *Authenticator) verifyWithExpiry(kind, token string) (string, time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != kind || parts[1] == "" {
		return "", time.Time{}, false
	}
	expiry, err := strconv.ParseInt(parts[2], 10, 64)
	expiresAt := time.Unix(expiry, 0).UTC()
	if err != nil || !a.now().Before(expiresAt) {
		return "", time.Time{}, false
	}
	payload := strings.Join(parts[:3], ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", time.Time{}, false
	}
	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(payload))
	return parts[1], expiresAt, hmac.Equal(signature, mac.Sum(nil))
}
