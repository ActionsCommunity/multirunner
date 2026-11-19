package backgroundexec

import (
	"context"
	"os/exec"
)

// Command creates a background helper without opening a Windows console.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	prepare(cmd)
	return cmd
}

// CommandContext is Command with cancellation controlled by ctx.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	prepare(cmd)
	return cmd
}
