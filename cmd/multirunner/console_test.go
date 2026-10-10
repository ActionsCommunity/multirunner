package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestConsoleBrowserCommandUsesExactFixedConsoleRoot(t *testing.T) {
	consoleURL := "http://127.0.0.1:9092/"
	tests := []struct {
		goos string
		name string
		args []string
	}{
		{goos: "windows", name: "rundll32", args: []string{"url.dll,FileProtocolHandler", consoleURL}},
		{goos: "darwin", name: "open", args: []string{consoleURL}},
		{goos: "linux", name: "xdg-open", args: []string{consoleURL}},
	}
	for _, test := range tests {
		t.Run(test.goos, func(t *testing.T) {
			name, args := consoleBrowserCommand(test.goos, consoleURL)
			if name != test.name || !reflect.DeepEqual(args, test.args) {
				t.Fatalf("launcher = %q %#v, want %q %#v", name, args, test.name, test.args)
			}
		})
	}
}

func TestConsoleCommandDefaultsToOpen(t *testing.T) {
	configPath := "config.yaml"
	command := consoleCmd(&configPath)
	if command.RunE == nil {
		t.Fatal("console command does not launch by default")
	}
	if command.Flags().Lookup("no-browser") == nil {
		t.Fatal("console command is missing --no-browser")
	}
	open, _, err := command.Find([]string{"open"})
	if err != nil {
		t.Fatal(err)
	}
	if open.RunE == nil || open.Flags().Lookup("no-browser") == nil {
		t.Fatal("console open compatibility command is incomplete")
	}
}

func TestRequireConsoleReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter, request *http.Request,
	) {
		if request.URL.Path != "/api/v1/session" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		http.Error(writer, "pairing required", http.StatusUnauthorized)
	}))
	defer server.Close()

	if err := requireConsoleReachable(server.URL + "/"); err != nil {
		t.Fatal(err)
	}
}

func TestRequireConsoleReachableRejectsUnexpectedService(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	err := requireConsoleReachable(server.URL + "/")
	if err == nil || !strings.Contains(err.Error(), "unexpected service response") {
		t.Fatalf("error = %v", err)
	}
}
