//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type gitCommandCancellation struct {
	cmd *exec.Cmd
}

func newGitCommandCancellation(cmd *exec.Cmd) (*gitCommandCancellation, error) {
	cancellation := &gitCommandCancellation{cmd: cmd}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	if err := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func (c *gitCommandCancellation) close() error {
	return nil
}
