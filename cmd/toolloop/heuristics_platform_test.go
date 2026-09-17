package main

import (
	"strings"
	"testing"
)

func TestHostPlatformInstructionsWindows(t *testing.T) {
	instructions := hostPlatformInstructions("windows")
	for _, want := range []string{
		"Host OS is windows (GOOS=windows)",
		"PowerShell-compatible, not Bash",
		"ls, which, cat, rm, grep, &&, 2>/dev/null, /dev/null, or uname",
		"fs for list/read/info/tree",
		`Get-Item .\file.jar`,
		"Get-Command python",
		`python -c "..."`,
		`.\name.jar`,
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions missing %q: %s", want, instructions)
		}
	}
}

func TestHostPlatformInstructionsUnix(t *testing.T) {
	instructions := hostPlatformInstructions("linux")
	for _, want := range []string{"Host OS is linux (GOOS=linux)", "Shell commands run with Bash"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions missing %q: %s", want, instructions)
		}
	}
}

func TestIsPlanFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"plan.md", true},
		{"./plan.md", true},
		{"sub/dir/plan.md", true},
		{"/abs/plan.md", true},
		{" plan.md ", true},
		{"plan", false},
		{"plan.txt", false},
		{"PLAN.MD", false},
		{"plan.md.bak", false},
		{"notes.md", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isPlanFile(c.path); got != c.want {
			t.Errorf("isPlanFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
