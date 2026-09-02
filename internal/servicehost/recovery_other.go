//go:build !windows && !linux && !darwin

package servicehost

// ConfigureRecovery is handled by the installed service definition off Windows.
func ConfigureRecovery(string) error {
	return nil
}

// ResetRecovery has no persistent native counter on this platform.
func ResetRecovery(string, string) error {
	return nil
}

func cleanupRecoveryArtifacts() error {
	return nil
}
