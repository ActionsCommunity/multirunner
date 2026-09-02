package servicehost

import (
	"strings"
	"testing"
	"time"
)

func TestWindowsRestartDelaysAreFiniteAndBounded(t *testing.T) {
	delays := WindowsRestartDelays()
	if len(delays) != 3 {
		t.Fatalf("restart attempts = %d, want 3", len(delays))
	}
	for i, delay := range delays {
		if delay <= 0 || delay > time.Minute {
			t.Errorf("restart delay %d = %s, want within (0, 1m]", i, delay)
		}
		if time.Duration(recoveryResetSeconds)*time.Second != FailureWindow {
			t.Fatalf("SCM reset period = %s, want %s", time.Duration(recoveryResetSeconds)*time.Second, FailureWindow)
		}
	}
}

func TestOptionsForWindowsConfiguresRecovery(t *testing.T) {
	options := OptionsFor("windows")
	if options["OnFailure"] != "restart" {
		t.Errorf("OnFailure = %v, want restart", options["OnFailure"])
	}

	if options["OnFailureDelayDuration"] != "15s" {
		t.Errorf("OnFailureDelayDuration = %v, want 15s", options["OnFailureDelayDuration"])
	}
	if options["OnFailureResetPeriod"] != recoveryResetSeconds {
		t.Errorf("OnFailureResetPeriod = %v, want %d", options["OnFailureResetPeriod"], recoveryResetSeconds)
	}
}

func TestOptionsUsesCurrentPlatform(t *testing.T) {
	options := Options()
	if len(options) == 0 {
		t.Fatal("current platform options are empty")
	}
}

func TestOptionsForSystemdRestartsOnlyOnFailure(t *testing.T) {
	unit, ok := OptionsFor("linux")["SystemdScript"].(string)
	if !ok {
		t.Fatal("SystemdScript is missing")
	}
	for _, directive := range []string{
		"Restart=on-failure",
		"RestartSec=15s",
		"StartLimitIntervalSec=10min",
		"StartLimitBurst=4",
		"StandardOutput=journal",
		"StandardError=journal",
	} {
		if !strings.Contains(unit, directive) {
			t.Errorf("systemd unit missing %q", directive)
		}
	}
	if strings.Contains(unit, "Restart=always") {
		t.Error("systemd unit restarts after an intentional stop")
	}
}

func TestOptionsForLaunchdRestartsOnlyAfterFailure(t *testing.T) {
	options := OptionsFor("darwin")
	plist, ok := options["LaunchdConfig"].(string)
	if !ok {
		t.Fatal("LaunchdConfig is missing")
	}
	for _, element := range []string{
		"<key>KeepAlive</key>",
		"<dict>",
		"<key>SuccessfulExit</key>",
		"<false/>",
		"<key>ThrottleInterval</key>",
		"<integer>15</integer>",
	} {
		if !strings.Contains(plist, element) {
			t.Errorf("launchd plist missing %q", element)
		}
	}
	if strings.Contains(plist, "<key>KeepAlive</key>\n\t<false/>") {
		t.Error("launchd plist disables failure recovery")
	}
	if options["LogDirectory"] != launchdLogDirectory {
		t.Errorf("LogDirectory = %v, want %s", options["LogDirectory"], launchdLogDirectory)
	}
	for _, destination := range []string{"StandardErrorPath", "StandardOutPath", "{{html .StandardOutPath}}"} {
		if !strings.Contains(plist, destination) {
			t.Errorf("launchd plist missing output destination %q", destination)
		}
	}
	for _, obsolete := range []string{"newsyslog", ".pid", "SIGHUP"} {
		if strings.Contains(plist, obsolete) {
			t.Errorf("launchd plist retains signal-based rotation artifact %q", obsolete)
		}
	}
}
