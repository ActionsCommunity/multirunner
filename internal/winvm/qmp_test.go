package winvm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestQMPQuitNamedVerifiesIdentityBeforeQuit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		actual    string
		wantError string
		wantQuit  bool
	}{
		{name: "matching identity", actual: "runner-1", wantQuit: true},
		{name: "different identity", actual: "runner-2", wantError: "identity mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, commands := startQMPTestServer(t, tc.actual)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			err := QMPQuitNamed(ctx, addr, "runner-1")
			if tc.wantError == "" && err != nil {
				t.Fatalf("QMPQuitNamed: %v", err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("QMPQuitNamed error = %v, want %q", err, tc.wantError)
			}
			got := <-commands
			if strings.Contains(got, "quit") != tc.wantQuit {
				t.Fatalf("commands = %q, want quit=%v", got, tc.wantQuit)
			}
		})
	}
}

func TestQMPQuitNamedHonorsCancellationAfterConnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		var buffer [1]byte
		_, _ = conn.Read(buffer[:])
	}()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- QMPQuitNamed(ctx, listener.Addr().String(), "runner-1")
	}()
	<-accepted
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("QMPQuitNamed error = nil after cancellation")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("QMPQuitNamed did not stop after cancellation")
	}
}

func startQMPTestServer(t *testing.T, name string) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	commands := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			commands <- ""
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintln(conn, `{"QMP":{"version":{},"capabilities":[]}}`)
		reader := bufio.NewReader(conn)
		var seen []string
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				commands <- strings.Join(seen, ",")
				return
			}
			var request struct {
				Execute string `json:"execute"`
			}
			if json.Unmarshal(line, &request) != nil {
				commands <- strings.Join(seen, ",")
				return
			}
			seen = append(seen, request.Execute)
			switch request.Execute {
			case "query-name":
				_, _ = fmt.Fprintf(conn, "{\"return\":{\"name\":%q}}\n", name)
			default:
				_, _ = fmt.Fprintln(conn, `{"return":{}}`)
			}
			if request.Execute == "quit" {
				commands <- strings.Join(seen, ",")
				return
			}
		}
	}()
	return listener.Addr().String(), commands
}
