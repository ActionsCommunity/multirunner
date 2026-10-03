// Package cache is a self-hosted GitHub Actions cache server implementing the
// v2 (twirp/JSON) CacheService protocol, so actions/cache stores blobs on this
// host instead of round-tripping to GitHub's Azure backend. The protocol and
// storage behavior are ported from falcondev-oss/github-actions-cache-server.
package cache

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"google.golang.org/protobuf/encoding/protowire"
)

// Server is the running cache HTTP server.
type Server struct {
	store          *store
	advertiseURL   string
	accessToken    string
	upstream       *url.URL
	upstreamClient *http.Client
	proxy          *httputil.ReverseProxy
	httpSrv        *http.Server
	logger         *slog.Logger
	gitBundlerRepo string
	gitBundler     func(ctx context.Context, repoSlug string, w io.Writer) error
	gcInterval     time.Duration // 0 disables the GC sweep
	gcMaxAge       time.Duration // 0 disables age-based eviction
	gcMaxBytes     int64         // 0 disables size-based eviction
}

type scopeContextKey struct{}

// SetGitBundler enables the dotgit-cache endpoint for one configured repository:
// GET /gitmirror/{owner}/{repo} streams a git bundle of that repo's host mirror.
func (s *Server) SetGitBundler(repoSlug string, f func(ctx context.Context, repoSlug string, w io.Writer) error) {
	s.gitBundlerRepo = strings.TrimSuffix(repoSlug, ".git")
	s.gitBundler = f
}

// New builds the cache server and opens its store.
func New(ctx context.Context, cfg config.Cache, logger *slog.Logger) (*Server, error) {
	if cfg.Storage != "" && cfg.Storage != "filesystem" {
		return nil, fmt.Errorf("cache storage %q not implemented (filesystem only)", cfg.Storage)
	}
	dbPath := filepath.Join(cfg.Path, "cache.db")
	blobRoot := filepath.Join(cfg.Path, "blobs")
	st, err := openStore(ctx, dbPath, blobRoot)
	if err != nil {
		return nil, err
	}

	accessToken := cfg.AccessToken
	if accessToken == "" {
		var err error
		accessToken, err = randomToken()
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("cache access token: %w", err)
		}
	} else if strings.ContainsAny(accessToken, "/?#") || url.PathEscape(accessToken) != accessToken {
		st.Close()
		return nil, fmt.Errorf("cache access token must be URL path safe")
	}

	s := &Server{
		store:        st,
		advertiseURL: strings.TrimRight(cfg.AdvertiseURL, "/"),
		accessToken:  accessToken,
		upstreamClient: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger.With("component", "cache"),
	}
	if cfg.GCIntervalSec > 0 {
		s.gcInterval = time.Duration(cfg.GCIntervalSec) * time.Second
	}
	if cfg.MaxAgeDays > 0 {
		s.gcMaxAge = time.Duration(cfg.MaxAgeDays) * 24 * time.Hour
	}
	if cfg.MaxSizeGB > 0 {
		s.gcMaxBytes = int64(cfg.MaxSizeGB) << 30
	}

	if cfg.Upstream != "" {
		up, err := url.Parse(cfg.Upstream)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("cache upstream url: %w", err)
		}
		if up.Host == "" ||
			(up.Scheme != "https" && !(up.Scheme == "http" && isLoopbackHost(up.Hostname()))) {
			st.Close()
			return nil, fmt.Errorf("cache upstream must use https, except for loopback test endpoints")
		}
		s.upstream = up
		s.proxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(up)
				pr.Out.Host = up.Host
			},
		}
	}

	s.httpSrv = &http.Server{Addr: cfg.Listen, Handler: s.routes()}
	return s, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// AdvertiseURL is the base URL runners should use to reach this cache.
func (s *Server) AdvertiseURL() string {
	if s.advertiseURL == "" {
		return ""
	}
	return s.advertiseURL + s.accessPrefix()
}

// RedactedAdvertiseURL is safe to log.
func (s *Server) RedactedAdvertiseURL() string {
	if s.advertiseURL == "" {
		return ""
	}
	return s.advertiseURL + "/_mr/<redacted>"
}

