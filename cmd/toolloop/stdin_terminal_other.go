//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package main

// stdinIsTTY has no TTY ioctl on this GOOS; the char-device check in
// stdinIsTerminal is all that is available.
func stdinIsTTY() bool { return true }
