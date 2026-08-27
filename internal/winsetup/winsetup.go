// Package winsetup installs the standalone Windows-container dockerd that the
// Windows backend needs. The setup script is embedded so the binary is
// self-contained; Install runs it elevated via UAC and reports the outcome.
package winsetup

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"

	"github.com/docker/docker/client"
)

// encodePowerShell encodes a script for powershell -EncodedCommand (base64 of
// UTF-16LE), so the embedded script runs without ever being written to disk.
func encodePowerShell(s string) string {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, len(u)*2)
	for i, r := range u {
		binary.LittleEndian.PutUint16(buf[i*2:], r)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

//go:embed install-windows-daemon.ps1
var script string

//go:embed install-containerd.ps1
var containerdScript string

// Pipe is the named pipe the standalone Windows dockerd listens on.
const Pipe = `npipe:////./pipe/docker_engine_windows`

// DaemonReachable reports whether a Windows-container daemon is reachable at host
// and running in windows mode.
func DaemonReachable(ctx context.Context, host string) bool {
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		return false
	}
	defer cli.Close()
	info, err := cli.Info(ctx)
	if err != nil {
		return false
	}
	return info.OSType == "windows"
}

func statusPaths() (statusFile, logFile string) {
	dir := filepath.Join(os.Getenv("ProgramData"), "multirunner")
	return filepath.Join(dir, "winsetup-status.txt"), filepath.Join(dir, "winsetup.log")
}

// LastStatus returns the outcome recorded by the last install run
// ("ok" | "reboot-required" | "error: …"), or ok=false if none.
func LastStatus() (string, bool) {
	statusFile, _ := statusPaths()
	b, err := os.ReadFile(statusFile)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// RebootPending reports whether Windows has a pending reboot (e.g. after enabling
// the Containers feature).
func RebootPending() bool { return rebootPending() }

// DaemonHint returns actionable guidance when the Windows daemon is unreachable,
// for use in doctor / preflight output.
func DaemonHint() string {
	if RebootPending() {
		return "Windows Containers feature enabled but REBOOT pending; reboot, then run: multirunner install-windows-daemon"
	}
	if s, ok := LastStatus(); ok {
		switch {
		case s == "reboot-required":
			return "Containers feature enabled, awaiting reboot; reboot, then run: multirunner install-windows-daemon"
		case strings.HasPrefix(s, "error"):
			return "previous install failed (" + s + "); re-run: multirunner install-windows-daemon"
		}
	}
	return "no Windows-container daemon; run: multirunner install-windows-daemon"
}

// wrapScript prepares a script for -EncodedCommand. -EncodedCommand cannot take
// arguments, so when args are supplied the script is wrapped in a script block
// and invoked with them; its param() block binds them normally. Scripts called
// without args are passed through unchanged.
func wrapScript(body string, args ...string) string {
	if len(args) == 0 {
		return body
	}
	return "& {\n" + body + "\n} " + strings.Join(args, " ")
}

// runElevated runs an embedded setup script elevated (UAC) and returns the
// status recorded in the status file plus any process error. The script is
// passed in-memory via -EncodedCommand so nothing is written to disk.
func runElevated(scriptBody string, args ...string) (status string, runErr error) {
	statusFile, logFile := statusPaths()
	_ = os.Remove(statusFile)
	enc := encodePowerShell(wrapScript(scriptBody, args...))
	psCmd := fmt.Sprintf(
		"Start-Process -FilePath powershell -Verb RunAs -Wait -ArgumentList @('-NoProfile','-ExecutionPolicy','Bypass','-EncodedCommand','%s')",
		enc)
	cmd := exec.Command("powershell", "-NoProfile", "-Command", psCmd)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	runErr = cmd.Run()
	status, _ = LastStatus()
	printLogTail(logFile)
	return status, runErr
}

// psQuote renders s as a PowerShell single-quoted literal. Only the single
// quote is special inside such a literal, and it is escaped by doubling.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// InstallContainerd installs containerd + runhcs + nerdctl + CNI elevated, the
// supported Windows-container runtime. Windows only.
func InstallContainerd() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("install-containerd is only supported on Windows")
	}
	status, runErr := runElevated(containerdScript)
	switch {
	case strings.HasPrefix(status, "reboot-required"):
		fmt.Println("\nContainers/Hyper-V features enabled — REBOOT required, then re-run: multirunner install-containerd")
		return nil
	case status == "ok":
		fmt.Println("\ncontainerd + nerdctl + runhcs installed and running (pipe \\\\.\\pipe\\containerd-containerd)")
		return nil
	case strings.HasPrefix(status, "error"):
		return fmt.Errorf("containerd install failed: %s", strings.TrimPrefix(status, "error: "))
	default:
		if runErr != nil {
			return fmt.Errorf("containerd install: elevation declined or failed: %w", runErr)
		}
		return fmt.Errorf("containerd install: unknown result")
	}
}

// InstallOptions configures the Windows daemon install.
type InstallOptions struct {
	// DataRoot overrides where the daemon stores images and containers. Empty
	// uses the script default (%ProgramData%\multirunner\docker\data). Set this
	// to keep the image store off the system volume, or to adopt an existing
	// store so images survive replacing a previous daemon.
	DataRoot string
}

// installArgs renders opts as PowerShell named arguments for the setup script.
func (o InstallOptions) installArgs() []string {
	var args []string
	if o.DataRoot != "" {
		args = append(args, "-DataRoot", psQuote(o.DataRoot))
	}
	return args
}

// Install extracts the embedded setup script, runs it elevated (UAC), and reports
// the result read back from the status file. Windows only.
func Install(opts InstallOptions) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("install-windows-daemon is only supported on Windows")
	}
	_, logFile := statusPaths()
	status, runErr := runElevated(script, opts.installArgs()...)

	switch {
	case strings.HasPrefix(status, "reboot-required"):
		fmt.Println("\nWindows Containers feature enabled — REBOOT required, then re-run: multirunner install-windows-daemon")
		return nil
	case status == "ok":
		fmt.Printf("\nWindows-container daemon installed and running on %s\n", Pipe)
		return nil
	case strings.HasPrefix(status, "error"):
		return fmt.Errorf("windows daemon install failed: %s (log: %s)", strings.TrimPrefix(status, "error: "), logFile)
	default:
		if runErr != nil {
			return fmt.Errorf("windows daemon install: elevation declined or failed: %w", runErr)
		}
		return fmt.Errorf("windows daemon install: unknown result (log: %s)", logFile)
	}
}

// printLogTail prints the last few lines of the elevated transcript so the user
// sees what happened in the (now-closed) elevated window.
func printLogTail(logFile string) {
	f, err := os.Open(logFile)
	if err != nil {
		return
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	const n = 20
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) > 0 {
		fmt.Println("--- install log (tail) ---")
		for _, l := range lines {
			fmt.Println(l)
		}
	}
}
