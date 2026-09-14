//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package main

import (
	"os"
	"os/exec"
)

type gitCommandCancellation struct {
	cmd *exec.Cmd
}

func newGitCommandCancellation(cmd *exec.Cmd) (*gitCommandCancellation, error) {
	cancellation := &gitCommandCancellation{cmd: cmd}
	cmd.Cancel = cancellation.terminate
	return cancellation, nil
}

func (c *gitCommandCancellation) attach(*exec.Cmd) error {
	return nil
}

func (c *gitCommandCancellation) terminate() error {
	if c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	return c.cmd.Process.Kill()
}

func (c *gitCommandCancellation) close() error {
	return nil
}
