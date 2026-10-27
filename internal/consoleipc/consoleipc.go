// Package consoleipc provides the protected local pairing channel between the
// Multirunner daemon and CLI.
package consoleipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	protocolVersion  = 1
	maxRequestBytes  = 1024
	maxResponseBytes = 2048
	ioTimeout        = 5 * time.Second
	maxConnections   = 4
)

type PairingIssuer interface {
	PairingTokenWithExpiry() (string, time.Time, error)
}

type Pairing struct {
	URL       string    `json:"url"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type request struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
}

type response struct {
	Version int      `json:"version"`
	Pairing *Pairing `json:"pairing,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type Server struct {
	listener net.Listener
	cleanup  func() error
	issuer   PairingIssuer
	url      string
	closed   chan struct{}
	once     sync.Once
}

func Listen(databasePath, consoleURL string, issuer PairingIssuer) (*Server, error) {
	if issuer == nil {
		return nil, errors.New("console pairing issuer is required")
	}
	if err := validateConsoleURL(consoleURL); err != nil {
		return nil, err
	}
	endpoint, err := Endpoint(databasePath)
	if err != nil {
		return nil, err
	}
	listener, cleanup, err := listenEndpoint(endpoint, databasePath)
	if err != nil {
		return nil, fmt.Errorf("listen for console pairing: %w", err)
	}
	return &Server{
		listener: listener,
		cleanup:  cleanup,
		issuer:   issuer,
		url:      consoleURL,
		closed:   make(chan struct{}),
	}, nil
}

func (s *Server) Serve(ctx context.Context) error {
	if s == nil || s.listener == nil {
		return errors.New("console pairing server is not initialized")
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.closed:
		}
	}()

	var workers sync.WaitGroup
	defer workers.Wait()
	slots := make(chan struct{}, maxConnections)
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept console pairing connection: %w", err)
		}
		select {
		case slots <- struct{}{}:
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-slots }()
				s.handle(connection)
			}()
		default:
			_ = connection.Close()
		}
	}
}

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	s.once.Do(func() {
		close(s.closed)
		if s.listener != nil {
			closeErr = s.listener.Close()
			if errors.Is(closeErr, net.ErrClosed) {
				closeErr = nil
			}
		}
		if s.cleanup != nil {
			closeErr = errors.Join(closeErr, s.cleanup())
		}
	})
	return closeErr
}

func (s *Server) handle(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(ioTimeout))

	var req request
	if err := decodeBounded(connection, maxRequestBytes, &req); err != nil {
		_ = json.NewEncoder(connection).Encode(response{
			Version: protocolVersion,
			Error:   "invalid pairing request",
		})
		return
	}
	if req.Version != protocolVersion || req.Operation != "pair" {
		_ = json.NewEncoder(connection).Encode(response{
			Version: protocolVersion,
			Error:   "unsupported pairing request",
		})
		return
	}
	token, expiresAt, err := s.issuer.PairingTokenWithExpiry()
	if err != nil {
		_ = json.NewEncoder(connection).Encode(response{
			Version: protocolVersion,
			Error:   "pairing is temporarily unavailable",
		})
		return
	}
	_ = json.NewEncoder(connection).Encode(response{
		Version: protocolVersion,
		Pairing: &Pairing{
			URL:       s.url,
			Token:     token,
			ExpiresAt: expiresAt.UTC(),
		},
	})
}

func RequestPairing(ctx context.Context, databasePath string) (Pairing, error) {
	return requestPairing(ctx, databasePath, dialEndpoint)
}

func requestPairing(
	ctx context.Context,
	databasePath string,
	dial func(context.Context, string) (net.Conn, error),
) (Pairing, error) {
	endpoint, err := Endpoint(databasePath)
	if err != nil {
		return Pairing{}, err
	}
	connection, err := dial(ctx, endpoint)
	if err != nil {
		return Pairing{}, fmt.Errorf(
			"connect to the Multirunner console service: %w; start the service and try again",
			err,
		)
	}
	defer connection.Close()
	deadline := time.Now().Add(ioTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return Pairing{}, fmt.Errorf("set console pairing deadline: %w", err)
	}
	if err := json.NewEncoder(connection).Encode(request{
		Version: protocolVersion, Operation: "pair",
	}); err != nil {
		return Pairing{}, fmt.Errorf("request console pairing: %w", err)
	}
	var result response
	if err := decodeBounded(connection, maxResponseBytes, &result); err != nil {
		return Pairing{}, fmt.Errorf("read console pairing response: %w", err)
	}
	if result.Version != protocolVersion {
		return Pairing{}, errors.New("console pairing protocol version is unsupported")
	}
	if result.Error != "" {
		return Pairing{}, fmt.Errorf("%s", result.Error)
	}
	if result.Pairing == nil || strings.TrimSpace(result.Pairing.Token) == "" ||
		len(result.Pairing.Token) > 512 || !result.Pairing.ExpiresAt.After(time.Now()) {
		return Pairing{}, errors.New("console pairing response is invalid")
	}
	if err := validateConsoleURL(result.Pairing.URL); err != nil {
		return Pairing{}, fmt.Errorf("console pairing response: %w", err)
	}
	return *result.Pairing, nil
}

func decodeBounded(reader io.Reader, maximum int64, destination any) error {
	buffered := bufio.NewReaderSize(reader, int(maximum)+1)
	message, err := buffered.ReadSlice('\n')
	if err != nil {
		return err
	}
	if int64(len(message)) > maximum {
		return errors.New("message exceeds size limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(message)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("message contains trailing data")
	}
	return nil
}

func validateConsoleURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" ||
		parsed.User != nil || parsed.Path != "/" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("console URL must be an HTTP loopback root URL")
	}
	host := parsed.Hostname()
	if host != "localhost" {
		address := net.ParseIP(host)
		if address == nil || !address.IsLoopback() {
			return errors.New("console URL must be an HTTP loopback root URL")
		}
	}
	return nil
}
