package servicehost

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"github.com/kardianos/service"
)

func TestLaunchdPlistRendersFailureOnlyRestartAndOutput(t *testing.T) {
	var rendered bytes.Buffer
	data := struct {
		*service.Config
		Path              string
		StandardOutPath   string
		StandardErrorPath string
	}{
		Config: &service.Config{
			Name:             launchdServiceName,
			Arguments:        []string{"run", "--config", "/etc/multirunner/config.yaml"},
			WorkingDirectory: "/etc/multirunner",
		},
		Path:              "/usr/local/bin/multirunner",
		StandardOutPath:   "/var/log/multirunner.out.log",
		StandardErrorPath: "/var/log/multirunner.err.log",
	}
	if err := template.Must(template.New("launchd").Parse(launchdPlist)).Execute(&rendered, data); err != nil {
		t.Fatal(err)
	}
	plist := rendered.String()
	decoder := xml.NewDecoder(strings.NewReader(plist))
	for {
		if _, err := decoder.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("launchd plist is not well-formed XML: %v", err)
		}
	}
	keepAlive := "<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>"
	if !strings.Contains(plist, keepAlive) {
		t.Fatalf("launchd failure-only restart policy is malformed:\n%s", plist)
	}
	if strings.Contains(plist, "<key>SuccessfulExit</key>\n\t\t<true/>") {
		t.Fatal("clean exits would restart")
	}
	throttle := "<key>ThrottleInterval</key>\n\t<integer>" + strconv.Itoa(launchdThrottle) + "</integer>"
	if launchdThrottle <= 0 || launchdThrottle > 60 || !strings.Contains(plist, throttle) {
		t.Fatalf("launchd throttle is not bounded to 1 through 60 seconds:\n%s", plist)
	}
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if !strings.Contains(plist, "<key>"+key+"</key>\n\t<string>/var/log/multirunner.out.log</string>") {
			t.Errorf("%s does not use the managed output file", key)
		}
	}
}

func TestConfigureLaunchdArtifactsIsIdempotentAndPreservesLogs(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	if err := configureLaunchdArtifacts(launchdServiceName, paths); err != nil {
		t.Fatal(err)
	}
	logPath := launchdLogPath(launchdServiceName, paths)
	if err := os.WriteFile(logPath, []byte("existing output\n"), launchdLogMode); err != nil {
		t.Fatal(err)
	}
	if err := configureLaunchdArtifacts(launchdServiceName, paths); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(logged) != "existing output\n" {
		t.Fatalf("idempotent install changed existing logs: %q", logged)
	}
	policy, err := os.ReadFile(launchdPolicyPath(launchdServiceName, paths))
	if err != nil {
		t.Fatal(err)
	}
	if string(policy) != renderNewsyslogPolicy(launchdServiceName, paths) {
		t.Fatalf("installed policy differs from rendered policy:\n%s", policy)
	}
	if runtime.GOOS != "windows" {
		assertPermissions(t, launchdPolicyPath(launchdServiceName, paths), launchdPolicyMode)
		assertPermissions(t, logPath, launchdLogMode)
	}
}

func TestNewsyslogPolicyBoundsSizeAndCountAndSignalsAfterRotation(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	policy := renderNewsyslogPolicy(launchdServiceName, paths)
	fields := strings.Fields(strings.TrimPrefix(policy, launchdPolicyMarker))
	if len(fields) != 9 {
		t.Fatalf("newsyslog fields = %q, want 9", fields)
	}
	if fields[0] != launchdLogPath(launchdServiceName, paths) ||
		fields[2] != "640" ||
		fields[3] != strconv.Itoa(launchdRotationCount) ||
		fields[4] != strconv.Itoa(launchdRotationSizeKB) ||
		fields[5] != "*" ||
		fields[6] != launchdNewsyslogFlags ||
		fields[7] != launchdPIDPath(launchdServiceName, paths) ||
		fields[8] != strconv.Itoa(launchdHangupSignal) {
		t.Fatalf("unexpected newsyslog policy fields: %q", fields)
	}
	if launchdRotationCount != 5 || launchdRotationSizeKB != 1024 {
		t.Fatalf("rotation bounds = count %d, size %d KB", launchdRotationCount, launchdRotationSizeKB)
	}
}

