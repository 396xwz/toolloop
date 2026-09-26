//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// stdinIsTTY reports whether stdin is a console: GetConsoleMode fails on
// redirected stdin (pipes, files) and other non-console handles.
func stdinIsTTY() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}
