//go:build windows

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
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/GerardSmit/multirunner/internal/securefile"
)

func Endpoint(databasePath string) (string, error) {
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absolute))))
	return `\\.\pipe\multirunner-console-` + hex.EncodeToString(digest[:16]), nil
}

func PrepareEndpointIdentity(databasePath string, create bool) error {
	path := operatorIdentityPath(databasePath)
	if _, err := readOperatorSID(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Installations from releases before the operator identity existed are
	// upgraded by the next elevated service command, run by the operator.
	if !create && !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("read console operator identity: %w (run this command from an elevated terminal once to record you as the console operator)", os.ErrNotExist)
	}
	operator, err := currentUserSID()
	if err != nil {
		return err
	}
	if !create && operator.IsWellKnown(windows.WinLocalSystemSid) {
		return fmt.Errorf("read console operator identity: %w", os.ErrNotExist)
	}
	if err := securefile.CreateExclusive(path, []byte(operator.String()+"\n")); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create console operator identity: %w", err)
		}
		if _, readErr := readOperatorSID(path); readErr != nil {
			return readErr
		}
	}
	return nil
}

func listenEndpoint(
	endpoint string, databasePath string,
) (net.Listener, func() error, error) {
	operator, err := readOperatorSID(operatorIdentityPath(databasePath))
	if errors.Is(err, os.ErrNotExist) {
		operator, err = currentUserSID()
	}
	if err != nil {
		return nil, nil, err
	}
	securityDescriptor := "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;" +
		operator.String() + ")"
	listener, err := winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: securityDescriptor,
		InputBufferSize:    maxRequestBytes,
		OutputBufferSize:   maxResponseBytes,
	})
	return listener, func() error { return nil }, err
}

func dialEndpoint(ctx context.Context, endpoint string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, endpoint)
}

func operatorIdentityPath(databasePath string) string {
	return filepath.Join(filepath.Dir(databasePath), "console.operator")
}

func readOperatorSID(path string) (*windows.SID, error) {
	data, err := securefile.Read(path)
	if err != nil {
		return nil, err
	}
	sid, err := windows.StringToSid(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("parse console operator identity: %w", err)
	}
	return sid, nil
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current operator identity: %w", err)
	}
	return user.User.Sid, nil
}
