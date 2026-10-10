//go:build !windows

package securefile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func CreateExclusive(path string, data []byte) error {
	if err := validateParentChain(path); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("create secret file handle")
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict secret file: %w", err)
	}
	if err := validateOpenFile(file); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write secret file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync secret file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close secret file: %w", err)
	}
	ok = true
	return nil
}

func Replace(path string, data []byte) error {
	if err := Check(path); err != nil {
		return err
	}
	if err := validateParentChain(path); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".securefile-*.tmp")
	if err != nil {
		return fmt.Errorf("create replacement secret: %w", err)
	}
	tempPath := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict replacement secret: %w", err)
	}
	if err := validateOpenFile(file); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write replacement secret: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync replacement secret: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close replacement secret: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace secret file: %w", err)
	}
	ok = true
	return nil
}

func Read(path string) ([]byte, error) {
	if err := validateParentChain(path); err != nil {
		return nil, err
	}
	file, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := validateOpenFile(file); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	return data, nil
}

func Check(path string) error {
	if err := validateParentChain(path); err != nil {
		return err
	}
	file, err := openNoFollow(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return validateOpenFile(file)
}

func openNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%w: symbolic links are not allowed", ErrInsecurePermissions)
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open secret file handle")
	}
	return file, nil
}

func validateOpenFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: secret is not a regular file", ErrInsecurePermissions)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: mode is %04o, want no group/world bits", ErrInsecurePermissions, perm)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return fmt.Errorf("%w: secret must have exactly one filesystem link", ErrInsecurePermissions)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%w: secret owner UID %d does not match expected UID %d", ErrInsecurePermissions, stat.Uid, os.Geteuid())
	}
	return nil
}

func validateParentChain(path string) error {
	parent, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	expected := uint32(os.Geteuid())
	for {
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: parent path %q is not a plain directory", ErrInsecurePermissions, parent)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != expected && stat.Uid != 0) {
			return fmt.Errorf("%w: parent path %q owner UID does not match expected UID %d or root", ErrInsecurePermissions, parent, expected)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return nil
		}
		parent = next
	}
}
