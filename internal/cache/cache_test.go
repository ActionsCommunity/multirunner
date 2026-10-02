package cache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	return newTestServerWithUpstream(t, "")
}

func newTestServerWithUpstream(t *testing.T, upstream string) (*Server, *httptest.Server) {
	t.Helper()
	cfg := config.Cache{
		Enabled:             true,
		Mode:                "local-server",
		Storage:             "filesystem",
		Path:                t.TempDir(),
		Listen:              "127.0.0.1:0",
		SkipTokenValidation: true,
		Upstream:            upstream,
	}
	s, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.routes())
	s.advertiseURL = ts.URL // signed URLs point back at the test server
	t.Cleanup(func() {
		ts.Close()
		s.store.Close()
	})
	return s, ts
}

func cacheBase(s *Server) string { return s.AdvertiseURL() }

func postJSON(t *testing.T, url string, body any) map[string]any {
	t.Helper()
	resp := postJSONRaw(t, url, body, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s: status %d: %s", url, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

func postJSONRaw(t *testing.T, url string, body any, bearer string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func testBearerToken(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	acJSON := `[{"Scope":"refs/heads/main","Permission":3}]`
	payloadObj := map[string]any{"ac": acJSON, "repository_id": "12345"}
	pb, err := json.Marshal(payloadObj)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(pb)
	return header + "." + payload + ".sig"
}

// blockID48 builds a 48-byte standard block id encoding the given chunk index.
func blockID48(index int) string {
	raw := []byte(fmt.Sprintf("%-36s%012d", "uuid", index))
	return base64.StdEncoding.EncodeToString(raw)
}

func TestCacheRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	const svc = "/twirp/github.actions.results.api.v1.CacheService/"
	key, version := "deps-abc", "v1"

	// 1. CreateCacheEntry
	create := postJSON(t, cacheBase(s)+svc+"CreateCacheEntry", map[string]any{"key": key, "version": version})
	if create["ok"] != true {
		t.Fatalf("CreateCacheEntry ok=%v", create["ok"])
	}
	uploadURL, _ := create["signed_upload_url"].(string)
	if uploadURL == "" {
		t.Fatal("no signed_upload_url")
	}

	// 2. Upload two chunks via Azure block PUTs, then commit the block list.
	chunk0 := []byte("hello-")
	chunk1 := []byte("world!")
	putChunk(t, uploadURL+"?blockid="+blockID48(0), chunk0)
	putChunk(t, uploadURL+"?blockid="+blockID48(1), chunk1)
	putChunk(t, uploadURL+"?comp=blocklist", nil)

	// 3. FinalizeCacheEntryUpload
	fin := postJSON(t, cacheBase(s)+svc+"FinalizeCacheEntryUpload", map[string]any{"key": key, "version": version})
	if fin["ok"] != true {
		t.Fatalf("Finalize ok=%v", fin["ok"])
	}

	// 4. GetCacheEntryDownloadURL (exact key)
	get := postJSON(t, cacheBase(s)+svc+"GetCacheEntryDownloadURL",
		map[string]any{"key": key, "version": version, "restore_keys": []string{}})
	if get["ok"] != true {
		t.Fatalf("Get ok=%v", get["ok"])
	}
	if get["matched_key"] != key {
		t.Errorf("matched_key=%v want %s", get["matched_key"], key)
	}
	dlURL, _ := get["signed_download_url"].(string)

	// 5. Download and verify concatenated bytes.
	got := httpGet(t, dlURL)
	if want := append(append([]byte{}, chunk0...), chunk1...); !bytes.Equal(got, want) {
		t.Errorf("download = %q, want %q", got, want)
	}
}

func TestCacheMissAndRestoreKeyPrefix(t *testing.T) {
	s, _ := newTestServer(t)
	const svc = "/twirp/github.actions.results.api.v1.CacheService/"

	// Miss: nothing stored.
	miss := postJSON(t, cacheBase(s)+svc+"GetCacheEntryDownloadURL",
		map[string]any{"key": "nope", "version": "v1"})
	if miss["ok"] != false {
		t.Fatalf("expected miss, got %v", miss)
	}

	// Store under key "deps-2024-01".
	stored := "deps-2024-01"
	create := postJSON(t, cacheBase(s)+svc+"CreateCacheEntry", map[string]any{"key": stored, "version": "v1"})
	putChunk(t, create["signed_upload_url"].(string)+"?blockid="+blockID48(0), []byte("data"))
	postJSON(t, cacheBase(s)+svc+"FinalizeCacheEntryUpload", map[string]any{"key": stored, "version": "v1"})

	// Restore-key prefix match: primary miss, restore_keys ["deps-"] should hit.
	got := postJSON(t, cacheBase(s)+svc+"GetCacheEntryDownloadURL",
		map[string]any{"key": "deps-2024-02", "version": "v1", "restore_keys": []string{"deps-"}})
	if got["ok"] != true {
		t.Fatalf("expected restore-key hit, got %v", got)
	}
	if got["matched_key"] != stored {
		t.Errorf("matched_key=%v want %s", got["matched_key"], stored)
	}

	// Version mismatch must miss even with same key.
	vm := postJSON(t, cacheBase(s)+svc+"GetCacheEntryDownloadURL",
		map[string]any{"key": stored, "version": "v2"})
	if vm["ok"] != false {
		t.Errorf("expected version-mismatch miss, got %v", vm)
	}
}

func TestCacheRejectsWrongAccessToken(t *testing.T) {
	_, ts := newTestServer(t)
	const svc = "/twirp/github.actions.results.api.v1.CacheService/"
	resp := postJSONRaw(t, ts.URL+"/_mr/wrong"+svc+"CreateCacheEntry", map[string]any{"key": "k", "version": "v"}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestStrictTokenValidationRejectsMissingBearer(t *testing.T) {
	s, ts := newTestServer(t)
	s.skipValidation = false
	const svc = "/twirp/github.actions.results.api.v1.CacheService/"
	resp := postJSONRaw(t, cacheBase(s)+svc+"CreateCacheEntry", map[string]any{"key": "k", "version": "v"}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	_ = ts
}

func TestRootCacheServiceUsesLocalStore(t *testing.T) {
	_, ts := newTestServer(t)
	const svc = "/twirp/github.actions.results.api.v1.CacheService/"
	resp := postJSONRaw(t, ts.URL+svc+"CreateCacheEntry",
		map[string]any{"key": "root-route", "version": "v1"}, testBearerToken(t))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	uploadURL, _ := out["signed_upload_url"].(string)
	if !strings.Contains(uploadURL, "/_mr/") {
		t.Fatalf("signed_upload_url = %q, want protected data-plane URL", uploadURL)
	}
}

func TestRootCacheServiceRejectsMissingBearer(t *testing.T) {
	_, ts := newTestServer(t)
	const svc = "/twirp/github.actions.results.api.v1.CacheService/"
	resp := postJSONRaw(t, ts.URL+svc+"CreateCacheEntry",
		map[string]any{"key": "root-route", "version": "v1"}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRootArtifactServiceProxiesUpstream(t *testing.T) {
	const path = "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact"
	var gotPath, gotHost, gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHost = r.Host
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	_, ts := newTestServerWithUpstream(t, upstream.URL)

	resp := postJSONRaw(t, ts.URL+path, map[string]any{"name": "artifact"}, "runtime-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if gotPath != path {
		t.Fatalf("upstream path = %q, want %q", gotPath, path)
	}
	wantHost := strings.TrimPrefix(upstream.URL, "http://")
	if gotHost != wantHost {
		t.Fatalf("upstream Host = %q, want %q", gotHost, wantHost)
	}
	if gotAuthorization != "Bearer runtime-token" {
		t.Fatalf("upstream Authorization = %q", gotAuthorization)
	}
}

func TestSignedDataPlaneRejectsBareID(t *testing.T) {
	s, _ := newTestServer(t)
	url := strings.TrimRight(cacheBase(s), "/") + "/devstoreaccount1/upload/123?blockid=" + blockID48(0)
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader("x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestSparsePartsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	const svc = "/twirp/github.actions.results.api.v1.CacheService/"
	create := postJSON(t, cacheBase(s)+svc+"CreateCacheEntry", map[string]any{"key": "sparse", "version": "v1"})
	putChunk(t, create["signed_upload_url"].(string)+"?blockid="+blockID48(0), []byte("a"))
	putChunk(t, create["signed_upload_url"].(string)+"?blockid="+blockID48(2), []byte("c"))

	resp := postJSONRaw(t, cacheBase(s)+svc+"FinalizeCacheEntryUpload", map[string]any{"key": "sparse", "version": "v1"}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}
}

func TestGitBundleLimitedToConfiguredRepo(t *testing.T) {
	s, _ := newTestServer(t)
	var got string
	s.SetGitBundler("octo/hello", func(_ context.Context, repoSlug string, w io.Writer) error {
		got = repoSlug
		_, _ = io.WriteString(w, "bundle")
		return nil
	})
	resp, err := http.Get(cacheBase(s) + "/gitmirror/octo/hello")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || got != "octo/hello" {
		t.Fatalf("allowed status=%d got=%q", resp.StatusCode, got)
	}
	resp, err = http.Get(cacheBase(s) + "/gitmirror/octo/other")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("forbidden status=%d, want 404", resp.StatusCode)
	}
}

func putChunk(t *testing.T, url string, body []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s: status %d: %s", url, resp.StatusCode, raw)
	}
}

func httpGet(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return b
}

func TestChunkIndexFromBlockID(t *testing.T) {
	// 48-byte standard format.
	if idx, ok := chunkIndexFromBlockID(blockID48(7)); !ok || idx != 7 {
		t.Errorf("48-byte: idx=%d ok=%v, want 7", idx, ok)
	}
	// 64-byte Docker Buildx format: uint32 index at offset 16.
	raw := make([]byte, 64)
	binary.BigEndian.PutUint32(raw[16:20], 42)
	if idx, ok := chunkIndexFromBlockID(base64.StdEncoding.EncodeToString(raw)); !ok || idx != 42 {
		t.Errorf("64-byte: idx=%d ok=%v, want 42", idx, ok)
	}
	// Garbage.
	if _, ok := chunkIndexFromBlockID("not-base64-!!!"); ok {
		t.Error("expected failure on garbage block id")
	}
}

func TestParseScopes(t *testing.T) {
	// Build an unsigned JWT with ac + repository_id claims.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	acJSON := `[{"Scope":"refs/heads/main","Permission":3},{"Scope":"refs/heads/dev","Permission":1}]`
	payloadObj := map[string]any{"ac": acJSON, "repository_id": "12345"}
	pb, _ := json.Marshal(payloadObj)
	payload := base64.RawURLEncoding.EncodeToString(pb)
	token := header + "." + payload + ".sig"

	info := parseScopes(token)
	if info.repoID != "12345" {
		t.Errorf("repoID=%q", info.repoID)
	}
	if w := writeScope(info.scopes); w != "refs/heads/main" {
		t.Errorf("writeScope=%q", w)
	}
	order := scopeStringsByPermission(info.scopes)
	if len(order) != 2 || order[0] != "refs/heads/main" {
		t.Errorf("order=%v", order)
	}
}
