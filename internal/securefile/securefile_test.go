package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateReadReplaceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := CreateExclusive(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	data, err := Read(path)
	if err != nil || string(data) != "first" {
		t.Fatalf("read = %q, %v", data, err)
	}
	if err := Replace(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	data, err = Read(path)
	if err != nil || string(data) != "second" {
		t.Fatalf("replacement read = %q, %v", data, err)
	}
	if err := CreateExclusive(path, []byte("third")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive create error = %v", err)
	}
}
