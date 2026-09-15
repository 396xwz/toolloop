//go:build windows

package tools

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunShellUsesPowerShell(t *testing.T) {
	cmd := runShell(context.Background(), "Write-Output hello")
	if executable := strings.TrimSuffix(strings.ToLower(filepath.Base(cmd.Path)), ".exe"); executable != "powershell" {
		t.Errorf("shell executable = %q, want powershell", cmd.Path)
	}
	if want := []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", "Write-Output hello"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("shell args = %#v, want %#v", cmd.Args, want)
	}
}
