//go:build windows

package backgroundexec

import (
	"context"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHelperHasNoConsole(t *testing.T) {
	if os.Getenv("MULTIRUNNER_CONSOLE_TEST") == "1" {
		window, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
		if window != 0 {
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString("no console")
		os.Exit(0)
	}
	for _, contextual := range []bool{false, true} {
		cmd := Command(os.Args[0], "-test.run=^TestHelperHasNoConsole$")
		if contextual {
			cmd = CommandContext(context.Background(), os.Args[0], "-test.run=^TestHelperHasNoConsole$")
		}
		cmd.Env = append(os.Environ(), "MULTIRUNNER_CONSOLE_TEST=1")
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != "no console" {
			t.Fatalf("contextual=%v: output=%q, err=%v", contextual, out, err)
		}
	}
}
