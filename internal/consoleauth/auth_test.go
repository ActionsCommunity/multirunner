package consoleauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/securefile"
)

func TestPairingCreatesSessionAndIsSingleUse(t *testing.T) {
	auth, err := New(make([]byte, secretBytes))
	if err != nil {
		t.Fatal(err)
	}
	auth.now = func() time.Time { return time.Unix(1000, 0) }
	token, err := auth.PairingToken()
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := SessionFromContext(r.Context())
		if !ok || session.ActorID == "" || session.CSRFToken == "" {
			t.Fatal("authenticated session was not attached to the request")
		}
		r.Header.Set("X-CSRF-Token", session.CSRFToken)
		if !auth.ValidateCSRF(r) {
			t.Fatal("session CSRF token was rejected")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := auth.Handler(next)

	urlAttempt := httptest.NewRecorder()
	handler.ServeHTTP(
		urlAttempt,
		httptest.NewRequest(http.MethodGet, "/auth/pair?token="+token, nil),
	)
	if urlAttempt.Code != http.StatusMethodNotAllowed || len(urlAttempt.Result().Cookies()) != 0 {
		t.Fatalf("URL pairing attempt = %d cookies=%d", urlAttempt.Code, len(urlAttempt.Result().Cookies()))
	}

	pair := httptest.NewRecorder()
	handler.ServeHTTP(pair, newPairingRequest(token))
	if pair.Code != http.StatusNoContent {
		t.Fatalf("pairing returned %d", pair.Code)
	}
	sessionCookieValue, proof := pairingCredentials(t, pair)
	if pair.Header().Get("Location") != "" || strings.Contains(pair.Body.String(), proof) {
		t.Fatal("pairing credential appeared in a redirect or response body")
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(sessionCookieValue)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("authenticated request returned %d", response.Code)
	}

	apiRequest := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	apiRequest.AddCookie(sessionCookieValue)
	apiRequest.Header.Set(ProofHeader, proof)
	apiResponse := httptest.NewRecorder()
	handler.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusNoContent {
		t.Fatalf("authenticated API request returned %d", apiResponse.Code)
	}

	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, newPairingRequest(token))
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("pairing replay returned %d", replay.Code)
	}
}

func TestFirstCallerWithoutDisplayedTokenGetsNoAuthority(t *testing.T) {
	auth, err := New(make([]byte, secretBytes))
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusNoContent || len(page.Result().Cookies()) != 0 {
		t.Fatalf("unauthenticated page = %d cookies=%d", page.Code, len(page.Result().Cookies()))
	}
	api := httptest.NewRecorder()
	handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/v1/session", nil))
	if api.Code != http.StatusUnauthorized || len(api.Result().Cookies()) != 0 {
		t.Fatalf("unauthenticated API = %d cookies=%d", api.Code, len(api.Result().Cookies()))
	}
	pair := httptest.NewRecorder()
	handler.ServeHTTP(pair, newPairingRequest(""))
	if pair.Code != http.StatusUnauthorized || len(pair.Result().Cookies()) != 0 {
		t.Fatalf("empty pairing = %d cookies=%d", pair.Code, len(pair.Result().Cookies()))
	}
}

func TestPairingRejectsCrossOriginAndBoundsInvalidAttempts(t *testing.T) {
	now := time.Unix(1000, 0)
	auth, err := NewWithClock(make([]byte, secretBytes), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.Handler(http.NotFoundHandler())

	crossOrigin := newPairingRequest("invalid")
	crossOrigin.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, crossOrigin)
	if response.Code != http.StatusForbidden || len(response.Result().Cookies()) != 0 {
		t.Fatalf("cross-origin pairing = %d cookies=%d", response.Code, len(response.Result().Cookies()))
	}

	for attempt := 0; attempt < maxPairingAttempts; attempt++ {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, newPairingRequest("invalid"))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid attempt %d = %d", attempt+1, response.Code)
		}
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, newPairingRequest("invalid"))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("bounded attempt = %d retry=%q", response.Code, response.Header().Get("Retry-After"))
	}
	now = now.Add(pairingLifetime)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, newPairingRequest("invalid"))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("attempt after rate window = %d", response.Code)
	}
}

