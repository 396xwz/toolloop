package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
)

func TestMapFSOp(t *testing.T) {
	cases := map[string]string{
		"read":     "read",
		"ReadFile": "read",
		"WRITE":    "write",
		"ls":       "list",
		"tree":     "tree",
		"stat":     "info",
	}
	for in, want := range cases {
		if out := mapFSOp(in); out != want {
			t.Fatalf("mapFSOp(%q) = %q; want %q", in, out, want)
		}
	}
}

func TestNormalizeFSArgs(t *testing.T) {
	in := map[string]string{"OP": "Write", "Path": "\"sample.go\"", "content": "hi"}
	out := normalizeFSArgs(in)
	if out["op"] != "write" {
		t.Fatalf("expected op write, got %q", out["op"])
	}
	if out["path"] != "sample.go" {
		t.Fatalf("expected path sample.go, got %q", out["path"])
	}
}

func TestCleanJSONAndParseToolJSON(t *testing.T) {
	raw := "```json\n{\n  \"tool\": \"fs\",\n  \"args\": {\"op\": \"write\", \"path\": \"sample.go\", \"content\": \"x\"}\n}\n```"
	tool, args, err := parseToolJSON(raw)
	if err != nil {
		t.Fatalf("parseToolJSON error: %v", err)
	}
	if tool != "fs" {
		t.Fatalf("expected tool fs, got %q", tool)
	}
	if args["op"] != "write" || args["path"] != "sample.go" || args["content"] != "x" {
		t.Fatalf("unexpected args: %v", args)
	}
}

func TestForceFSArgsFromContent(t *testing.T) {
	raw := "Here is the response:\n```json\n{\"op\":\"write\",\"path\":\"sample.go\",\"content\":\"package main\\n\\nfunc main(){}\"}\n```"
	args, ok := forceFSArgsFromContent(raw)
	if !ok {
		t.Fatalf("forceFSArgsFromContent failed to parse")
	}
	if args["op"] != "write" || args["path"] != "sample.go" {
		t.Fatalf("unexpected parsed args: %v", args)
	}
}

func TestExtractFSCommandFromTextAndParseKeyValueArgs(t *testing.T) {
	line := "fs write path=sample.go content='package main\nfunc main(){}'"
	args, ok := extractFSCommandFromText(line)
	if !ok {
		t.Fatalf("extractFSCommandFromText failed")
	}
	if args["path"] != "sample.go" || args["content"] == "" {
		t.Fatalf("unexpected args: %v", args)
	}
}

func TestResolveToolFromResponse_ContentJSON(t *testing.T) {
	m := api.Message{Content: "{\"tool\": \"fs\", \"args\": {\"op\": \"write\", \"path\": \"sample.go\", \"content\": \"x\"}}"}
	name, args, ok := resolveToolFromResponse(m)
	if !ok || name != "fs" {
		t.Fatalf("resolveToolFromResponse failed: %s %v %v", name, args, ok)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func newTestGitRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	runTestGit(t, dir, "init", "--quiet")
	runTestGit(t, dir, "config", "user.name", "Toolloop Test")
	runTestGit(t, dir, "config", "user.email", "toolloop@example.test")
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "sample.txt")
	runTestGit(t, dir, "commit", "--quiet", "-m", "Initial commit")
	return dir
}

func TestGitREPLCurrentState(t *testing.T) {
	dir := newTestGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "sample.txt")
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("unstaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	out, err := handleGitREPLCommand(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleGitREPLCommand: %v", err)
	}
	for _, want := range []string{
		"Repository root: " + dir,
		"Branch: ",
		"Status:",
		"MM sample.txt",
		"Staged changes:",
		"-before",
		"+staged",
		"Unstaged changes:",
		"-staged",
		"+unstaged",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestGitREPLStatusPreservesTwoColumns(t *testing.T) {
	tests := []struct {
		name  string
		stage bool
		want  string
	}{
		{name: "unstaged only", want: "\n M sample.txt\n"},
		{name: "staged only", stage: true, want: "\nM  sample.txt\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := newTestGitRepo(t)
			if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if test.stage {
				runTestGit(t, dir, "add", "sample.txt")
			}
			t.Chdir(dir)

			out, err := handleGitREPLCommand(context.Background(), nil)
			if err != nil {
				t.Fatalf("handleGitREPLCommand: %v", err)
			}
			if !strings.Contains(out, test.want) {
				t.Errorf("status did not preserve two-column entry %q:\n%s", test.want, out)
			}
		})
	}
}

