//go:build !windows

package consoleipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func Endpoint(databasePath string) (string, error) {
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(filepath.Clean(absolute)))
	directory := filepath.Join(
		filepath.Dir(absolute),
		".multirunner-console-"+hex.EncodeToString(digest[:8]),
	)
	endpoint := filepath.Join(directory, "control.sock")
	if len(endpoint) >= 104 {
		directory = filepath.Join("/tmp", ".multirunner-console-"+hex.EncodeToString(digest[:]))
		endpoint = filepath.Join(directory, "control.sock")
	}
	return endpoint, nil
}

func PrepareEndpointIdentity(string, bool) error {
	return nil
}

func listenEndpoint(endpoint string, _ string) (net.Listener, func() error, error) {
	directory := filepath.Dir(endpoint)
	if err := ensurePrivateDirectory(directory); err != nil {
		return nil, nil, err
	}
	if err := removeOwnedStaleSocket(endpoint); err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(endpoint)
		return nil, nil, err
	}
	return listener, func() error {
		err := os.Remove(endpoint)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}, nil
}

func dialEndpoint(ctx context.Context, endpoint string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", endpoint)
}

func ensurePrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("console pairing socket directory must be a private plain directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("console pairing socket directory owner does not match the service identity")
	}
	return nil
}

func removeOwnedStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) ||
		info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refusing to replace unsafe console pairing socket %q", path)
	}
	return os.Remove(path)
}
