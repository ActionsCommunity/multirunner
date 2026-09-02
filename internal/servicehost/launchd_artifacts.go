package servicehost

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const (
	launchdServiceName     = "multirunner"
	launchdLogDirectory    = "/var/log"
	launchdPolicyDirectory = "/etc/newsyslog.d"
	launchdDaemonDirectory = "/Library/LaunchDaemons"
	launchdPIDDirectory    = "/var/run"
	launchdPolicyMarker    = "# Managed by multirunner.\n"
	launchdLogMode         = 0o640
	launchdPolicyMode      = 0o644
	launchdRotationCount   = 5
	launchdRotationSizeKB  = 1024
	launchdHangupSignal    = 1
	launchdNewsyslogFlags  = "Z"
)

var validLaunchdServiceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type launchdArtifactPaths struct {
	logDirectory    string
	policyDirectory string
	daemonDirectory string
	pidDirectory    string
}

var systemLaunchdArtifactPaths = launchdArtifactPaths{
	logDirectory:    launchdLogDirectory,
	policyDirectory: launchdPolicyDirectory,
	daemonDirectory: launchdDaemonDirectory,
	pidDirectory:    launchdPIDDirectory,
}

func configureLaunchdArtifacts(name string, paths launchdArtifactPaths) error {
	if err := validateLaunchdServiceName(name); err != nil {
		return err
	}
	for label, path := range map[string]string{
		"log":     paths.logDirectory,
		"policy":  paths.policyDirectory,
		"daemon":  paths.daemonDirectory,
		"runtime": paths.pidDirectory,
	} {
		if err := validateSecureDirectory(path); err != nil {
			return fmt.Errorf("validate %s directory: %w", label, err)
		}
	}
	if err := validateRegularFile(launchdDaemonPath(name, paths)); err != nil {
		return fmt.Errorf("validate installed launchd service: %w", err)
	}
	if err := ensureLogFile(launchdLogPath(name, paths)); err != nil {
		return fmt.Errorf("prepare launchd log: %w", err)
	}
	if err := removeProcessIDFile(name, paths, 0); err != nil {
		return err
	}
	policy := []byte(renderNewsyslogPolicy(name, paths))
	if err := ensureManagedFile(launchdPolicyPath(name, paths), policy, launchdPolicyMode, true); err != nil {
		return fmt.Errorf("install newsyslog policy: %w", err)
	}
	return nil
}

func cleanupLaunchdArtifacts(name string, paths launchdArtifactPaths) error {
	if err := validateLaunchdServiceName(name); err != nil {
		return err
	}
	if _, err := os.Lstat(launchdDaemonPath(name, paths)); err == nil {
		return validateRegularFile(launchdDaemonPath(name, paths))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect launchd service: %w", err)
	}
	if err := removeManagedPolicy(launchdPolicyPath(name, paths)); err != nil {
		return err
	}
	return removeProcessIDFile(name, paths, 0)
}

func registerLaunchdProcess(name string, paths launchdArtifactPaths, pid int) (func() error, error) {
	if err := validateLaunchdServiceName(name); err != nil {
		return nil, err
	}
	if pid <= 0 {
		return nil, fmt.Errorf("process id must be positive")
	}
	if err := validateSecureDirectory(paths.pidDirectory); err != nil {
		return nil, fmt.Errorf("validate runtime directory: %w", err)
	}
	content := []byte(strconv.Itoa(pid) + "\n")
	path := launchdPIDPath(name, paths)
	if err := ensureManagedFile(path, content, launchdPolicyMode, false); err != nil {
		return nil, fmt.Errorf("write launchd process id: %w", err)
	}
	return func() error {
		return removeProcessIDFile(name, paths, pid)
	}, nil
}

func renderNewsyslogPolicy(name string, paths launchdArtifactPaths) string {
	return fmt.Sprintf("%s%s root:admin %o %d %d * %s %s %d\n",
		launchdPolicyMarker,
		launchdLogPath(name, paths),
		launchdLogMode,
		launchdRotationCount,
		launchdRotationSizeKB,
		launchdNewsyslogFlags,
		launchdPIDPath(name, paths),
		launchdHangupSignal,
	)
}

func ensureLogFile(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		if err := os.Chmod(path, launchdLogMode); err != nil {
			return fmt.Errorf("set %s permissions: %w", path, err)
		}
		return validateFilePermissions(path, launchdLogMode)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, launchdLogMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := file.Chmod(launchdLogMode); err != nil {
		file.Close()
		return fmt.Errorf("set %s permissions: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return validateFilePermissions(path, launchdLogMode)
}

func ensureManagedFile(path string, content []byte, mode os.FileMode, requireMarker bool) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		if requireMarker {
			existing, readErr := os.ReadFile(path)
			if readErr != nil {
				return fmt.Errorf("read %s: %w", path, readErr)
			}
			if !strings.HasPrefix(string(existing), launchdPolicyMarker) {
				return fmt.Errorf("refuse to replace unmanaged file %s", path)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", path, err)
	}

	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if len(content) > 0 {
		if _, err := temp.Write(content); err != nil {
			temp.Close()
			return fmt.Errorf("write temporary file: %w", err)
		}
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return validateFilePermissions(path, mode)
}

func validateFilePermissions(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("verify %s: %w", path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != mode.Perm() {
		return fmt.Errorf("%s permissions are %o, want %o", path, info.Mode().Perm(), mode.Perm())
	}
	return nil
}

func removeManagedPolicy(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect newsyslog policy: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to remove non-regular newsyslog policy %s", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read newsyslog policy: %w", err)
	}
	if !strings.HasPrefix(string(content), launchdPolicyMarker) {
		return fmt.Errorf("refuse to remove unmanaged newsyslog policy %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove newsyslog policy: %w", err)
	}
	return nil
}

func removeProcessIDFile(name string, paths launchdArtifactPaths, expectedPID int) error {
	path := launchdPIDPath(name, paths)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect process id file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to remove non-regular process id file %s", path)
	}
	if expectedPID > 0 {
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read process id file: %w", err)
		}
		if strings.TrimSpace(string(content)) != strconv.Itoa(expectedPID) {
			return nil
		}
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove process id file: %w", err)
	}
	return nil
}

func validateLaunchdServiceName(name string) error {
	if !validLaunchdServiceName.MatchString(name) || filepath.Base(name) != name {
		return fmt.Errorf("invalid launchd service name %q", name)
	}
	return nil
}

func validateSecureDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group or world writable", path)
	}
	return nil
}

func validateRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group or world writable", path)
	}
	return nil
}

func launchdLogPath(name string, paths launchdArtifactPaths) string {
	return filepath.Join(paths.logDirectory, name+".out.log")
}

func launchdPolicyPath(name string, paths launchdArtifactPaths) string {
	return filepath.Join(paths.policyDirectory, name+".conf")
}

func launchdDaemonPath(name string, paths launchdArtifactPaths) string {
	return filepath.Join(paths.daemonDirectory, name+".plist")
}

func launchdPIDPath(name string, paths launchdArtifactPaths) string {
	return filepath.Join(paths.pidDirectory, name+".pid")
}
