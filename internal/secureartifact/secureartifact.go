// Package secureartifact protects local operational artifacts from unsafe
// filesystem aliases, ownership changes, and pathname replacement races.
package secureartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrInsecure = errors.New("artifact path is insecure")

func PrepareRoot(path string) (string, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve artifact root: %w", err)
	}
	_, statErr := os.Lstat(root)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return "", fmt.Errorf("inspect artifact root: %w", statErr)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create artifact root: %w", err)
	}
	if created {
		if err := protectRoot(root); err != nil {
			return "", fmt.Errorf("protect artifact root: %w", err)
		}
	}
	if err := validateRoot(root); err != nil {
		return "", fmt.Errorf("validate artifact root: %w", err)
	}
	return root, nil
}

func Open(root, path string) (*os.File, error) {
	if !Contains(root, path) {
		return nil, fmt.Errorf("%w: file is outside its artifact root", ErrInsecure)
	}
	file, err := openAtRoot(root, path)
	if err != nil {
		return nil, err
	}
	if err := validateFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func Contains(root, path string) bool {
	if filepath.Clean(filepath.Dir(path)) != filepath.Clean(root) {
		return false
	}
	return within(root, path)
}

func DigestAndRewind(ctx context.Context, file *os.File) (int64, string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, "", fmt.Errorf("rewind artifact: %w", err)
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			size += int64(count)
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, "", fmt.Errorf("hash artifact: %w", readErr)
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, "", fmt.Errorf("rewind artifact after hashing: %w", err)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != "" &&
		relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative)
}
