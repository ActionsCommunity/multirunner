//go:build windows

package securefile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// Owners are indexed by the exact directory containing the protected file.
// A binding never grants trust to sibling directories, descendants, or files
// directly in an ancestor, even though that file's parent chain is checked.
var installationOwners = struct {
	sync.RWMutex
	owners map[string]string
}{owners: make(map[string]string)}

type installationBindingStore interface {
	read(directory string) (string, error)
	create(directory, value string) (string, error)
}

// ProvisionInstallationOwner records a machine-protected, immutable binding.
// The caller must first securely read the operator marker. Only that actual
// operator's elevated token can provision; LocalSystem cannot self-authenticate
// an operator from a file in the directory it is trying to trust.
func ProvisionInstallationOwner(directory, operator string) error {
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !canProvisionInstallationOwner(current.User.Sid, operator, windows.GetCurrentProcessToken().IsElevated()) {
		return fmt.Errorf("%w: installation owner must be recorded by the elevated console operator", ErrInsecurePermissions)
	}
	return provisionInstallationOwner(directory, operator, registryBindingStore{})
}

// ProvisionCredentialOwner authenticates an explicitly selected credential and
// its directory as the same actual elevated operator before recording a binding.
// Unlike a console directory, a credential directory has no operator marker.
func ProvisionCredentialOwner(path, operator string) error {
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !canProvisionInstallationOwner(current.User.Sid, operator, windows.GetCurrentProcessToken().IsElevated()) {
		return fmt.Errorf("%w: credential owner must be recorded by the elevated console operator", ErrInsecurePermissions)
	}
	return provisionCredentialOwner(path, operator, registryBindingStore{}, validateCredentialFile)
}

func provisionCredentialOwner(path, operator string, store installationBindingStore, check func(string) error) error {
	if err := check(path); err != nil {
		return fmt.Errorf("validate configured OAuth token before recording its directory owner: %w", err)
	}
	return provisionInstallationOwner(filepath.Dir(path), operator, store)
}

// BootstrapCredentialOwner authenticates a configured credential directory
// independently and requires the console installation's already-bound operator.
func BootstrapCredentialOwner(path, operator string) error {
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !current.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return nil
	}
	return bootstrapCredentialOwner(path, operator, registryBindingStore{}, validateCredentialFile)
}

func bootstrapCredentialOwner(path, operator string, store installationBindingStore, check func(string) error) error {
	if operator == "" {
		return fmt.Errorf("%w: configured OAuth token requires a trusted console operator", ErrInsecurePermissions)
	}
	if _, err := bootstrapInstallationOwnerForOperator(filepath.Dir(path), operator, store); err != nil {
		return err
	}
	return check(path)
}

func validateCredentialFile(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(absolute), filepath.Clean(resolved)) {
		return fmt.Errorf("%w: configured OAuth token must use a plain, non-aliased path", ErrInsecurePermissions)
	}
	return Check(path)
}

func canProvisionInstallationOwner(current *windows.SID, operator string, elevated bool) bool {
	return elevated && current != nil &&
		!current.IsWellKnown(windows.WinLocalSystemSid) && current.String() == operator
}

func provisionInstallationOwner(directory, operator string, store installationBindingStore) error {
	canonical, err := canonicalInstallationDirectory(directory)
	if err != nil {
		return err
	}
	sid, err := windows.StringToSid(operator)
	if err != nil || sid.String() != operator || sid.IsWellKnown(windows.WinLocalSystemSid) {
		return fmt.Errorf("%w: invalid installation operator SID", ErrInsecurePermissions)
	}
	value := canonical + "\n" + operator
	existing, err := store.create(canonical, value)
	if err != nil {
		return fmt.Errorf("record installation owner (run service install/start as the elevated console operator): %w", err)
	}
	if existing != value {
		return fmt.Errorf("%w: installation owner binding already exists with different contents", ErrInsecurePermissions)
	}
	return nil
}

// BootstrapInstallationOwner authenticates the owner independently of the
// installation's files. Ordinary foreground operators keep the existing policy.
func BootstrapInstallationOwner(directory string) (string, error) {
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	if !current.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return "", nil
	}
	return bootstrapInstallationOwner(directory, registryBindingStore{})
}

func bootstrapInstallationOwner(directory string, store installationBindingStore) (string, error) {
	return bootstrapInstallationOwnerForOperator(directory, "", store)
}

func bootstrapInstallationOwnerForOperator(directory, expectedOperator string, store installationBindingStore) (string, error) {
	canonical, err := canonicalInstallationDirectory(directory)
	if err != nil {
		return "", err
	}
	value, err := store.read(canonical)
	if err != nil {
		return "", fmt.Errorf("read trusted installation owner (run service install/start as the elevated console operator): %w", err)
	}
	boundDirectory, operator, ok := strings.Cut(value, "\n")
	sid, sidErr := windows.StringToSid(operator)
	if !ok || boundDirectory != canonical || sidErr != nil ||
		sid.String() != operator || sid.IsWellKnown(windows.WinLocalSystemSid) {
		return "", fmt.Errorf("%w: invalid installation owner binding", ErrInsecurePermissions)
	}
	if expectedOperator != "" && operator != expectedOperator {
		return "", fmt.Errorf("%w: configured credential directory is bound to a different operator", ErrInsecurePermissions)
	}
	installationOwners.Lock()
	defer installationOwners.Unlock()
	if previous, exists := installationOwners.owners[canonical]; exists && previous != operator {
		return "", fmt.Errorf("%w: installation owner binding changed", ErrInsecurePermissions)
	}
	installationOwners.owners[canonical] = operator
	return operator, nil
}

func installationOwnerForPath(path string, current, fallback *windows.SID) *windows.SID {
	if !current.IsWellKnown(windows.WinLocalSystemSid) {
		return fallback
	}
	directory, err := canonicalInstallationDirectory(filepath.Dir(path))
	if err != nil {
		return fallback
	}
	installationOwners.RLock()
	operator := installationOwners.owners[directory]
	installationOwners.RUnlock()
	sid, err := windows.StringToSid(operator)
	if err != nil {
		return fallback
	}
	return sid
}

func canonicalInstallationDirectory(directory string) (string, error) {
	// GetFullPathName (used by filepath.Abs) removes trailing dots and spaces.
	// Reject those aliases before normalization can hide them.
	for _, component := range strings.Split(strings.ReplaceAll(directory, "/", `\`), `\`) {
		if component != "." && component != ".." &&
			(strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ")) {
			return "", fmt.Errorf("%w: ambiguous installation directory", ErrInsecurePermissions)
		}
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	volume := filepath.VolumeName(absolute)
	if len(volume) != 2 || volume[1] != ':' ||
		!((volume[0] >= 'A' && volume[0] <= 'Z') || (volume[0] >= 'a' && volume[0] <= 'z')) {
		return "", fmt.Errorf("%w: installation requires a local drive path", ErrInsecurePermissions)
	}
	for _, component := range strings.Split(absolute[len(volume)+1:], `\`) {
		if strings.ContainsAny(component, ":\"<>|?*\x00\r\n") ||
			strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return "", fmt.Errorf("%w: ambiguous installation directory", ErrInsecurePermissions)
		}
	}
	return strings.ToLower(filepath.Clean(absolute)), nil
}

func installationBindingName(directory string) string {
	digest := sha256.Sum256([]byte(directory))
	return hex.EncodeToString(digest[:])
}
