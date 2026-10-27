package update

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

const EmbeddedRootVariable = "github.com/GerardSmit/multirunner/internal/update.EmbeddedRootBase64"

// EmbeddedRootBase64 is set by trusted release builds. Development builds are
// intentionally trustless unless an operator configures an external root.
var EmbeddedRootBase64 string

func EmbeddedRoot() ([]byte, error) {
	value := strings.TrimSpace(EmbeddedRootBase64)
	if value == "" {
		return nil, nil
	}
	root, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(bytesTrimSpace(root)) == 0 {
		return nil, errors.New("embedded update root is invalid")
	}
	return root, nil
}

func VerifyEmbeddedRoot(data []byte) (Envelope[Root], error) {
	root, err := decodeEnvelope[Root](data)
	if err != nil {
		return Envelope[Root]{}, err
	}
	if err := validateRoot(root.Signed, time.Now().UTC()); err != nil {
		return Envelope[Root]{}, err
	}
	if err := verifyRole(root, "root", root.Signatures, root.Signed, Policy{}); err != nil {
		return Envelope[Root]{}, err
	}
	return root, nil
}

func EncodeEmbeddedRoot(data []byte) (string, error) {
	if _, err := VerifyEmbeddedRoot(data); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
