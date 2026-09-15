//go:build windows

package tools

import (
	"context"
	"os/exec"
)

func runShell(ctx context.Context, cmd string) *exec.Cmd {
	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", cmd)
}
