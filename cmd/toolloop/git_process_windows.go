//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsTaskkillTimeout = time.Second

type gitCommandCancellation struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	job         windows.Handle
	terminating bool
}

func newGitCommandCancellation(cmd *exec.Cmd) (*gitCommandCancellation, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	cancellation := &gitCommandCancellation{cmd: cmd, job: job}
	cmd.Cancel = cancellation.terminate
	return cancellation, nil
}

func (c *gitCommandCancellation) attach(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	var assignErr error
	if err := cmd.Process.WithHandle(func(handle uintptr) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.job == 0 || c.terminating {
			assignErr = os.ErrProcessDone
			return
		}
		assignErr = windows.AssignProcessToJobObject(c.job, windows.Handle(handle))
	}); err != nil {
		return err
	}
	return assignErr
}

func windowsTaskkillArgs(pid int) []string {
	return []string{"/PID", strconv.Itoa(pid), "/T", "/F"}
}

func windowsTaskkillCommand(ctx context.Context, pid int) *exec.Cmd {
	return exec.CommandContext(ctx, "taskkill", windowsTaskkillArgs(pid)...)
}

func (c *gitCommandCancellation) terminate() error {
	c.mu.Lock()
	if c.terminating {
		c.mu.Unlock()
		return os.ErrProcessDone
	}
	c.terminating = true
	process := c.cmd.Process
	job := c.job
	c.job = 0
	c.mu.Unlock()

	if process == nil {
		if job != 0 {
			_ = windows.CloseHandle(job)
		}
		return os.ErrProcessDone
	}

	// A child may have been created before Git was assigned to the Job Object.
	// taskkill targets that short window; its timeout keeps cancellation bounded.
	taskkillCtx, cancel := context.WithTimeout(context.Background(), windowsTaskkillTimeout)
	_ = windowsTaskkillCommand(taskkillCtx, process.Pid).Run()
	cancel()

	killErr := process.Kill()
	jobErr := error(nil)
	closeErr := error(nil)
	if job != 0 {
		jobErr = windows.TerminateJobObject(job, 1)
		closeErr = windows.CloseHandle(job)
	}
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return killErr
	}
	if jobErr != nil {
		return jobErr
	}
	return closeErr
}

func (c *gitCommandCancellation) close() error {
	c.mu.Lock()
	job := c.job
	c.job = 0
	c.mu.Unlock()
	if job == 0 {
		return nil
	}
	return windows.CloseHandle(job)
}
