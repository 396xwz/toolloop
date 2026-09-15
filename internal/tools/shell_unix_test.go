//go:build unix

package tools

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRunShellUsesBash(t *testing.T) {
	cmd := runShell(context.Background(), "echo hello")
	if filepath.Base(cmd.Path) != "bash" {
		t.Errorf("shell executable = %q, want bash", cmd.Path)
	}
	if want := []string{"bash", "-c", "echo hello"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("shell args = %#v, want %#v", cmd.Args, want)
	}
}
