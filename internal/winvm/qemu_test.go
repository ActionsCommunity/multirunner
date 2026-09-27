package winvm

import (
	"strings"
	"testing"
)

func TestDetectAccel(t *testing.T) {
	restore := kvmAvailable
	t.Cleanup(func() { kvmAvailable = restore })
	kvmAvailable = func() bool { return true }
	cases := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "kvm"},
		{"windows", "amd64", "whpx"},
		{"darwin", "amd64", "hvf"},
		{"linux", "arm64", "tcg"},
		{"windows", "arm64", "tcg"},
		{"darwin", "arm64", "tcg"},
		{"plan9", "amd64", "tcg"},
	}
	for _, tc := range cases {
		if got := DetectAccel(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("DetectAccel(%s, %s) = %s, want %s", tc.goos, tc.goarch, got, tc.want)
		}
	}
	kvmAvailable = func() bool { return false }
	if got := DetectAccel("linux", "amd64"); got != "tcg" {
		t.Errorf("DetectAccel without a usable /dev/kvm = %s, want tcg", got)
	}
}

func TestOverlayCreateArgs(t *testing.T) {
	got := strings.Join(OverlayCreateArgs("golden.qcow2", "job.qcow2"), " ")
	for _, want := range []string{"create", "-b golden.qcow2", "job.qcow2", "-f qcow2"} {
		if !strings.Contains(got, want) {
			t.Errorf("overlay args missing %q: %s", want, got)
		}
	}
}

func TestQEMUArgs(t *testing.T) {
	got := strings.Join(QEMUArgs(LaunchOpts{
		Overlay: "job.qcow2", JITISOPath: "jit.iso", MemMB: 8192, CPUs: 4, Accel: "kvm",
		Name: "runner-1", QMPAddr: "127.0.0.1:4567", PIDFile: "runner-1.pid",
	}), " ")
	for _, want := range []string{
		"-accel kvm", "-m 8192", "-smp 4",
		"file=job.qcow2,if=none,id=osdisk,format=qcow2", "ide-hd,drive=osdisk",
		"file=jit.iso,media=cdrom,readonly=on",
		"-name runner-1", "-pidfile runner-1.pid",
		"-qmp tcp:127.0.0.1:4567,server=on,wait=off",
		"-display none", "e1000", "-serial",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("qemu args missing %q:\n%s", want, got)
		}
	}
}

func TestQEMUArgsDefaults(t *testing.T) {
	got := strings.Join(QEMUArgs(LaunchOpts{Overlay: "j.qcow2"}), " ")
	if !strings.Contains(got, "-m 4096") || !strings.Contains(got, "-smp 2") || !strings.Contains(got, "-accel tcg") {
		t.Errorf("defaults not applied: %s", got)
	}
	if strings.Contains(got, "media=cdrom") {
		t.Error("no cdrom expected when JITISOPath empty")
	}
}
