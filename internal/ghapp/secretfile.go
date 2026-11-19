package ghapp

import (
	"log/slog"

	"github.com/GerardSmit/multirunner/internal/securefile"
)

// CheckOwnerOnly verifies that a credential file is protected by the
// platform-native owner policy.
func CheckOwnerOnly(path string) error {
	return securefile.Check(path)
}

func warnIfPermissive(path string) {
	if err := CheckOwnerOnly(path); err != nil {
		slog.Warn("credential file has unexpected access permissions; rewrite it before reuse",
			slog.String("path", path), slog.Any("error", err))
	}
}
