package update

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestEncodeEmbeddedRootRoundTripsVerifiedRoot(t *testing.T) {
	repository := newTestRepository(t)
	encoded, err := EncodeEmbeddedRoot(repository.root)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, repository.root) {
		t.Fatal("encoded root did not round trip exactly")
	}
}

func TestEncodeEmbeddedRootRejectsUnsignedData(t *testing.T) {
	if _, err := EncodeEmbeddedRoot([]byte(`{"signed":{},"signatures":[]}`)); err == nil {
		t.Fatal("unsigned root was encoded")
	}
}
