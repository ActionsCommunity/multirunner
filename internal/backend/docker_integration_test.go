package backend

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

const dockerIntegrationImage = "alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"

func dockerIntegrationBackend(t *testing.T) *dockerBackend {
	t.Helper()
	host := os.Getenv("MULTIRUNNER_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("set MULTIRUNNER_TEST_DOCKER_HOST to run")
	}
	be, err := newDockerBackend("docker-linux", host, "")
	if err != nil {
		t.Fatalf("newDockerBackend: %v", err)
	}
	t.Cleanup(func() {
		if err := be.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return be
}

// TestDockerPing connects to a real daemon when MULTIRUNNER_TEST_DOCKER_HOST is
// set (Docker or Podman docker-compat). Skipped otherwise.
func TestDockerPing(t *testing.T) {
	be := dockerIntegrationBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := be.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	t.Log("daemon reachable")
}

func TestDockerLinuxContainerControls(t *testing.T) {
	be := dockerIntegrationBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	osType, err := be.OSType(ctx)
	if err != nil {
		t.Fatalf("OSType: %v", err)
	}
	if osType != "linux" {
		t.Skipf("container controls integration test requires a Linux daemon, got %q", osType)
	}
	if err := be.EnsureImage(ctx, dockerIntegrationImage); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	settings := ContainerSettings{
		CPUCount:        1,
		MemoryBytes:     64 * 1024 * 1024,
		MemorySwapBytes: 128 * 1024 * 1024,
		DNS:             []string{"1.1.1.1", "8.8.8.8"},
	}
	req := LaunchRequest{
		Name:             fmt.Sprintf("multirunner-controls-%d", time.Now().UnixNano()),
		Image:            dockerIntegrationImage,
		EncodedJITConfig: "integration-test",
		Container:        settings,
	}
	containerConfig, hostConfig, err := be.launchConfigs(req)
	if err != nil {
		t.Fatalf("launchConfigs: %v", err)
	}
	created, err := be.cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, req.Name)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		err := be.cli.ContainerRemove(cleanupCtx, created.ID, container.RemoveOptions{Force: true})
		if err != nil && !client.IsErrNotFound(err) {
			t.Errorf("ContainerRemove(%s): %v", created.ID, err)
		}
	})

	inspected, err := be.cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if inspected.HostConfig == nil {
		t.Fatal("ContainerInspect returned no host configuration")
	}
	resources := inspected.HostConfig.Resources
	if resources.NanoCPUs != settings.CPUCount*nanoCPUsPerCPU {
		t.Errorf("NanoCPUs = %d, want %d", resources.NanoCPUs, settings.CPUCount*nanoCPUsPerCPU)
	}
	if resources.Memory != settings.MemoryBytes {
		t.Errorf("Memory = %d, want %d", resources.Memory, settings.MemoryBytes)
	}
	if resources.MemorySwap != settings.MemorySwapBytes {
		t.Errorf("MemorySwap = %d, want %d", resources.MemorySwap, settings.MemorySwapBytes)
	}
	if !slices.Equal(inspected.HostConfig.DNS, settings.DNS) {
		t.Errorf("DNS = %v, want %v", inspected.HostConfig.DNS, settings.DNS)
	}
}
