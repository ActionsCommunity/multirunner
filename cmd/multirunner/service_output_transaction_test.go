package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutputPathTransactionRollbackRestoresEveryPath(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "service.log")
	replacement := filepath.Join(directory, "replacement")
	writeContent(t, path, "current")
	writeContent(t, replacement, "replacement")
	for index := 1; index <= 5; index++ {
		writeContent(t, archivePath(path, index), string(rune('0'+index)))
	}

	transaction, err := beginOutputPathTransaction(path, replacement, 5)
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, path, "replacement")
	assertFileContent(t, archivePath(path, 1), "current")
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, path, "current")
	assertFileContent(t, replacement, "replacement")
	for index := 1; index <= 5; index++ {
		assertFileContent(t, archivePath(path, index), string(rune('0'+index)))
	}
}

func TestOutputPathTransactionCommitPrunesOnlyOldestArchive(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "service.log")
	replacement := filepath.Join(directory, "replacement")
	writeContent(t, path, "current")
	writeContent(t, replacement, "replacement")
	for index := 1; index <= 5; index++ {
		writeContent(t, archivePath(path, index), string(rune('0'+index)))
	}

	transaction, err := beginOutputPathTransaction(path, replacement, 5)
	if err != nil {
		t.Fatal(err)
	}
	oldestBackup := transaction.oldestBackup
	if oldestBackup == "" {
		t.Fatal("oldest archive was not preserved until commit")
	}
	assertFileContent(t, oldestBackup, "5")
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldestBackup); !os.IsNotExist(err) {
		t.Fatalf("oldest backup remains after commit: %v", err)
	}
	assertFileContent(t, path, "replacement")
	assertFileContent(t, archivePath(path, 1), "current")
	for index := 2; index <= 5; index++ {
		assertFileContent(t, archivePath(path, index), string(rune('0'+index-1)))
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("repeated commit failed: %v", err)
	}
}

func TestOutputPathTransactionFailureRestoresCompletedMoves(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "service.log")
	writeContent(t, path, "current")
	for index := 1; index <= 2; index++ {
		writeContent(t, archivePath(path, index), string(rune('0'+index)))
	}

	if _, err := beginOutputPathTransaction(path, filepath.Join(directory, "missing"), 2); err == nil {
		t.Fatal("missing replacement did not fail the transaction")
	}
	assertFileContent(t, path, "current")
	assertFileContent(t, archivePath(path, 1), "1")
	assertFileContent(t, archivePath(path, 2), "2")
}

func TestOutputPathTransactionWithoutOldestArchive(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "service.log")
	replacement := filepath.Join(directory, "replacement")
	writeContent(t, path, "current")
	writeContent(t, replacement, "replacement")

	transaction, err := beginOutputPathTransaction(path, replacement, 3)
	if err != nil {
		t.Fatal(err)
	}
	if transaction.oldestBackup != "" {
		t.Fatalf("unexpected oldest backup: %s", transaction.oldestBackup)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, path, "replacement")
	assertFileContent(t, archivePath(path, 1), "current")
}

func TestOutputPathTransactionReportsRollbackFailure(t *testing.T) {
	directory := t.TempDir()
	transaction := &outputPathTransaction{moves: []outputPathMove{{
		source: filepath.Join(directory, "source"),
		target: filepath.Join(directory, "missing"),
	}}}
	if err := transaction.Rollback(); err == nil {
		t.Fatal("missing rollback target produced no error")
	}
}

func TestOutputPathTransactionFailIncludesRollbackFailure(t *testing.T) {
	directory := t.TempDir()
	transaction := &outputPathTransaction{moves: []outputPathMove{{
		source: filepath.Join(directory, "source"),
		target: filepath.Join(directory, "missing"),
	}}}
	if err := transaction.fail(os.ErrInvalid); err == nil {
		t.Fatal("transaction failure omitted its rollback failure")
	}
}

func TestOutputPathTransactionRejectsOccupiedBackup(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "service.log")
	replacement := filepath.Join(directory, "replacement")
	writeContent(t, archivePath(path, 2), "oldest")
	backup := replacement + ".oldest"
	if err := os.Mkdir(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	writeContent(t, filepath.Join(backup, "child"), "occupied")
	if _, err := beginOutputPathTransaction(path, replacement, 2); err == nil {
		t.Fatal("occupied oldest backup was accepted")
	}
}

func TestOutputPathTransactionReportsCommitFailure(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "oldest")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeContent(t, filepath.Join(directory, "child"), "retained")
	transaction := &outputPathTransaction{oldestBackup: directory}
	if err := transaction.Commit(); err == nil {
		t.Fatal("unremovable oldest backup produced no error")
	}
}

func writeContent(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}