func TestConsumedPairingTokenRemainsSingleUseAfterRestart(t *testing.T) {
	secret := bytesOf(0x42)
	statePath := PairingStatePath(filepath.Join(t.TempDir(), "console.secret"))
	issuer, err := New(secret)
	if err != nil {
		t.Fatal(err)
	}
	token, err := issuer.PairingToken()
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewPersistent(secret, statePath)
	if err != nil {
		t.Fatal(err)
	}
	firstResponse := httptest.NewRecorder()
	first.Handler(http.NotFoundHandler()).ServeHTTP(
		firstResponse,
		newPairingRequest(token),
	)
	if firstResponse.Code != http.StatusNoContent {
		t.Fatalf("initial pairing returned %d", firstResponse.Code)
	}

	restarted, err := NewPersistent(secret, statePath)
	if err != nil {
		t.Fatal(err)
	}
	replay := httptest.NewRecorder()
	restarted.Handler(http.NotFoundHandler()).ServeHTTP(
		replay,
		newPairingRequest(token),
	)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("pairing replay after restart returned %d", replay.Code)
	}
}

func TestPersistentPairingStateFailsClosedWhenInvalid(t *testing.T) {
	statePath := PairingStatePath(filepath.Join(t.TempDir(), "console.secret"))
	if err := securefile.CreateExclusive(statePath, []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistent(bytesOf(0x43), statePath); err == nil {
		t.Fatal("invalid replay state was accepted")
	}
}

func TestAPIRequiresCookieAndMatchingOriginProof(t *testing.T) {
	auth, err := New(make([]byte, secretBytes))
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	firstCookie, firstProof := pairSession(t, handler, auth)
	_, secondProof := pairSession(t, handler, auth)

	for _, test := range []struct {
		name   string
		cookie *http.Cookie
		proof  string
		want   int
	}{
		{name: "cookie and proof", cookie: firstCookie, proof: firstProof, want: http.StatusNoContent},
		{name: "cookie only", cookie: firstCookie, want: http.StatusUnauthorized},
		{name: "other session proof", cookie: firstCookie, proof: secondProof, want: http.StatusUnauthorized},
		{name: "proof only", proof: firstProof, want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
			if test.cookie != nil {
				request.AddCookie(test.cookie)
			}
			if test.proof != "" {
				request.Header.Set(ProofHeader, test.proof)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("response = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestPairingRejectsExpiredMalformedAndWrongSecretTokens(t *testing.T) {
	current := time.Unix(1000, 0).UTC()
	auth, err := NewWithClock(bytesOf(0x11), func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.Handler(http.NotFoundHandler())
	token, err := auth.PairingToken()
	if err != nil {
		t.Fatal(err)
	}
	current = current.Add(pairingLifetime)

	wrongSecret, err := NewWithClock(bytesOf(0x22), func() time.Time { return time.Unix(1000, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	wrongToken, err := wrongSecret.PairingToken()
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "expired", token: token},
		{name: "missing", token: ""},
		{name: "malformed", token: "pair.not-a-token"},
		{name: "invalid signature encoding", token: "pair.nonce.4102444800.!"},
		{name: "wrong installation secret", token: wrongToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newPairingRequest(test.token))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
			if len(response.Result().Cookies()) != 0 {
				t.Fatal("rejected pairing created a session cookie")
			}
		})
	}
}

func TestExpiredAndRevokedSessionsRejectOriginProof(t *testing.T) {
	current := time.Unix(1000, 0).UTC()
	auth, err := NewWithClock(bytesOf(0x33), func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	expiredCookie, expiredProof := pairSession(t, handler, auth)
	current = current.Add(sessionLifetime)
	expired := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	expired.AddCookie(expiredCookie)
	expired.Header.Set(ProofHeader, expiredProof)
	expiredResponse := httptest.NewRecorder()
	handler.ServeHTTP(expiredResponse, expired)
	if expiredResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expired session status = %d", expiredResponse.Code)
	}

	current = current.Add(time.Second)
	revokedCookie, revokedProof := pairSession(t, handler, auth)
	auth.RevokeSessions()
	revoked := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	revoked.AddCookie(revokedCookie)
	revoked.Header.Set(ProofHeader, revokedProof)
	revokedResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokedResponse, revoked)
	if revokedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status = %d", revokedResponse.Code)
	}
}

func TestSessionCarriesPairingTimeAndRevocationGeneration(t *testing.T) {
	auth, err := New(make([]byte, secretBytes))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	auth.now = func() time.Time { return now }
	var session Session
	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ = SessionFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	cookie, proof := pairSession(t, handler, auth)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	request.AddCookie(cookie)
	request.Header.Set(ProofHeader, proof)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !session.IssuedAt.Equal(now) ||
		!session.PairedAt.Equal(now) || !session.ExpiresAt.Equal(now.Add(sessionLifetime)) ||
		session.RevocationGeneration != 1 ||
		!auth.ValidateRecentPairing(request.WithContext(contextWithSession(request.Context(), session))) {
		t.Fatalf("session = %+v, status = %d", session, response.Code)
	}

	auth.RevokeSessions()
	revoked := httptest.NewRecorder()
	handler.ServeHTTP(revoked, request)
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session returned %d", revoked.Code)
	}
}

func TestUnauthenticatedRequestExplainsPairing(t *testing.T) {
	auth, err := New(make([]byte, secretBytes))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	auth.Handler(http.NotFoundHandler()).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/api/v1/session", nil),
	)
	if response.Code != http.StatusUnauthorized ||
		!strings.Contains(response.Body.String(), "multirunner console open") {
		t.Fatalf("unexpected response: %d %q", response.Code, response.Body.String())
	}
}

func TestSecretRoundTrip(t *testing.T) {
	path := SecretPath(t.TempDir() + string(filepath.Separator) + "history.db")
	first, err := EnsureSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("loaded secret differs")
	}
	rotated, err := RotateSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(rotated) {
		t.Fatal("rotation did not change the secret")
	}
}

