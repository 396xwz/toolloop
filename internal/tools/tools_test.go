package tools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type namedTool struct {
	name string
}

func (t namedTool) Name() string { return t.name }

func (t namedTool) Execute(ctx context.Context, args map[string]string) (string, error) {
	return "", nil
}

func TestToolRegistryNames(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register("c", namedTool{name: "c"})
	reg.Register("a", namedTool{name: "a"})
	reg.Register("b", namedTool{name: "b"})

	got := reg.Names()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Names() mismatch\ngot:  %v\nwant: %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() mismatch\ngot:  %v\nwant: %v", got, want)
		}
	}
}

func TestFileSystemEditReplacesUniqueSnippetAndReturnsDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.txt")
	original := "alpha\nbeta\ncharlie\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := (FileSystemTool{}).Execute(context.Background(), map[string]string{
		"op":          "edit",
		"path":        path,
		"old_snippet": "beta",
		"new_snippet": "bravo",
	})
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "alpha\nbravo\ncharlie\n"; got != want {
		t.Fatalf("file content mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
	for _, want := range []string{"diff --git", "-beta", "+bravo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFileSystemEditRejectsZeroOrMultipleMatchesWithoutWriting(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		oldSnippet string
	}{
		{name: "zero matches", content: "alpha\nbeta\n", oldSnippet: "gamma"},
		{name: "multiple matches", content: "alpha\nbeta\nbeta\n", oldSnippet: "beta"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.txt")
			if err := os.WriteFile(path, []byte(tt.content), 0644); err != nil {
				t.Fatal(err)
			}

			_, err := (FileSystemTool{}).Execute(context.Background(), map[string]string{
				"op":          "edit",
				"path":        path,
				"old_snippet": tt.oldSnippet,
				"new_snippet": "replacement",
			})
			if err == nil {
				t.Fatal("expected edit to fail")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(data); got != tt.content {
				t.Fatalf("file changed despite failed edit\ngot:\n%s\nwant:\n%s", got, tt.content)
			}
		})
	}
}

func TestFileSystemEditValidatesLineChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.txt")
	original := "alpha\nbeta\ncharlie\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("beta\n"))

	if _, err := (FileSystemTool{}).Execute(context.Background(), map[string]string{
		"op":          "edit",
		"path":        path,
		"old_snippet": "beta",
		"new_snippet": "bravo",
		"start_line":  "2",
		"end_line":    "2",
		"checksum":    fmt.Sprintf("%x", sum),
	}); err != nil {
		t.Fatalf("edit with valid checksum failed: %v", err)
	}

	path = filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := (FileSystemTool{}).Execute(context.Background(), map[string]string{
		"op":          "edit",
		"path":        path,
		"old_snippet": "beta",
		"new_snippet": "bravo",
		"start_line":  "2",
		"end_line":    "2",
		"checksum":    "bad",
	})
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("file changed despite checksum mismatch\ngot:\n%s\nwant:\n%s", string(data), original)
	}
}

func TestPythonToolRunsScriptFile(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hello.py")
	if err := os.WriteFile(script, []byte("print('hello from script')\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := (PythonTool{}).Execute(context.Background(), map[string]string{"path": script})
	if err != nil {
		t.Fatalf("python script execution failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "hello from script") {
		t.Fatalf("expected script output in result, got: %s", out)
	}
}

func TestPythonToolRunsInlineCode(t *testing.T) {
	out, err := (PythonTool{}).Execute(context.Background(), map[string]string{
		"code": "print('hello from inline code')",
	})
	if err != nil {
		t.Fatalf("python inline execution failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "hello from inline code") {
		t.Fatalf("expected inline output in result, got: %s", out)
	}
}

func TestPythonToolRequiresPathOrCode(t *testing.T) {
	if _, err := (PythonTool{}).Execute(context.Background(), map[string]string{}); err == nil {
		t.Fatal("expected error when neither path nor code is provided")
	}
}

func TestPythonToolMissingScriptErrors(t *testing.T) {
	if _, err := (PythonTool{}).Execute(context.Background(), map[string]string{
		"path": filepath.Join(t.TempDir(), "does-not-exist.py"),
	}); err == nil {
		t.Fatal("expected error for nonexistent script")
	}
}

func TestPythonToolPassesArgs(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "argtest.py")
	src := "import sys\nprint('args:', sys.argv[1:])\n"
	if err := os.WriteFile(script, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := (PythonTool{}).Execute(context.Background(), map[string]string{
		"path": script,
		"args": "foo bar",
	})
	if err != nil {
		t.Fatalf("python script with args failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "'foo', 'bar'") {
		t.Fatalf("expected args echoed in output, got: %s", out)
	}
}

func TestPythonToolResolvesRepoRelativeScriptFromSrcCwd(t *testing.T) {
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(repoRoot, "..", "src")
	if st, err := os.Stat(srcDir); err != nil || !st.IsDir() {
		t.Skip("src directory not available")
	}
	t.Chdir(srcDir)

	out, err := (PythonTool{}).Execute(context.Background(), map[string]string{
		"code": "print('cwd smoke')",
	})
	if err != nil {
		t.Fatalf("inline python from src cwd failed: %v\noutput: %s", err, out)
	}

	script := filepath.Join(repoRoot, "..", "python", "tool_smoke_test.py")
	if err := os.WriteFile(script, []byte("print('repo relative smoke')\n"), 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(script)

	out, err = (PythonTool{}).Execute(context.Background(), map[string]string{
		"path": "python/tool_smoke_test.py",
	})
	if err != nil {
		t.Fatalf("repo-relative python script from src cwd failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "repo relative smoke") {
		t.Fatalf("expected repo-relative script output, got: %s", out)
	}
}