func TestGitREPLRevision(t *testing.T) {
	dir := newTestGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "greeting.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "greeting.txt")
	runTestGit(t, dir, "commit", "--quiet", "-m", "Add greeting")
	sha := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "HEAD"))
	t.Chdir(dir)

	out, err := handleGitREPLCommand(context.Background(), []string{"HEAD"})
	if err != nil {
		t.Fatalf("handleGitREPLCommand: %v", err)
	}
	for _, want := range []string{
		"Resolved revision: " + sha,
		"Commit metadata:",
		"Add greeting",
		"Commit changes:",
		"+hello",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestGitREPLDisablesTextconv(t *testing.T) {
	dir := newTestGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("sample.txt diff=sideeffect\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", ".gitattributes")
	runTestGit(t, dir, "commit", "--quiet", "-m", "Configure textconv")
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "sample.txt")
	runTestGit(t, dir, "commit", "--quiet", "-m", "Change sample")

	marker := filepath.Join(dir, "textconv-ran")
	command := fmt.Sprintf("sh -c 'printf invoked > %q'", marker)
	runTestGit(t, dir, "config", "diff.sideeffect.textconv", command)
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "diff", "--textconv")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("configured textconv driver did not run: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	for _, revision := range [][]string{nil, []string{"HEAD"}} {
		if _, err := handleGitREPLCommand(context.Background(), revision); err != nil {
			t.Fatalf("handleGitREPLCommand(%v): %v", revision, err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("textconv driver ran for /git %v", revision)
		}
	}
}

func TestRunGitLimitedTimesOut(t *testing.T) {
	dir := newTestGitRepo(t)
	runTestGit(t, dir, "config", "alias.slow", "!exec sleep 1")
	originalTimeout := gitCommandTimeout
	gitCommandTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		gitCommandTimeout = originalTimeout
	})

	start := time.Now()
	_, err := runGitLimited(context.Background(), dir, 4096, "slow")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "git slow timed out after 50ms") {
		t.Fatalf("error = %v; want clear timeout error", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("command returned after %s; want close to 50ms timeout", elapsed)
	}
}

func TestParseGitVersion(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    gitVersion
		wantErr bool
	}{
		{name: "standard", output: "git version 2.30.0\n", want: gitVersion{major: 2, minor: 30}},
		{name: "vendor suffix", output: "git version 2.43.0.windows.1\n", want: gitVersion{major: 2, minor: 43}},
		{name: "vendor annotation", output: "git version 2.39.3 (Apple Git-145)\n", want: gitVersion{major: 2, minor: 39}},
		{name: "release candidate", output: "git version 3.0.0-rc1\n", want: gitVersion{major: 3, minor: 0}},
		{name: "missing prefix", output: "version 2.30.0", wantErr: true},
		{name: "missing minor", output: "git version 2", wantErr: true},
		{name: "non-numeric minor", output: "git version 2.x.0", wantErr: true},
		{name: "unstructured suffix", output: "git version 2.30.0 vendor", wantErr: true},
		{name: "multiple lines", output: "git version 2.30.0\nextra", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseGitVersion(test.output)
			if test.wantErr {
				if err == nil {
					t.Fatal("parseGitVersion succeeded; want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitVersion: %v", err)
			}
			if got != test.want {
				t.Fatalf("parseGitVersion = %+v; want %+v", got, test.want)
			}
		})
	}
}

func TestGitVersionAtLeast(t *testing.T) {
	tests := []struct {
		version gitVersion
		want    bool
	}{
		{version: gitVersion{major: 2, minor: 29}, want: false},
		{version: gitVersion{major: 2, minor: 30}, want: true},
		{version: gitVersion{major: 2, minor: 31}, want: true},
		{version: gitVersion{major: 3, minor: 0}, want: true},
	}
	for _, test := range tests {
		if got := test.version.atLeast(2, 30); got != test.want {
			t.Errorf("%+v.atLeast(2, 30) = %t; want %t", test.version, got, test.want)
		}
	}
}

func TestGitCommandInterrupted(t *testing.T) {
	if !gitCommandInterrupted(errors.New("git status timed out after 10s")) {
		t.Fatal("expected timeout to be detected")
	}
	if gitCommandInterrupted(errors.New("git status failed")) {
		t.Fatal("unexpected interruption detection")
	}
}

func TestGitREPLErrors(t *testing.T) {
	requireGit(t)
	t.Run("outside repository", func(t *testing.T) {
		t.Chdir(t.TempDir())
		_, err := handleGitREPLCommand(context.Background(), nil)
		if err == nil || err.Error() != "not inside a git repository" {
			t.Fatalf("error = %v; want not inside a git repository", err)
		}
	})

	t.Run("invalid revision", func(t *testing.T) {
		dir := newTestGitRepo(t)
		t.Chdir(dir)
		_, err := handleGitREPLCommand(context.Background(), []string{"missing-revision"})
		if err == nil || err.Error() != "invalid git revision: missing-revision" {
			t.Fatalf("error = %v; want invalid revision error", err)
		}
	})

	t.Run("extra arguments", func(t *testing.T) {
		_, err := handleGitREPLCommand(context.Background(), []string{"HEAD", "main"})
		if err == nil || err.Error() != "usage: /git [revision]" {
			t.Fatalf("error = %v; want usage error", err)
		}
	})
}

func TestGitREPLTruncatesLargeDiff(t *testing.T) {
	dir := newTestGitRepo(t)
	large := strings.Repeat("x", gitSectionOutputLimit+1024) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "sample.txt")
	t.Chdir(dir)

	out, err := handleGitREPLCommand(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleGitREPLCommand: %v", err)
	}
	if !strings.Contains(out, "...[truncated]") {
		t.Fatalf("expected truncation marker in output")
	}
	if len(out) > gitRenderedOutputLimit+len(gitTruncationMarker) {
		t.Fatalf("output length = %d; limit = %d", len(out), gitRenderedOutputLimit+len(gitTruncationMarker))
	}
}
