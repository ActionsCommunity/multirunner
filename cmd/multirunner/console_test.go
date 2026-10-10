package main

import (
	"reflect"
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