func (s *Server) accessPrefix() string {
	if s.accessToken == "" {
		return ""
	}
	return "/_mr/" + s.accessToken
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	const svc = "/twirp/github.actions.results.api.v1.CacheService/"
	protected := "/_mr/{token}"

	// Modern Actions clients normalize ACTIONS_RESULTS_URL to its origin for
	// Twirp control-plane calls. Root cache routes therefore require a valid
	// Actions bearer token, while signed data-plane URLs retain the private
	// path token.
	mux.HandleFunc("POST "+svc+"CreateCacheEntry", s.requireCacheBearer(s.handleCreateCacheEntry))
	mux.HandleFunc("POST "+svc+"GetCacheEntryDownloadURL", s.requireCacheBearer(s.handleGetDownloadURL))
	mux.HandleFunc("POST "+svc+"FinalizeCacheEntryUpload", s.requireCacheBearer(s.handleFinalize))
	mux.HandleFunc(svc, s.requireCacheBearer(s.handleUnsupportedCacheService))

	mux.HandleFunc("POST "+protected+svc+"CreateCacheEntry", s.protect(s.requireCacheBearer(s.handleCreateCacheEntry)))
	mux.HandleFunc("POST "+protected+svc+"GetCacheEntryDownloadURL", s.protect(s.requireCacheBearer(s.handleGetDownloadURL)))
	mux.HandleFunc("POST "+protected+svc+"FinalizeCacheEntryUpload", s.protect(s.requireCacheBearer(s.handleFinalize)))
	mux.HandleFunc(protected+svc, s.protect(s.requireCacheBearer(s.handleUnsupportedCacheService)))
	mux.HandleFunc("PUT "+protected+"/devstoreaccount1/upload/{id}", s.protect(s.handleUploadPut))
	mux.HandleFunc("PUT "+protected+"/upload/{id}", s.protect(s.handleUploadPut))
	mux.HandleFunc("GET "+protected+"/download/{id}", s.protect(s.handleDownload))
	mux.HandleFunc("GET "+protected+"/gitmirror/{owner}/{repo}", s.protect(s.handleGitBundle))
	mux.HandleFunc(protected+"/", s.protect(s.handleCatchAll))
	mux.HandleFunc("/twirp/", s.handleCatchAll)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", http.NotFound)
	return s.logRequests(mux)
}

func (s *Server) requireCacheBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		info := parseScopes(token)
		if token == "" || token == auth || len(info.scopes) == 0 || info.repoID == "" {
			httpError(w, http.StatusUnauthorized, "missing or invalid cache token")
			return
		}
		if !s.validateRuntimeToken(r.Context(), token) {
			httpError(w, http.StatusUnauthorized, "cache token authentication failed")
			return
		}
		ctx := context.WithValue(r.Context(), scopeContextKey{}, info)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) validateRuntimeToken(ctx context.Context, token string) bool {
	if s.upstream == nil {
		s.logger.Warn("cache token validation unavailable: upstream is not configured")
		return false
	}
	runID, jobID, ok := parseResultsBackendIDs(token)
	if !ok {
		s.logger.Warn("cache token validation rejected: missing Actions.Results scope")
		return false
	}
	target := s.upstream.JoinPath(
		"twirp",
		"github.actions.results.api.v1.ArtifactService",
		"ListArtifacts",
	)
	target.RawQuery = ""
	target.ForceQuery = false
	target.Fragment = ""

	var body []byte
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendString(body, runID)
	body = protowire.AppendTag(body, 2, protowire.BytesType)
	body = protowire.AppendString(body, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		s.logger.Warn("build cache token validation request", "err", err)
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("Accept", "application/protobuf")

	resp, err := s.upstreamClient.Do(req)
	if err != nil {
		s.logger.Warn("cache token validation request failed", "err", err)
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode != http.StatusOK {
		s.logger.Warn("cache token validation rejected", "status", resp.StatusCode)
		return false
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/protobuf" {
		s.logger.Warn("cache token validation rejected: unexpected content type",
			"content_type", resp.Header.Get("Content-Type"))
		return false
	}
	return true
}

func (s *Server) handleUnsupportedCacheService(w http.ResponseWriter, _ *http.Request) {
	httpError(w, http.StatusNotFound, "unsupported cache operation")
}

func (s *Server) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("token") != s.accessToken {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// handleGitBundle streams a git bundle of the requested repo's host mirror
// (dotgit-cache). The repo name may carry a ".git" suffix.
func (s *Server) handleGitBundle(w http.ResponseWriter, r *http.Request) {
	if s.gitBundler == nil {
		http.Error(w, "git cache not enabled", http.StatusNotFound)
		return
	}
	slug := r.PathValue("owner") + "/" + strings.TrimSuffix(r.PathValue("repo"), ".git")
	if slug != s.gitBundlerRepo {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if err := s.gitBundler(r.Context(), slug, w); err != nil {
		s.logger.Error("git bundle", "repo", slug, "err", err)
		// headers may already be sent; nothing more to do.
	}
}

// logRequests logs each incoming request so cache traffic is observable.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.logger.Info("request", "method", r.Method, "path", s.redactPath(r.URL.Path))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) redactPath(path string) string {
	if prefix := s.accessPrefix(); strings.HasPrefix(path, prefix) {
		return "/_mr/<redacted>" + strings.TrimPrefix(path, prefix)
	}
	return path
}

// runGC periodically evicts stale/oversized cache entries until ctx is cancelled.
func (s *Server) runGC(ctx context.Context) {
	if s.gcInterval == 0 || (s.gcMaxAge == 0 && s.gcMaxBytes == 0) {
		return
	}
	t := time.NewTicker(s.gcInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var olderThan int64
			if s.gcMaxAge > 0 {
				olderThan = time.Now().Add(-s.gcMaxAge).UnixMilli()
			}
			n, err := s.store.evict(ctx, olderThan, s.gcMaxBytes)
			if err != nil {
				s.logger.Warn("cache gc failed", "err", err)
			} else if n > 0 {
				s.logger.Info("cache gc evicted entries", "count", n)
			}
		}
	}
}

// Start runs the server until ctx is cancelled, then shuts it down gracefully.
func (s *Server) Start(ctx context.Context) error {
	go s.runGC(ctx)
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("cache server listening", "addr", s.httpSrv.Addr, "advertise", s.RedactedAdvertiseURL())
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutCtx)
		return s.store.Close()
	case err := <-errCh:
		s.store.Close()
		return err
	}
}

