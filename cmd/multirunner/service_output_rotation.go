package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type outputFileOpener func(string, os.FileMode) (*os.File, error)
type outputFileRebinder func(*os.File) error

type outputPathMove struct {
	source string
	target string
}

type outputPathTransaction struct {
	moves        []outputPathMove
	oldestBackup string
}

type rotatingOutput struct {
	path     string
	maxBytes int64
	archives int
	mode     os.FileMode
	open     outputFileOpener

	mu     sync.Mutex
	file   *os.File
	size   int64
	closed bool
}

func openRotatingOutput(path string, maxBytes int64, archives int, mode os.FileMode, open outputFileOpener) (*rotatingOutput, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("service output path must be absolute")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("service output size must be positive")
	}
	if archives < 1 {
		return nil, fmt.Errorf("service output archive count must be positive")
	}
	if open == nil {
		return nil, fmt.Errorf("service output opener is required")
	}
	if err := validateOutputDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	for index := 0; index <= archives; index++ {
		candidate := path
		if index > 0 {
			candidate = archivePath(path, index)
		}
		if err := validateOutputFile(candidate, mode, true); err != nil {
			return nil, err
		}
	}
	file, err := open(path, mode)
	if err != nil {
		return nil, err
	}
	if err := secureOutputFile(file, mode); err != nil {
		file.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat service output: %w", err)
	}
	return &rotatingOutput{
		path:     path,
		maxBytes: maxBytes,
		archives: archives,
		mode:     mode,
		open:     open,
		file:     file,
		size:     info.Size(),
	}, nil
}

func (output *rotatingOutput) RotateIfNeeded(rebind outputFileRebinder) (bool, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.closed {
		return false, os.ErrClosed
	}
	info, err := output.file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat service output before rotation: %w", err)
	}
	output.size = info.Size()
	if output.size < output.maxBytes {
		return false, nil
	}
	if err := output.rotate(rebind); err != nil {
		return false, err
	}
	return true, nil
}

func (output *rotatingOutput) Close() error {
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.closed {
		return nil
	}
	output.closed = true
	if err := output.file.Sync(); err != nil {
		output.file.Close()
		return fmt.Errorf("sync service output: %w", err)
	}
	if err := output.file.Close(); err != nil {
		return fmt.Errorf("close service output: %w", err)
	}
	return nil
}

func (output *rotatingOutput) rotate(rebind outputFileRebinder) error {
	for index := 0; index <= output.archives; index++ {
		candidate := output.path
		if index > 0 {
			candidate = archivePath(output.path, index)
		}
		if err := validateOutputFile(candidate, output.mode, index > 0); err != nil {
			return err
		}
	}
	replacement, tempPath, err := createReplacementOutput(output.path, output.mode)
	if err != nil {
		return err
	}
	defer func() {
		if replacement != nil {
			replacement.Close()
		}
	}()
	defer os.Remove(tempPath)
	if rebind == nil {
		return output.rotateOwnedFile(replacement, tempPath)
	}
	if err := output.file.Sync(); err != nil {
		return fmt.Errorf("sync service output before rotation: %w", err)
	}
	transaction, err := beginOutputPathTransaction(output.path, tempPath, output.archives)
	if err != nil {
		return err
	}
	if err := rebind(replacement); err != nil {
		if rollbackErr := transaction.Rollback(); rollbackErr != nil {
			return output.stopAfterRotationError(fmt.Errorf("%v; rollback service output paths: %w", err, rollbackErr))
		}
		return err
	}
	previous := output.file
	output.file = replacement
	replacement = nil
	output.size = 0
	closeErr := previous.Close()
	commitErr := transaction.Commit()
	if closeErr != nil {
		if commitErr != nil {
			return fmt.Errorf("close archived service output: %v; %w", closeErr, commitErr)
		}
		return fmt.Errorf("close archived service output: %w", closeErr)
	}
	return commitErr
}

func (output *rotatingOutput) rotateOwnedFile(replacement *os.File, tempPath string) error {
	if err := output.file.Sync(); err != nil {
		return fmt.Errorf("sync service output before rotation: %w", err)
	}
	if err := output.file.Close(); err != nil {
		return fmt.Errorf("close service output before rotation: %w", err)
	}
	output.file = nil
	if err := replacement.Close(); err != nil {
		output.closed = true
		return fmt.Errorf("close replacement service output: %w", err)
	}
	if err := rotateOutputPaths(output.path, tempPath, output.archives); err != nil {
		output.closed = true
		return err
	}
	file, err := output.open(output.path, output.mode)
	if err != nil {
		output.closed = true
		return fmt.Errorf("reopen service output after rotation: %w", err)
	}
	if err := secureOutputFile(file, output.mode); err != nil {
		file.Close()
		output.closed = true
		return err
	}
	output.file = file
	output.size = 0
	return nil
}

