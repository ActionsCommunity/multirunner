//go:build !windows

package update

import (
	"fmt"
	"os"
	"syscall"
)

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func openArtifactFile(path string) (*os.File, error) {
	return os.Open(path)
}

func validateRegularFile(_ *os.File, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return ErrIntegrity
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return fmt.Errorf("%w: update artifact must have exactly one link", ErrIntegrity)
	}
	return nil
}
