//go:build !unix && !windows

package tools

import (
	"context"
	"os/exec"
)

// Non-Windows platforms retain the package's historic Bash invocation. Bash
// must be available on PATH when running on one of these uncommon targets.
func runShell(ctx context.Context, cmd string) *exec.Cmd {
	return exec.CommandContext(ctx, "bash", "-c", cmd)
}
