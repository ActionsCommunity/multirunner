//go:build windows

package main

import "testing"

func TestServiceConfigArgument(t *testing.T) {
	cases := map[string]string{
		`C:\multirunner\multirunner.exe run --config C:\multirunner\config.yaml`:         `C:\multirunner\config.yaml`,
		`"C:\Program Files\mr\multirunner.exe" --config "C:\My Configs\config.yaml" run`: `C:\My Configs\config.yaml`,
		`C:\multirunner\multirunner.exe run --config=C:\multirunner\config.yaml`:         `C:\multirunner\config.yaml`,
		`C:\multirunner\multirunner.exe run`:                                             "",
		`C:\multirunner\multirunner.exe run --config`:                                    "",
	}
	for commandLine, want := range cases {
		if got := serviceConfigArgument(commandLine); got != want {
			t.Errorf("serviceConfigArgument(%q) = %q, want %q", commandLine, got, want)
		}
	}
	if !sameConfigPath(`c:\multirunner\CONFIG.yaml`, `C:\multirunner\config.yaml`) {
		t.Fatal("config paths must compare case-insensitively on Windows")
	}
}