func rotateOutputPaths(path, replacement string, archives int) error {
	if err := os.Remove(archivePath(path, archives)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("prune oldest service output: %w", err)
	}
	for index := archives - 1; index >= 1; index-- {
		source := archivePath(path, index)
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect service output archive: %w", err)
		}
		if err := os.Rename(source, archivePath(path, index+1)); err != nil {
			return fmt.Errorf("shift service output archive: %w", err)
		}
	}
	if err := os.Rename(path, archivePath(path, 1)); err != nil {
		return fmt.Errorf("archive service output: %w", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		return fmt.Errorf("activate replacement service output: %w", err)
	}
	return nil
}

func beginOutputPathTransaction(path, replacement string, archives int) (*outputPathTransaction, error) {
	transaction := &outputPathTransaction{}
	oldest := archivePath(path, archives)
	if _, err := os.Lstat(oldest); err == nil {
		transaction.oldestBackup = replacement + ".oldest"
		if err := transaction.move(oldest, transaction.oldestBackup); err != nil {
			return nil, fmt.Errorf("preserve oldest service output: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect oldest service output: %w", err)
	}
	for index := archives - 1; index >= 1; index-- {
		source := archivePath(path, index)
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, transaction.fail(fmt.Errorf("inspect service output archive: %w", err))
		}
		if err := transaction.move(source, archivePath(path, index+1)); err != nil {
			return nil, transaction.fail(fmt.Errorf("shift service output archive: %w", err))
		}
	}
	if err := transaction.move(path, archivePath(path, 1)); err != nil {
		return nil, transaction.fail(fmt.Errorf("archive service output: %w", err))
	}
	if err := transaction.move(replacement, path); err != nil {
		return nil, transaction.fail(fmt.Errorf("activate replacement service output: %w", err))
	}
	return transaction, nil
}

func (transaction *outputPathTransaction) move(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	transaction.moves = append(transaction.moves, outputPathMove{source: source, target: target})
	return nil
}

func (transaction *outputPathTransaction) fail(cause error) error {
	if err := transaction.Rollback(); err != nil {
		return fmt.Errorf("%v; rollback service output paths: %w", cause, err)
	}
	return cause
}

func (transaction *outputPathTransaction) Rollback() error {
	for index := len(transaction.moves) - 1; index >= 0; index-- {
		move := transaction.moves[index]
		if err := os.Rename(move.target, move.source); err != nil {
			return fmt.Errorf("restore %s: %w", move.source, err)
		}
	}
	transaction.moves = nil
	transaction.oldestBackup = ""
	return nil
}

func (transaction *outputPathTransaction) Commit() error {
	transaction.moves = nil
	if transaction.oldestBackup == "" {
		return nil
	}
	if err := os.Remove(transaction.oldestBackup); err != nil {
		return fmt.Errorf("prune oldest service output: %w", err)
	}
	transaction.oldestBackup = ""
	return nil
}

func createReplacementOutput(path string, mode os.FileMode) (*os.File, string, error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return nil, "", fmt.Errorf("create replacement service output: %w", err)
	}
	tempPath := file.Name()
	if err := secureOutputFile(file, mode); err != nil {
		file.Close()
		os.Remove(tempPath)
		return nil, "", err
	}
	return file, tempPath, nil
}

func (output *rotatingOutput) stopAfterRotationError(cause error) error {
	output.closed = true
	if output.file != nil {
		file := output.file
		output.file = nil
		if err := file.Close(); err != nil {
			return fmt.Errorf("%v; close unusable service output: %w", cause, err)
		}
	}
	return cause
}

func validateOutputDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect service output directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("service output parent is not a directory: %s", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("service output directory is group or world writable: %s", path)
	}
	return nil
}

func validateOutputFile(path string, mode os.FileMode, allowMissing bool) error {
	info, err := os.Lstat(path)
	if allowMissing && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect service output file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("service output path is not a regular file: %s", path)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("secure service output permissions: %w", err)
	}
	return nil
}

func secureOutputFile(file *os.File, mode os.FileMode) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat service output: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("service output is not a regular file")
	}
	if err := file.Chmod(mode); err != nil {
		return fmt.Errorf("set service output permissions: %w", err)
	}
	return nil
}

func archivePath(path string, index int) string {
	return fmt.Sprintf("%s.%d", path, index)
}
