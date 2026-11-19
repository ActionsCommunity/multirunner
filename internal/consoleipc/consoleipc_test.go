package consoleipc

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

type testIssuer struct {
	token   string
	expires time.Time
	err     error
}

func (i testIssuer) PairingTokenWithExpiry() (string, time.Time, error) {
	return i.token, i.expires, i.err
}

func TestRequestPairingUsesProtectedProtocolResponse(t *testing.T) {
	expires := time.Now().Add(time.Minute).UTC()
	server := &Server{
		issuer: testIssuer{token: "pairing-token", expires: expires},
		url:    "http://127.0.0.1:9092/",
	}
	clientConnection, serverConnection := net.Pipe()
	go server.handle(serverConnection)

	pairing, err := requestPairing(
		context.Background(),
		t.TempDir()+"/history.db",
		func(context.Context, string) (net.Conn, error) {
			return clientConnection, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if pairing.URL != server.url || pairing.Token != "pairing-token" ||
		!pairing.ExpiresAt.Equal(expires) {
		t.Fatalf("pairing = %+v", pairing)
	}
}

func TestProtectedEndpointRoundTrip(t *testing.T) {
	expires := time.Now().Add(time.Minute).UTC()
	databasePath := t.TempDir() + "/history.db"
	if err := PrepareEndpointIdentity(databasePath, true); err != nil {
		t.Fatal(err)
	}
	server, err := Listen(
		databasePath,
		"http://127.0.0.1:9092/",
		testIssuer{token: "protected-token", expires: expires},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx)
	}()

	requestContext, requestCancel := context.WithTimeout(context.Background(), time.Second)
	pairing, err := RequestPairing(requestContext, databasePath)
	requestCancel()
	if err != nil {
		cancel()
		_ = server.Close()
		<-done
		t.Fatal(err)
	}
	if pairing.Token != "protected-token" || !pairing.ExpiresAt.Equal(expires) {
		t.Fatalf("pairing = %+v", pairing)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerRejectsUnsupportedPairingRequest(t *testing.T) {
	server := &Server{
		issuer: testIssuer{
			token: "must-not-be-issued", expires: time.Now().Add(time.Minute),
		},
		url: "http://127.0.0.1:9092/",
	}
	clientConnection, serverConnection := net.Pipe()
	go server.handle(serverConnection)
	if err := json.NewEncoder(clientConnection).Encode(request{
		Version: protocolVersion + 1, Operation: "pair",
	}); err != nil {
		t.Fatal(err)
	}
	var result response
	if err := decodeBounded(clientConnection, maxResponseBytes, &result); err != nil {
		t.Fatal(err)
	}
	if result.Error != "unsupported pairing request" || result.Pairing != nil {
		t.Fatalf("response = %+v", result)
	}
}

func TestDecodeBoundedRejectsOversizedMessage(t *testing.T) {
	message := `{"version":1,"operation":"` +
		strings.Repeat("x", maxRequestBytes) + "\"}\n"
	var req request
	if err := decodeBounded(strings.NewReader(message), maxRequestBytes, &req); err == nil {
		t.Fatal("oversized message was accepted")
	}
}

func TestValidateConsoleURLRequiresLoopbackRoot(t *testing.T) {
	for _, value := range []string{
		"https://127.0.0.1:9092/",
		"http://example.com/",
		"http://127.0.0.1:9092/path",
		"http://127.0.0.1:9092/?token=secret",
		"http://operator@127.0.0.1:9092/",
	} {
		if err := validateConsoleURL(value); err == nil {
			t.Fatalf("URL %q was accepted", value)
		}
	}
	if err := validateConsoleURL("http://[::1]:9092/"); err != nil {
		t.Fatal(err)
	}
}
