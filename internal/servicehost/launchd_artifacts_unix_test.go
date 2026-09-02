//go:build !windows

package servicehost

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigureLaunchdArtifactsRejectsSymlinkLog(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	target := filepath.Join(t.TempDir(), "target.log")
	if err := os.WriteFile(target, nil, launchdLogMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, launchdLogPath(launchdServiceName, paths)); err != nil {
		t.Fatal(err)
	}
	if err := configureLaunchdArtifacts(launchdServiceName, paths); err == nil {
		t.Fatal("symlink log destination was accepted")
	}
}

func TestConfigureLaunchdArtifactsRejectsWritableDaemonDefinition(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	daemonPath := launchdDaemonPath(launchdServiceName, paths)
	if err := os.Chmod(daemonPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := configureLaunchdArtifacts(launchdServiceName, paths); err == nil {
		t.Fatal("group-writable launchd definition was accepted")
	}
}