func TestCleanupLaunchdArtifactsWaitsForUninstallAndIsIdempotent(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	if err := configureLaunchdArtifacts(launchdServiceName, paths); err != nil {
		t.Fatal(err)
	}
	if _, err := registerLaunchdProcess(launchdServiceName, paths, 123); err != nil {
		t.Fatal(err)
	}
	policyPath := launchdPolicyPath(launchdServiceName, paths)
	if err := cleanupLaunchdArtifacts(launchdServiceName, paths); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(policyPath); err != nil {
		t.Fatalf("cleanup removed policy while service is installed: %v", err)
	}
	if err := os.Remove(launchdDaemonPath(launchdServiceName, paths)); err != nil {
		t.Fatal(err)
	}
	if err := cleanupLaunchdArtifacts(launchdServiceName, paths); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(policyPath); !os.IsNotExist(err) {
		t.Fatalf("policy remains after uninstall: %v", err)
	}
	if _, err := os.Stat(launchdPIDPath(launchdServiceName, paths)); !os.IsNotExist(err) {
		t.Fatalf("PID file remains after uninstall: %v", err)
	}
	if err := cleanupLaunchdArtifacts(launchdServiceName, paths); err != nil {
		t.Fatalf("second cleanup failed: %v", err)
	}
}

func TestCleanupLaunchdArtifactsRefusesUnmanagedPolicy(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	if err := os.Remove(launchdDaemonPath(launchdServiceName, paths)); err != nil {
		t.Fatal(err)
	}
	policyPath := launchdPolicyPath(launchdServiceName, paths)
	if err := os.WriteFile(policyPath, []byte("# administrator policy\n"), launchdPolicyMode); err != nil {
		t.Fatal(err)
	}
	if err := cleanupLaunchdArtifacts(launchdServiceName, paths); err == nil {
		t.Fatal("cleanup removed an unmanaged policy")
	}
	if _, err := os.Stat(policyPath); err != nil {
		t.Fatalf("unmanaged policy was removed: %v", err)
	}
}

func TestRegisterLaunchdProcessDoesNotRemoveSuccessorPID(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	cleanupFirst, err := registerLaunchdProcess(launchdServiceName, paths, 123)
	if err != nil {
		t.Fatal(err)
	}
	cleanupSecond, err := registerLaunchdProcess(launchdServiceName, paths, 456)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupFirst(); err != nil {
		t.Fatal(err)
	}
	pidPath := launchdPIDPath(launchdServiceName, paths)
	pid, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(pid)) != "456" {
		t.Fatalf("successor PID = %q, want 456", pid)
	}
	if err := cleanupSecond(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("PID file remains after cleanup: %v", err)
	}
}

func TestLaunchdArtifactPathsRejectUnsafeNames(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	for _, name := range []string{"", "../multirunner", "multi/runner", ".hidden"} {
		if err := configureLaunchdArtifacts(name, paths); err == nil {
			t.Errorf("configure accepted unsafe service name %q", name)
		}
		if err := cleanupLaunchdArtifacts(name, paths); err == nil {
			t.Errorf("cleanup accepted unsafe service name %q", name)
		}
	}
}