// ---- twirp CacheService handlers ----

func (s *Server) handleCreateCacheEntry(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scopeOrError(w, r)
	if !ok {
		return
	}
	var body struct {
		Key     string `json:"key"`
		Version string `json:"version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	write := writeScope(sc.scopes)
	if write == "" {
		httpError(w, http.StatusForbidden, "no scope with write permission")
		return
	}
	id, created, err := s.store.createUpload(r.Context(), body.Key, body.Version, write, sc.repoID)
	if err != nil {
		s.serverError(w, "createUpload", err)
		return
	}
	if !created {
		writeJSON(w, map[string]any{"ok": false})
		return
	}
	writeJSON(w, map[string]any{
		"ok":                true,
		"signed_upload_url": fmt.Sprintf("%s/devstoreaccount1/upload/%s", s.baseURL(r), s.signID("upload", strconv.FormatInt(id, 10))),
	})
}

// baseURL returns the scheme+host the client used to reach this server, so the
// data-plane (upload/download) URLs we hand back are reachable by that same
// client — the configured advertise host (e.g. host.docker.internal) is not
// resolvable from a VM (which reaches us via the SLIRP alias) or from a
// container reached via the host gateway. Falls back to the advertise URL.
func (s *Server) baseURL(r *http.Request) string {
	if r.Host == "" {
		return s.AdvertiseURL()
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host + s.accessPrefix()
}

func (s *Server) handleGetDownloadURL(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scopeOrError(w, r)
	if !ok {
		return
	}
	var body struct {
		Key         string   `json:"key"`
		Version     string   `json:"version"`
		RestoreKeys []string `json:"restore_keys"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	keys := append([]string{body.Key}, body.RestoreKeys...)
	entry, found, err := s.store.matchEntry(r.Context(), keys, body.Version, scopeStringsByPermission(sc.scopes), sc.repoID)
	if err != nil {
		s.serverError(w, "matchEntry", err)
		return
	}
	if !found {
		writeJSON(w, map[string]any{"ok": false})
		return
	}
	writeJSON(w, map[string]any{
		"ok":                  true,
		"signed_download_url": fmt.Sprintf("%s/download/%s", s.baseURL(r), s.signID("download", entry.ID)),
		"matched_key":         entry.Key,
	})
}

func (s *Server) handleFinalize(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scopeOrError(w, r)
	if !ok {
		return
	}
	var body struct {
		Key     string `json:"key"`
		Version string `json:"version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	write := writeScope(sc.scopes)
	if write == "" {
		httpError(w, http.StatusForbidden, "no scope with write permission")
		return
	}
	id, err := s.store.completeUpload(r.Context(), body.Key, body.Version, write, sc.repoID)
	switch {
	case errors.Is(err, errNoParts), errors.Is(err, errPartMismatch):
		httpError(w, http.StatusBadRequest, err.Error())
		return
	case isNoRows(err):
		httpError(w, http.StatusNotFound, "upload not found")
		return
	case err != nil:
		s.serverError(w, "completeUpload", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "entry_id": id})
}

// ---- Azure block upload data plane ----

func (s *Server) handleUploadPut(w http.ResponseWriter, r *http.Request) {
	idStr, ok := s.verifySignedID("upload", r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusForbidden, "invalid upload signature")
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid upload id")
		return
	}
	q := r.URL.Query()
	if q.Get("comp") == "blocklist" {
		// Block-list commit: the runner sends the ordered block ids; we track
		// part counts independently, so just acknowledge.
		w.Header().Set("x-ms-request-id", newRequestID())
		w.WriteHeader(http.StatusCreated)
		return
	}

	chunkIndex := 0
	if bid := q.Get("blockid"); bid != "" {
		idx, ok := chunkIndexFromBlockID(bid)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid block id")
			return
		}
		chunkIndex = idx
	}
	if err := s.store.uploadPart(r.Context(), id, chunkIndex, r.Body); err != nil {
		s.serverError(w, "uploadPart", err)
		return
	}
	w.Header().Set("x-ms-request-id", newRequestID())
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := s.verifySignedID("download", r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusForbidden, "invalid download signature")
		return
	}
	rc, err := s.store.openDownload(r.Context(), id)
	if errors.Is(err, os.ErrNotExist) {
		httpError(w, http.StatusNotFound, "cache entry not found")
		return
	}
	if err != nil {
		s.serverError(w, "openDownload", err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (s *Server) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	if s.proxy == nil {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if prefix := s.accessPrefix(); strings.HasPrefix(r.URL.Path, prefix+"/") {
		clone := r.Clone(r.Context())
		clone.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if clone.URL.Path == "" {
			clone.URL.Path = "/"
		}
		r = clone
	}
	s.logger.Debug("proxying to upstream", "method", r.Method, "path", r.URL.Path)
	s.proxy.ServeHTTP(w, r)
}

// ---- auth / scopes ----

type scopeInfo struct {
	scopes []scopeEntry
	repoID string
}

type scopeEntry struct {
	Scope      string `json:"Scope"`
	Permission int    `json:"Permission"`
}

// scopeOrError returns scopes authenticated by requireCacheBearer.
func (s *Server) scopeOrError(w http.ResponseWriter, r *http.Request) (scopeInfo, bool) {
	info, ok := r.Context().Value(scopeContextKey{}).(scopeInfo)
	if !ok {
		httpError(w, http.StatusUnauthorized, "cache token was not authenticated")
		return scopeInfo{}, false
	}
	return info, true
}

// parseScopes decodes the JWT payload and reads the `ac` (cache scopes) and
// `repository_id` claims. requireCacheBearer authenticates the token before
// these unverified decoded values are trusted.
func parseScopes(token string) scopeInfo {
	var info scopeInfo
	if token == "" {
		return info
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return info
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return info
	}
	var claims struct {
		AC           string `json:"ac"`
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return info
	}
	info.repoID = claims.RepositoryID
	if claims.AC != "" {
		_ = json.Unmarshal([]byte(claims.AC), &info.scopes)
	}
	return info
}

func parseResultsBackendIDs(token string) (runID, jobID string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", false
	}
	var claims struct {
		Scope string `json:"scp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", false
	}
	for _, scope := range strings.Fields(claims.Scope) {
		segments := strings.Split(scope, ":")
		if len(segments) == 3 && segments[0] == "Actions.Results" &&
			segments[1] != "" && segments[2] != "" {
			return segments[1], segments[2], true
		}
	}
	return "", "", false
}

func writeScope(scopes []scopeEntry) string {
	for _, sc := range scopes {
		if sc.Permission >= 2 {
			return sc.Scope
		}
	}
	return ""
}

func scopeStringsByPermission(scopes []scopeEntry) []string {
	sorted := make([]scopeEntry, len(scopes))
	copy(sorted, scopes)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Permission > sorted[j].Permission })
	out := make([]string, len(sorted))
	for i, sc := range sorted {
		out[i] = sc.Scope
	}
	return out
}

// ---- helpers ----

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

func (s *Server) serverError(w http.ResponseWriter, op string, err error) {
	s.logger.Error("cache server error", "op", op, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func newRequestID() string {
	id, err := randomID()
	if err != nil {
		return "00000000"
	}
	return strconv.FormatInt(id, 16)
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func (s *Server) signID(kind, id string) string {
	exp := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(s.accessToken))
	_, _ = io.WriteString(mac, kind+"\n"+id+"\n"+exp)
	return id + "." + exp + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifySignedID(kind, signed string) (string, bool) {
	parts := strings.Split(signed, ".")
	if len(parts) != 3 {
		return "", false
	}
	id, exp, gotHex := parts[0], parts[1], parts[2]
	until, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > until {
		return "", false
	}
	got, err := hex.DecodeString(gotHex)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(s.accessToken))
	_, _ = io.WriteString(mac, kind+"\n"+id+"\n"+exp)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return "", false
	}
	return id, true
}
