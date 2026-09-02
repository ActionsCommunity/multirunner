//go:build darwin

package servicehost

// ConfigureRecovery installs bounded rotation for launchd-owned runtime output.
func ConfigureRecovery(name string) error {
	return configureLaunchdArtifacts(name, systemLaunchdArtifactPaths)
}

// ResetRecovery has no persistent launchd failure counter.
func ResetRecovery(string, string) error {
	return nil
}

// RegisterLaunchdProcess creates the PID file newsyslog signals after rotation.
func RegisterLaunchdProcess(pid int) (func() error, error) {
	return registerLaunchdProcess(launchdServiceName, systemLaunchdArtifactPaths, pid)
}

func cleanupRecoveryArtifacts() error {
	return cleanupLaunchdArtifacts(launchdServiceName, systemLaunchdArtifactPaths)
}