func TestConfigureLaunchdArtifactsRejectsNonRegularLog(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	if err := os.Mkdir(launchdLogPath(launchdServiceName, paths), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := configureLaunchdArtifacts(launchdServiceName, paths); err == nil {
		t.Fatal("non-regular log destination was accepted")
	}
}

func TestConfigureLaunchdArtifactsDoesNotInstallPolicyAfterPIDCleanupFailure(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	if err := os.Mkdir(launchdPIDPath(launchdServiceName, paths), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := configureLaunchdArtifacts(launchdServiceName, paths); err == nil {
		t.Fatal("non-regular PID file was accepted")
	}
	if _, err := os.Stat(launchdPolicyPath(launchdServiceName, paths)); !os.IsNotExist(err) {
		t.Fatalf("policy installed after earlier configuration failure: %v", err)
	}
}

func TestConfigureLaunchdArtifactsRejectsInvalidDirectoriesAndDaemon(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, paths launchdArtifactPaths)
	}{
		{
			name: "missing log directory",
			mutate: func(t *testing.T, paths launchdArtifactPaths) {
				t.Helper()
				if err := os.Remove(paths.logDirectory); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "policy path is not a directory",
			mutate: func(t *testing.T, paths launchdArtifactPaths) {
				t.Helper()
				if err := os.Remove(paths.policyDirectory); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.policyDirectory, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "daemon definition is missing",
			mutate: func(t *testing.T, paths launchdArtifactPaths) {
				t.Helper()
				if err := os.Remove(launchdDaemonPath(launchdServiceName, paths)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "daemon definition is not regular",
			mutate: func(t *testing.T, paths launchdArtifactPaths) {
				t.Helper()
				daemonPath := launchdDaemonPath(launchdServiceName, paths)
				if err := os.Remove(daemonPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(daemonPath, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := newLaunchdTestPaths(t)
			test.mutate(t, paths)
			if err := configureLaunchdArtifacts(launchdServiceName, paths); err == nil {
				t.Fatal("invalid artifact layout was accepted")
			}
		})
	}
}

func TestConfigureLaunchdArtifactsRefusesUnmanagedPolicy(t *testing.T) {
	paths := newLaunchdTestPaths(t)
	policyPath := launchdPolicyPath(launchdServiceName, paths)
	if err := os.WriteFile(policyPath, []byte("# administrator policy\n"), launchdPolicyMode); err != nil {
		t.Fatal(err)
	}
	if err := configureLaunchdArtifacts(launchdServiceName, paths); err == nil {
		t.Fatal("unmanaged policy was replaced")
	}
	content, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# administrator policy\n" {
		t.Fatalf("unmanaged policy changed: %q", content)
	}
}

func TestCleanupLaunchdArtifactsRejectsNonRegularDaemonAndPolicy(t *testing.T) {
	t.Run("daemon", func(t *testing.T) {
		paths := newLaunchdTestPaths(t)
		daemonPath := launchdDaemonPath(launchdServiceName, paths)
		if err := os.Remove(daemonPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(daemonPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := cleanupLaunchdArtifacts(launchdServiceName, paths); err == nil {
			t.Fatal("non-regular daemon definition was accepted")
		}
	})
	t.Run("policy", func(t *testing.T) {
		paths := newLaunchdTestPaths(t)
		if err := os.Remove(launchdDaemonPath(launchdServiceName, paths)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(launchdPolicyPath(launchdServiceName, paths), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := cleanupLaunchdArtifacts(launchdServiceName, paths); err == nil {
			t.Fatal("non-regular policy was removed")
		}
	})
}

func TestRegisterLaunchdProcessValidatesInputs(t *testing.T) {
	tests := []struct {
		name    string
		service string
		pid     int
		mutate  func(t *testing.T, paths launchdArtifactPaths)
	}{
		{name: "invalid service name", service: "../multirunner", pid: 1},
		{name: "zero process id", service: launchdServiceName, pid: 0},
		{name: "negative process id", service: launchdServiceName, pid: -1},
		{
			name:    "invalid runtime directory",
			service: launchdServiceName,
			pid:     1,
			mutate: func(t *testing.T, paths launchdArtifactPaths) {
				t.Helper()
				if err := os.Remove(paths.pidDirectory); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:    "non-regular process id destination",
			service: launchdServiceName,
			pid:     1,
			mutate: func(t *testing.T, paths launchdArtifactPaths) {
				t.Helper()
				if err := os.Mkdir(launchdPIDPath(launchdServiceName, paths), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := newLaunchdTestPaths(t)
			if test.mutate != nil {
				test.mutate(t, paths)
			}
			if _, err := registerLaunchdProcess(test.service, paths, test.pid); err == nil {
				t.Fatal("invalid process registration was accepted")
			}
		})
	}
}

func newLaunchdTestPaths(t *testing.T) launchdArtifactPaths {
	t.Helper()
	root := t.TempDir()
	paths := launchdArtifactPaths{
		logDirectory:    filepath.Join(root, "log"),
		policyDirectory: filepath.Join(root, "newsyslog.d"),
		daemonDirectory: filepath.Join(root, "LaunchDaemons"),
		pidDirectory:    filepath.Join(root, "run"),
	}
	for _, directory := range []string{paths.logDirectory, paths.policyDirectory, paths.daemonDirectory, paths.pidDirectory} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(launchdDaemonPath(launchdServiceName, paths), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return paths
}

func assertPermissions(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want.Perm() {
		t.Errorf("%s permissions = %o, want %o", path, info.Mode().Perm(), want.Perm())
	}
}
