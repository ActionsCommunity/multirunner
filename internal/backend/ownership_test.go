package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

func TestDockerOwnedRunnerStoreUsesCompleteOwnershipBoundary(t *testing.T) {
	ownership := RunnerOwnership{
		Instance:   "host-a",
		Target:     "https://github.com/o/r",
		Pool:       "linux",
		ScaleSetID: 7,
	}

	ownedLabels := ownershipLabels(ownership)
	ownedLabels[labelName] = "runner-owned"
	ownedLabels[labelRunnerID] = "41"
	foreignLabels := make(map[string]string, len(ownedLabels))
	for key, value := range ownedLabels {
		foreignLabels[key] = value
	}
	foreignLabels[labelInstance] = "host-b"

	var filtersSeen map[string]map[string]bool
	var removed string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filtersSeen); err != nil {
				t.Errorf("decode filters: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[
				{"Id":"owned","Names":["/runner-owned"],"Labels":%s},
				{"Id":"foreign","Names":["/runner-foreign"],"Labels":%s}
			]`, mustJSON(t, ownedLabels), mustJSON(t, foreignLabels))
		case r.Method == http.MethodDelete:
			removed = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	cli, err := client.NewClientWithOpts(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithVersion("1.47"),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	store := &dockerBackend{cli: cli, name: "test"}
	got, err := store.ListOwnedRunners(t.Context(), ownership)
	if err != nil {
		t.Fatalf("ListOwnedRunners: %v", err)
	}
	if len(got) != 1 || got[0].ResourceID != "owned" || got[0].RunnerID != 41 {
		t.Fatalf("owned runners = %+v", got)
	}
	for key, value := range reconciliationLabels(ownership) {
		if !filtersSeen["label"][key+"="+value] {
			t.Errorf("ownership filter missing %s=%s: %v", key, value, filtersSeen)
		}
	}
	if err := store.RemoveOwnedRunner(t.Context(), got[0].ResourceID); err != nil {
		t.Fatalf("RemoveOwnedRunner: %v", err)
	}
	if !strings.HasSuffix(removed, "/containers/owned") {
		t.Fatalf("removed path = %q, want owned container", removed)
	}
}

func TestDockerLaunchPersistsRunnerOwnership(t *testing.T) {
	ownership := RunnerOwnership{
		Instance: "host-a", Target: "https://github.com/o/r", Pool: "linux", ScaleSetID: 7, RunnerID: 41,
	}
	var labels map[string]string
	var autoRemove bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
			var request struct {
				Labels     map[string]string `json:"Labels"`
				HostConfig struct {
					AutoRemove bool `json:"AutoRemove"`
				} `json:"HostConfig"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			labels = request.Labels
			autoRemove = request.HostConfig.AutoRemove
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"Id":"container-1","Warnings":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/container-1/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithVersion("1.47"),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	store := &dockerBackend{cli: cli, name: "test"}
	if _, err := store.Launch(t.Context(), LaunchRequest{
		Name: "runner-owned", Image: "runner:latest", Ownership: ownership,
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for key, value := range ownershipLabels(ownership) {
		if labels[key] != value {
			t.Errorf("label %s = %q, want %q", key, labels[key], value)
		}
	}
	if autoRemove {
		t.Fatal("scale-set container enabled auto-remove and cannot preserve cleanup state")
	}
}

func TestDockerOwnedRunnerRemovalToleratesMissingResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithVersion("1.47"),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	store := &dockerBackend{cli: cli, name: "test"}
	if err := store.RemoveOwnedRunner(t.Context(), "gone"); err != nil {
		t.Fatalf("RemoveOwnedRunner: %v", err)
	}
}

func TestDockerHandleStopsWithoutRemovingCleanupRecord(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithVersion("1.47"),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	handle := &dockerHandle{cli: cli, id: "runner", preserve: true}
	if err := handle.Kill(t.Context()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if got := strings.Join(methods, ","); got != http.MethodPost {
		t.Fatalf("methods = %q, want stop POST without DELETE", got)
	}
}

func TestDockerHandleRemovesUnownedPoolContainer(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithVersion("1.47"),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	handle := &dockerHandle{cli: cli, id: "runner"}
	if err := handle.Kill(t.Context()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if got := strings.Join(methods, ","); got != http.MethodPost+","+http.MethodDelete {
		t.Fatalf("methods = %q, want stop POST and removal DELETE", got)
	}
}

func TestDockerHandleForcesUnownedPoolRemovalAfterStopFailure(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodPost {
			http.Error(w, "stop failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithVersion("1.47"),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	handle := &dockerHandle{cli: cli, id: "runner"}
	if err := handle.Kill(t.Context()); err == nil {
		t.Fatal("Kill error = nil, want stop failure")
	}
	if got := strings.Join(methods, ","); got != http.MethodPost+","+http.MethodDelete {
		t.Fatalf("methods = %q, want stop POST and forced removal DELETE", got)
	}
}

func TestContainerdOwnedRunnerStoreRejectsForeignResource(t *testing.T) {
	ownership := RunnerOwnership{
		Instance:   "host-a",
		Target:     "https://github.com/o/r",
		Pool:       "windows",
		ScaleSetID: 9,
	}

	ownedLabels := ownershipLabels(ownership)
	ownedLabels[labelName] = "runner-owned"
	ownedLabels[labelRunnerID] = "51"
	foreignLabels := make(map[string]string, len(ownedLabels))
	for key, value := range ownedLabels {
		foreignLabels[key] = value
	}
	foreignLabels[labelPool] = "other-pool"

	var listArgs []string
	store := &containerdBackend{runCommand: func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "ps":
			listArgs = append([]string(nil), args...)
			return "owned\nforeign\n", nil
		case "inspect":
			if args[len(args)-1] == "owned" {
				return mustJSON(t, ownedLabels), nil
			}
			return mustJSON(t, foreignLabels), nil
		default:
			return "", fmt.Errorf("unexpected command %v", args)
		}
	}}

	got, err := store.ListOwnedRunners(t.Context(), ownership)
	if err != nil {
		t.Fatalf("ListOwnedRunners: %v", err)
	}
	if len(got) != 1 || got[0].ResourceID != "owned" || got[0].RunnerID != 51 {
		t.Fatalf("owned runners = %+v", got)
	}
	joined := strings.Join(listArgs, " ")
	for key, value := range reconciliationLabels(ownership) {
		if !strings.Contains(joined, "label="+key+"="+value) {
			t.Errorf("ownership filter missing %s=%s: %v", key, value, listArgs)
		}
	}

}

func TestRunnerOwnershipRejectsPartialBoundary(t *testing.T) {
	if err := (RunnerOwnership{}).Validate(); err != nil {
		t.Fatalf("zero ownership should be valid for pool mode: %v", err)
	}
	if err := (RunnerOwnership{Instance: "host-a"}).Validate(); err == nil {
		t.Fatal("partial ownership was accepted")
	}
	if err := (RunnerOwnership{
		Instance: "host-a", Target: "https://github.com/o/r", Pool: "linux", ScaleSetID: 7,
	}).Validate(); err != nil {
		t.Fatalf("complete ownership boundary was rejected: %v", err)
	}
}

func TestContainerdLaunchPersistsRunnerOwnership(t *testing.T) {
	ownership := RunnerOwnership{
		Instance: "host-a", Target: "https://github.com/o/r", Pool: "windows", ScaleSetID: 9, RunnerID: 51,
	}
	var launchArgs []string
	store := &containerdBackend{runCommand: func(_ context.Context, args ...string) (string, error) {
		launchArgs = append([]string(nil), args...)
		return "container-1", nil
	}}
	if _, err := store.Launch(t.Context(), LaunchRequest{
		Name: "runner-owned", Image: "runner:latest", Ownership: ownership,
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	joined := strings.Join(launchArgs, " ")
	for key, value := range ownershipLabels(ownership) {
		if !strings.Contains(joined, key+"="+value) {
			t.Errorf("launch label missing %s=%s: %v", key, value, launchArgs)
		}
	}
}

func TestContainerdLaunchCleansContainerAfterRunFailure(t *testing.T) {
	var commands []string
	store := &containerdBackend{runCommand: func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, strings.Join(args, " "))
		switch args[0] {
		case "run":
			return "", errors.New("task start failed")
		case "inspect":
			return mustJSON(t, map[string]string{
				labelManaged: "true",
				labelName:    "runner-failed",
			}), nil
		}
		return "", nil
	}}

	handle, err := store.Launch(t.Context(), LaunchRequest{
		Name: "runner-failed", Image: "runner:latest",
	})
	if err == nil || handle == nil {
		t.Fatalf("Launch = (%v, %v), want failure with cleanup handle", handle, err)
	}
	if err := handle.Kill(t.Context()); err != nil {
		t.Fatalf("cleanup handle Kill: %v", err)
	}
	if got := commands[len(commands)-2]; !strings.HasPrefix(got, "inspect ") {
		t.Fatalf("ownership check command = %q, want inspect", got)
	}
	if got := commands[len(commands)-1]; got != "rm -f runner-failed" {
		t.Fatalf("cleanup command = %q, want forced removal", got)
	}
}

func TestContainerdFailedLaunchDoesNotRemoveMismatchedContainer(t *testing.T) {
	var commands []string
	store := &containerdBackend{runCommand: func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, strings.Join(args, " "))
		if args[0] == "run" {
			return "", errors.New("name conflict")
		}
		return mustJSON(t, map[string]string{
			labelManaged: "true",
			labelName:    "different-runner",
		}), nil
	}}

	handle, err := store.Launch(t.Context(), LaunchRequest{
		Name: "runner-failed", Image: "runner:latest",
	})
	if err == nil || handle == nil {
		t.Fatalf("Launch = (%v, %v), want failure with cleanup handle", handle, err)
	}
	if err := handle.Kill(t.Context()); err == nil || !strings.Contains(err.Error(), "mismatched ownership") {
		t.Fatalf("cleanup error = %v, want ownership mismatch", err)
	}
	for _, command := range commands {
		if strings.HasPrefix(command, "rm ") {
			t.Fatalf("removed foreign container with command %q", command)
		}
	}
}

func TestContainerdOwnedRunnerRemovalToleratesMissingResource(t *testing.T) {
	store := &containerdBackend{runCommand: func(context.Context, ...string) (string, error) {
		return "", errors.New("no such container")
	}}
	if err := store.RemoveOwnedRunner(t.Context(), "gone"); err != nil {
		t.Fatalf("RemoveOwnedRunner: %v", err)
	}
}

func TestContainerdHandlePreservesStoppedResourceForConvergentCleanup(t *testing.T) {
	var commands []string
	store := &containerdBackend{runCommand: func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, strings.Join(args, " "))
		switch args[0] {
		case "wait":
			return "0", nil
		case "stop":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected command %v", args)
		}
	}}
	handle := &containerdHandle{b: store, name: "runner", id: "container-1", preserve: true}
	if code, err := handle.Wait(t.Context()); err != nil || code != 0 {
		t.Fatalf("Wait = (%d, %v)", code, err)
	}
	if err := handle.Kill(t.Context()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if got := strings.Join(commands, ","); got != "wait runner,stop runner" {
		t.Fatalf("commands = %q, want wait and stop without removal", got)
	}
}

func TestContainerdHandleRemovesUnownedPoolContainerAfterWait(t *testing.T) {
	var commands []string
	store := &containerdBackend{runCommand: func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, strings.Join(args, " "))
		switch args[0] {
		case "wait":
			return "0", nil
		case "rm":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected command %v", args)
		}
	}}
	handle := &containerdHandle{b: store, name: "runner", id: "container-1"}
	if code, err := handle.Wait(t.Context()); err != nil || code != 0 {
		t.Fatalf("Wait = (%d, %v)", code, err)
	}
	if got := strings.Join(commands, ","); got != "wait runner,rm -f runner" {
		t.Fatalf("commands = %q, want wait followed by removal", got)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}
