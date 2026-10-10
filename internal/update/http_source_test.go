package update

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHTTPSourceFetchesVersionedMetadataAndConsistentTarget(t *testing.T) {
	repository := newTestRepository(t)
	requests := []string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/repo/2.root.json":
			http.NotFound(w, r)
		case "/repo/timestamp.json":
			_, _ = w.Write(repository.timestamp)
		case "/repo/4.snapshot.json":
			_, _ = w.Write(repository.snapshot)
		case "/repo/5.targets.json":
			_, _ = w.Write(repository.targets)
		default:
			var targets Envelope[Targets]
			mustJSON(t, repository.targets, &targets)
			target := targets.Signed.Targets["multirunner_v1.2.0_windows_amd64.exe"]
			expected := "/repo/" + target.Hashes["sha256"] + ".multirunner_v1.2.0_windows_amd64.exe"
			if r.URL.Path != expected {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(repository.artifact)
		}
	}))
	defer server.Close()
	base, err := url.Parse(server.URL + "/repo/")
	if err != nil {
		t.Fatal(err)
	}
	source := &HTTPSource{base: base, client: server.Client()}
	bundle, err := source.Bundle(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(bundle.Timestamp) != string(repository.timestamp) ||
		string(bundle.Snapshot) != string(repository.snapshot) ||
		string(bundle.Targets) != string(repository.targets) {
		t.Fatal("metadata bundle differs from repository")
	}
	var targets Envelope[Targets]
	mustJSON(t, bundle.Targets, &targets)
	target := targets.Signed.Targets["multirunner_v1.2.0_windows_amd64.exe"]
	reader, err := source.OpenTarget(
		t.Context(), "multirunner_v1.2.0_windows_amd64.exe", target,
	)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(body) != string(repository.artifact) {
		t.Fatalf("target = %q, %v", body, err)
	}
	if strings.Join(requests, ",") !=
		"/repo/2.root.json,/repo/timestamp.json,/repo/4.snapshot.json,/repo/5.targets.json,"+
			"/repo/"+target.Hashes["sha256"]+".multirunner_v1.2.0_windows_amd64.exe" {
		t.Fatalf("requests = %v", requests)
	}
}

func TestHTTPSourceRejectsUnsafeConfigurationAndPaths(t *testing.T) {
	for _, value := range []string{
		"http://updates.example/repo",
		"https://user@example.com/repo",
		"https://updates.example/repo?channel=stable",
	} {
		if _, err := NewHTTPSource(value, nil); err == nil {
			t.Fatalf("unsafe URL %q accepted", value)
		}
	}
	source, err := NewHTTPSource("https://updates.example/repo/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.resolve("../secret"); err == nil {
		t.Fatal("escaping repository path accepted")
	}
	for _, value := range []string{"../target", "/target", `folder\target`, "a/../target"} {
		if err := validRepositoryPath(value); err == nil {
			t.Fatalf("unsafe path %q accepted", value)
		}
	}
}