func TestEnsureSecretRejectsPermissiveExistingSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.secret")
	original := []byte(strings.Repeat("a", 43) + "\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureSecret(path); !errors.Is(err, securefile.ErrInsecurePermissions) {
		t.Fatalf("permissive secret error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(original) {
		t.Fatal("permissive secret was silently rewritten")
	}
}

func TestSecretRotationInvalidatesOldSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.secret")
	oldSecret, err := EnsureSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	oldAuth, err := New(oldSecret)
	if err != nil {
		t.Fatal(err)
	}
	oldHandler := oldAuth.Handler(http.NotFoundHandler())
	cookie, proof := pairSession(t, oldHandler, oldAuth)
	newSecret, err := RotateSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	newAuth, err := New(newSecret)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	request.AddCookie(cookie)
	request.Header.Set(ProofHeader, proof)
	response := httptest.NewRecorder()
	newAuth.Handler(http.NotFoundHandler()).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("old session after rotation returned %d", response.Code)
	}
}

func pairSession(t *testing.T, handler http.Handler, auth *Authenticator) (*http.Cookie, string) {
	t.Helper()
	token, err := auth.PairingToken()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newPairingRequest(token))
	return pairingCredentials(t, response)
}

func newPairingRequest(token string) *http.Request {
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:9092/auth/pair",
		strings.NewReader(`{"token":`+strconv.Quote(token)+`}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1:9092")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	return request
}

func pairingCredentials(t *testing.T, response *httptest.ResponseRecorder) (*http.Cookie, string) {
	t.Helper()
	var session, proof *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		switch cookie.Name {
		case sessionCookie:
			session = cookie
		case pairingProofCookie:
			proof = cookie
		}
	}
	if session == nil || !session.HttpOnly || proof == nil || proof.HttpOnly || proof.Value == "" {
		t.Fatalf("unexpected pairing cookies: %#v", response.Result().Cookies())
	}
	return session, proof.Value
}

func contextWithSession(ctx context.Context, session Session) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, session)
}

func bytesOf(value byte) []byte {
	return []byte(strings.Repeat(string([]byte{value}), secretBytes))
}
