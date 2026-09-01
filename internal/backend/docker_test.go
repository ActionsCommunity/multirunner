package backend

import (
	"math"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestDockerLaunchConfigsLinuxControls(t *testing.T) {
	b := &dockerBackend{name: "docker-linux"}
	req := LaunchRequest{
		Name:             "runner",
		Image:            "runner:latest",
		EncodedJITConfig: "jit",
		CPUCount:         2,
		MemoryBytes:      4_294_967_296,
		MemorySwapBytes:  8_589_934_592,
		DNS:              []string{"1.1.1.1", "2001:db8::1"},
	}

	_, host, err := b.launchConfigs(req)
	if err != nil {
		t.Fatalf("launchConfigs: %v", err)
	}
	if host.NanoCPUs != 2_000_000_000 || host.CPUCount != 0 {
		t.Errorf("CPU resources = NanoCPUs %d, CPUCount %d", host.NanoCPUs, host.CPUCount)
	}
	if host.Memory != req.MemoryBytes || host.MemorySwap != req.MemorySwapBytes {
		t.Errorf("memory resources = %d/%d, want %d/%d", host.Memory, host.MemorySwap, req.MemoryBytes, req.MemorySwapBytes)
	}
	if len(host.DNS) != 2 || host.DNS[0] != "1.1.1.1" || host.DNS[1] != "2001:db8::1" {
		t.Errorf("DNS = %v", host.DNS)
	}
}

func TestDockerLaunchConfigsWindowsControls(t *testing.T) {
	b := &dockerBackend{name: "docker-windows", isolation: container.IsolationProcess}
	req := LaunchRequest{
		Name:        "runner",
		CPUCount:    4,
		MemoryBytes: 4_294_967_296,
		DNS:         []string{"10.0.0.10"},
	}

	_, host, err := b.launchConfigs(req)
	if err != nil {
		t.Fatalf("launchConfigs: %v", err)
	}
	if host.CPUCount != 4 || host.NanoCPUs != 0 {
		t.Errorf("CPU resources = CPUCount %d, NanoCPUs %d", host.CPUCount, host.NanoCPUs)
	}
	if host.Memory != req.MemoryBytes || host.MemorySwap != 0 {
		t.Errorf("memory resources = %d/%d", host.Memory, host.MemorySwap)
	}
	if len(host.DNS) != 1 || host.DNS[0] != "10.0.0.10" {
		t.Errorf("DNS = %v", host.DNS)
	}
	if host.Isolation != container.IsolationProcess {
		t.Errorf("Isolation = %q, want process", host.Isolation)
	}
}

func TestDockerLaunchConfigsOmittedControls(t *testing.T) {
	b := &dockerBackend{name: "docker-linux"}
	_, host, err := b.launchConfigs(LaunchRequest{Name: "runner"})
	if err != nil {
		t.Fatalf("launchConfigs: %v", err)
	}
	if host.NanoCPUs != 0 || host.CPUCount != 0 || host.Memory != 0 || host.MemorySwap != 0 || host.DNS != nil {
		t.Errorf("omitted controls changed Docker defaults: %+v", host.Resources)
	}
}

func TestDockerLaunchConfigsRejectsInvalidControls(t *testing.T) {
	cases := map[string]struct {
		backend string
		req     LaunchRequest
		want    string
	}{
		"negative CPU": {
			backend: "docker-linux",
			req:     LaunchRequest{CPUCount: -1},
			want:    "cpu count",
		},
		"overflowed CPU": {
			backend: "docker-linux",
			req:     LaunchRequest{CPUCount: math.MaxInt64/nanoCPUsPerCPU + 1},
			want:    "overflows",
		},
		"negative memory": {
			backend: "docker-linux",
			req:     LaunchRequest{MemoryBytes: -1},
			want:    "memory",
		},
		"swap without memory": {
			backend: "docker-linux",
			req:     LaunchRequest{MemorySwapBytes: 1024},
			want:    "requires",
		},
		"swap below memory": {
			backend: "docker-linux",
			req:     LaunchRequest{MemoryBytes: 2048, MemorySwapBytes: 1024},
			want:    "greater than or equal",
		},
		"hostname DNS": {
			backend: "docker-linux",
			req:     LaunchRequest{DNS: []string{"resolver.example.com"}},
			want:    "IP address",
		},
		"Windows swap": {
			backend: "docker-windows",
			req:     LaunchRequest{MemoryBytes: 1024, MemorySwapBytes: 1024},
			want:    "unsupported",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := &dockerBackend{name: tc.backend}
			_, _, err := b.launchConfigs(tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("launchConfigs error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
