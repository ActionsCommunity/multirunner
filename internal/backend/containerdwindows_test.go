package backend

import (
	"reflect"
	"strings"
	"testing"
)

func TestContainerdLaunchArgsControls(t *testing.T) {
	b := &containerdBackend{isolation: "process"}
	req := LaunchRequest{
		Name:             "runner",
		Image:            "runner:latest",
		EncodedJITConfig: "jit",
		Container: ContainerSettings{
			CPUCount:    2,
			MemoryBytes: 4_294_967_296,
		},
		Env: map[string]string{"Z": "last", "A": "first"},
	}
	got := b.launchArgs(req, req.Env)
	want := []string{
		"run", "-d", "--name", "runner", "--isolation", "process",
		"--cpus", "2",
		"--memory", "4294967296",
		"-e", "JIT_CONFIG=jit",
		"-e", "A=first",
		"-e", "Z=last",
		"--label", "multirunner=true",
		"--label", "multirunner.name=runner",
		"runner:latest",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("launchArgs = %v, want %v", got, want)
	}
}

func TestContainerdLaunchArgsOmittedControls(t *testing.T) {
	b := &containerdBackend{isolation: "hyperv"}
	got := b.launchArgs(LaunchRequest{Name: "runner", Image: "runner:latest"}, nil)
	for _, flag := range []string{"--cpus", "--memory", "--memory-swap", "--dns"} {
		if containsArgument(got, flag) {
			t.Errorf("launchArgs contains omitted flag %q: %v", flag, got)
		}
	}
}

func TestContainerdLaunchRejectsUnsupportedControlsBeforeRun(t *testing.T) {
	b := &containerdBackend{}
	cases := map[string]struct {
		req  LaunchRequest
		want string
	}{
		"memory swap": {
			req:  LaunchRequest{Name: "runner", Container: ContainerSettings{MemoryBytes: 1024, MemorySwapBytes: 1024}},
			want: "memory swap is unsupported",
		},
		"DNS": {
			req:  LaunchRequest{Name: "runner", Container: ContainerSettings{DNS: []string{"1.1.1.1"}}},
			want: "DNS is unsupported",
		},
		"invalid DNS": {
			req:  LaunchRequest{Name: "runner", Container: ContainerSettings{DNS: []string{"resolver.example.com"}}},
			want: "must be an IP address",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := b.Launch(t.Context(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Launch error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func containsArgument(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
