// Package servicehost defines the native service-manager recovery policy.
package servicehost

import (
	"fmt"
	"runtime"
	"time"

	"github.com/kardianos/service"
)

const (
	// StopTimeout leaves the service manager time to finish process teardown.
	StopTimeout = 15 * time.Second

	recoveryResetSeconds = 10 * 60
	launchdThrottle      = 15
)

var restartDelays = [...]time.Duration{
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// WindowsRestartDelays returns the finite SCM recovery schedule.
func WindowsRestartDelays() []time.Duration {
	delays := make([]time.Duration, len(restartDelays))
	copy(delays, restartDelays[:])
	return delays
}

// Options returns the kardianos options for the current platform.
func Options() service.KeyValue {
	return OptionsFor(runtime.GOOS)
}

// OptionsFor returns the native recovery configuration for goos.
func OptionsFor(goos string) service.KeyValue {
	switch goos {
	case "windows":
		return service.KeyValue{
			"OnFailure":              "restart",
			"OnFailureDelayDuration": restartDelays[0].String(),
			"OnFailureResetPeriod":   recoveryResetSeconds,
		}
	case "linux":
		return service.KeyValue{
			"Restart":       "on-failure",
			"SystemdScript": systemdUnit,
		}
	case "darwin":
		return service.KeyValue{
			"LogDirectory":  launchdLogDirectory,
			"LaunchdConfig": launchdPlist,
		}
	default:
		return service.KeyValue{"Restart": "on-failure"}
	}
}

const systemdUnit = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}
StartLimitIntervalSec=10min
StartLimitBurst=4
{{range $i, $dep := .Dependencies}}{{$dep}}
{{end}}
[Service]
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}}
{{if .WorkingDirectory}}WorkingDirectory={{.WorkingDirectory|cmdEscape}}{{end}}
{{if .UserName}}User={{.UserName}}{{end}}
Restart=on-failure
RestartSec=15s
StandardOutput=journal
StandardError=journal
{{range $k, $v := .EnvVars}}Environment={{$k}}={{$v}}
{{end}}
[Install]
WantedBy=multi-user.target
`

var launchdPlist = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{html .Name}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{html .Path}}</string>
		{{range .Config.Arguments}}<string>{{html .}}</string>
		{{end}}
	</array>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>%d</integer>
	{{if .WorkingDirectory}}<key>WorkingDirectory</key>
	<string>{{html .WorkingDirectory}}</string>{{end}}
	<key>StandardErrorPath</key>
	<string>{{html .StandardOutPath}}</string>
	<key>StandardOutPath</key>
	<string>{{html .StandardOutPath}}</string>
</dict>
</plist>
`, launchdThrottle)
