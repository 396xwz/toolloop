//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestWindowsTaskkillArgs(t *testing.T) {
	if got, want := windowsTaskkillArgs(4321), []string{"/PID", "4321", "/T", "/F"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windowsTaskkillArgs(4321) = %q; want %q", got, want)
	}
}

func TestWindowsTaskkillCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	cmd := windowsTaskkillCommand(ctx, 4321)
	if cmd.Args[0] != "taskkill" {
		t.Fatalf("command = %q; want taskkill", cmd.Args[0])
	}
	if got, want := cmd.Args[1:], windowsTaskkillArgs(4321); !reflect.DeepEqual(got, want) {
		t.Fatalf("command arguments = %q; want %q", got, want)
	}
}

func TestWindowsTerminateWithoutStartedProcessMarksCancellation(t *testing.T) {
	cancellation := &gitCommandCancellation{cmd: &exec.Cmd{}}

	if err := cancellation.terminate(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminate() error = %v; want %v", err, os.ErrProcessDone)
	}
	if !cancellation.terminating {
		t.Fatal("terminate() did not mark cancellation as terminating")
	}
	if err := cancellation.terminate(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("second terminate() error = %v; want %v", err, os.ErrProcessDone)
	}
}
