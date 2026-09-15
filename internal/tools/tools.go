package tools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Tool interface {
	Name() string
	Execute(ctx context.Context, args map[string]string) (string, error)
}

type ToolRegistry struct {
	tools map[string]Tool
}

func NewToolRegistry() ToolRegistry {
	return ToolRegistry{tools: make(map[string]Tool)}
}

func (r *ToolRegistry) Register(name string, t Tool) {
	r.tools[name] = t
}

func (r *ToolRegistry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

//
// ======================
// Web Search Tool
// ======================
// Uses DuckDuckGo Instant Answer API (no key required)
//

type WebSearchTool struct{}

func (w WebSearchTool) Name() string { return "web_search" }

func (w WebSearchTool) Execute(ctx context.Context, args map[string]string) (string, error) {
	query := args["query"]
	if query == "" {
		return "", fmt.Errorf("missing 'query' argument")
	}

	// Use DuckDuckGo HTML version (more reliable than Instant Answer API)
	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)

	client := &http.Client{
		Timeout: 12 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return "", err
	}

	// Important headers to look like a real browser
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 200000))
	if err != nil {
		return "", err
	}

	html := string(body)

	// Very simple extraction of results
	var results []string
	lines := strings.Split(html, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Look for result titles and snippets
		if strings.Contains(line, "result__a") || strings.Contains(line, "result__snippet") {
			// Clean basic HTML tags
			clean := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(line, "")
			clean = strings.TrimSpace(clean)
			if len(clean) > 30 {
				results = append(results, clean)
			}
		}
	}

	if len(results) == 0 {
		// Fallback message
		return fmt.Sprintf("No good results found for '%s'. Try using the browser tool on '%s' ", query, query), nil
	}

	// Return top results
	output := fmt.Sprintf("Search results for '%s':\n\n", query)
	for i, r := range results {
		if i >= 8 {
			break
		}
		output += fmt.Sprintf("%d. %s\n\n", i+1, r)
	}

	return output, nil
}

//
// ======================
// Browser Tool (simple HTTP GET)
// ======================

type BrowserTool struct{}

func (b BrowserTool) Name() string { return "browser" }

func (b BrowserTool) Execute(ctx context.Context, args map[string]string) (string, error) {
	action := args["action"]
	targetURL := args["url"]

	if targetURL == "" {
		return "", fmt.Errorf("missing 'url' argument")
	}

	if action == "" {
		action = "get"
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; MyAgent/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20000)) // limit size
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Browser %s %s\nStatus: %d\n\n%s", action, targetURL, resp.StatusCode, string(body)), nil
}

//
// ======================
// Scrape Tool (Scrapling CLI)
// ======================
// Uses the Python Scrapling CLI when available. This is useful for dynamic
// sites where a plain HTTP GET from BrowserTool only returns app shell HTML.
//

type ScrapeTool struct{}

func (s ScrapeTool) Name() string { return "scrape" }

func (s ScrapeTool) Execute(ctx context.Context, args map[string]string) (string, error) {
	targetURL := strings.TrimSpace(args["url"])
	if targetURL == "" {
		return "", fmt.Errorf("missing 'url' argument")
	}

	mode := strings.ToLower(strings.TrimSpace(args["mode"]))
	if mode == "" {
		mode = "fetch"
	}
	switch mode {
	case "get", "fetch", "stealthy-fetch":
	default:
		return "", fmt.Errorf("unknown scrape mode: %s (supported: get, fetch, stealthy-fetch)", mode)
	}

	outputPath := strings.TrimSpace(args["output"])
	if outputPath == "" {
		outputPath = "sports.md"
	}

	timeout := strings.TrimSpace(args["timeout"])
	if timeout == "" {
		if mode == "get" {
			timeout = "45"
		} else {
			timeout = "60000"
		}
	}

	// Default kept modest: this content is embedded directly into the
	// model's prompt context (see GenerateFinalAnswer/PlanNextStep), so a
	// large default risks crowding out the model's own completion budget
	// and producing empty/truncated answers. The full scrape is still
	// written to outputPath regardless of this cap.
	const defaultMaxChars = 8000
	const hardMaxChars = 40000
	maxChars := defaultMaxChars
	if m := strings.TrimSpace(args["max_chars"]); m != "" {
		fmt.Sscanf(m, "%d", &maxChars)
	}
	if maxChars <= 0 {
		maxChars = defaultMaxChars
	}
	if maxChars > hardMaxChars {
		maxChars = hardMaxChars
	}

	bin, err := findScraplingBinary()
	if err != nil {
		return "", err
	}

	cmdArgs := []string{"extract", mode, targetURL, outputPath}
	if selector := strings.TrimSpace(args["selector"]); selector != "" {
		cmdArgs = append(cmdArgs, "--css-selector", selector)
	}
	if mode == "get" {
		cmdArgs = append(cmdArgs, "--timeout", timeout)
	} else {
		cmdArgs = append(cmdArgs,
			"--timeout", timeout,
			"--wait", firstNonEmpty(args["wait"], "3000"),
			"--network-idle",
			"--block-ads",
			"--ai-targeted",
		)
	}

	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("scrapling failed: %w", err)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		return string(out), fmt.Errorf("scrapling wrote no readable output file %q: %w", outputPath, err)
	}

	content := string(data)
	truncated := false
	if len(content) > maxChars {
		content = content[:maxChars]
		truncated = true
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Scraped %s with Scrapling mode=%s\nOutput file: %s\nBytes: %d\n", targetURL, mode, outputPath, len(data)))
	if truncated {
		b.WriteString(fmt.Sprintf("[Truncated to first %d chars]\n", maxChars))
	}
	if len(out) > 0 {
		b.WriteString("\nScrapling output:\n")
		b.WriteString(truncateToolOutput(string(out), 4000))
		b.WriteString("\n")
	}
	b.WriteString("\nContent:\n")
	b.WriteString(content)
	return b.String(), nil
}

func findScraplingBinary() (string, error) {
	if fromEnv := strings.TrimSpace(os.Getenv("SCRAPLING_BIN")); fromEnv != "" {
		if st, err := os.Stat(fromEnv); err == nil && !st.IsDir() {
			return fromEnv, nil
		}
		return "", fmt.Errorf("SCRAPLING_BIN is set but not executable: %s", fromEnv)
	}
	if p, err := exec.LookPath("scrapling"); err == nil {
		return p, nil
	}

	var candidates []string
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, ".venv", "bin", "scrapling"),
			filepath.Join(wd, "..", ".venv", "bin", "scrapling"),
			filepath.Join(wd, "..", "..", ".venv", "bin", "scrapling"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, ".venv", "bin", "scrapling"),
			filepath.Join(dir, "..", ".venv", "bin", "scrapling"),
			filepath.Join(dir, "..", "..", ".venv", "bin", "scrapling"),
		)
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("scrapling CLI not found; install it or set SCRAPLING_BIN")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

//
// ======================
// Python Tool
// ======================
// Runs a Python script (or inline code) with the project's own interpreter
// whenever possible. Using `shell cmd="python3 ..."` silently picks up
// whatever python3 is first on PATH, which is often the *system* Python
// rather than the project's .venv — so scripts that need e.g. scipy fail
// with ModuleNotFoundError even though `.venv/bin/python3 script.py` works
// fine by hand. This tool resolves the interpreter the same way
// findScraplingBinary resolves the scrapling CLI: PYTHON_BIN env var, then
// a .venv next to the cwd/binary, then whatever "python3"/"python" is on
// PATH as a last resort.
//

type PythonTool struct{}

func (p PythonTool) Name() string { return "python" }

func (p PythonTool) Execute(ctx context.Context, args map[string]string) (string, error) {
	path := strings.TrimSpace(args["path"])
	code := args["code"]
	if path == "" && strings.TrimSpace(code) == "" {
		return "", fmt.Errorf("missing 'path' (script file) or 'code' (inline source) for python tool")
	}

	bin, err := findPythonBinary()
	if err != nil {
		return "", err
	}

	var cmdArgs []string
	var tmpFile string
	if path != "" {
		resolvedPath, err := resolvePythonScriptPath(path)
		if err != nil {
			return "", fmt.Errorf("python script not found: %s: %w", path, err)
		}
		cmdArgs = append(cmdArgs, resolvedPath)
	} else {
		f, err := os.CreateTemp("", "agent-python-*.py")
		if err != nil {
			return "", err
		}
		tmpFile = f.Name()
		if _, err := f.WriteString(code); err != nil {
			f.Close()
			os.Remove(tmpFile)
			return "", err
		}
		f.Close()
		defer os.Remove(tmpFile)
		cmdArgs = append(cmdArgs, tmpFile)
	}
	if extra := strings.TrimSpace(args["args"]); extra != "" {
		cmdArgs = append(cmdArgs, splitFiles(extra)...)
	}

	timeout := 60 * time.Second
	if t := strings.TrimSpace(args["timeout"]); t != "" {
		var secs int
		if _, err := fmt.Sscanf(t, "%d", &secs); err == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, cmdArgs...)
	if wd := strings.TrimSpace(args["cwd"]); wd != "" {
		cmd.Dir = wd
	}
	out, err := cmd.CombinedOutput()

	const maxOutput = 20000
	output := truncateToolOutput(string(out), maxOutput)
	if err != nil {
		return output, fmt.Errorf("python (%s) failed: %w", bin, err)
	}
	return fmt.Sprintf("Ran %s with %s\n\n%s", firstNonEmpty(path, "<inline code>"), bin, output), nil
}

func resolvePythonScriptPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		if _, err := os.Stat(path); err != nil {
			return "", err
		}
		return path, nil
	}

	var candidates []string
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, path),
			filepath.Join(wd, "..", path),
			filepath.Join(wd, "..", "..", path),
		)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, path),
			filepath.Join(dir, "..", path),
			filepath.Join(dir, "..", "..", path),
		)
	}
	for _, candidate := range candidates {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

// findPythonBinary resolves which python interpreter to use, preferring the
// project's own virtualenv so scripts have access to installed packages
// (scipy, etc.) instead of a bare system interpreter.
func findPythonBinary() (string, error) {
	if fromEnv := strings.TrimSpace(os.Getenv("PYTHON_BIN")); fromEnv != "" {
		if st, err := os.Stat(fromEnv); err == nil && !st.IsDir() {
			return fromEnv, nil
		}
		return "", fmt.Errorf("PYTHON_BIN is set but not executable: %s", fromEnv)
	}

	var candidates []string
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, ".venv", "bin", "python3"),
			filepath.Join(wd, "..", ".venv", "bin", "python3"),
			filepath.Join(wd, "..", "..", ".venv", "bin", "python3"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, ".venv", "bin", "python3"),
			filepath.Join(dir, "..", ".venv", "bin", "python3"),
			filepath.Join(dir, "..", "..", ".venv", "bin", "python3"),
		)
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("python"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("no python interpreter found; set PYTHON_BIN or create a .venv")
}

func truncateToolOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...[truncated]..."
}

//
// ======================
// File System Tool
// ======================

type FileSystemTool struct{}

func (f FileSystemTool) Name() string { return "fs" }

func (f FileSystemTool) Execute(ctx context.Context, args map[string]string) (string, error) {
	op := args["op"]
	path := args["path"]
	if path == "" {
		path = "."
	}

	// Smart defaults
	if op == "" {
		if args["depth"] != "" {
			op = "tree" // depth provided → assume tree
		} else {
			op = "list"
		}
	}
	if path == "" && op != "tree" {
		// allow tree with default "."
		if op != "tree" {
			return "", fmt.Errorf("missing 'path' argument")
		}
		path = "."
	}
	if path == "" {
		path = "."
	}

	// Basic safety: prevent escaping too far (optional, adjust as needed)
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	_ = abs // you can add allow-list logic here later

	switch op {
	case "list":
		return f.listDir(path)
	case "tree":
		maxDepth := 4
		if d := args["depth"]; d != "" {
			fmt.Sscanf(d, "%d", &maxDepth)
		}
		return f.tree(path, maxDepth)
	case "read":
		maxBytes := 1200000
		if m := args["max_bytes"]; m != "" {
			fmt.Sscanf(m, "%d", &maxBytes)
		}
		return f.readFile(path, maxBytes)
	case "read_many":
		// path can be comma-separated or space-separated list
		files := splitFiles(path)
		maxBytes := 6000000
		if m := args["max_bytes"]; m != "" {
			fmt.Sscanf(m, "%d", &maxBytes)
		}
		return f.readMany(files, maxBytes)
	case "write":
		content := args["content"]
		if content == "" {
			return "", fmt.Errorf("missing 'content' for write")
		}
		err := os.WriteFile(path, []byte(content), 0644)
		if err != nil {
			return "", err
		}
		return "written successfully", nil
	case "edit":
		return f.editFile(path, args)
	case "info":
		return f.fileInfo(path)
	default:
		return "", fmt.Errorf("unknown fs operation: %s (supported: list, tree, read, read_many, write, edit, info)", op)
	}
}

func (f FileSystemTool) listDir(path string) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Contents of %s:\n\n", path))

	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			b.WriteString(fmt.Sprintf("- %s (error reading info)\n", e.Name()))
			continue
		}
		if e.IsDir() {
			b.WriteString(fmt.Sprintf("📁 %s/\n", e.Name()))
		} else {
			b.WriteString(fmt.Sprintf("📄 %s  (%d bytes)\n", e.Name(), info.Size()))
		}
	}
	return b.String(), nil
}

// func Directory tree
func (f FileSystemTool) tree(root string, maxDepth int) (string, error) {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Directory tree of %s (max depth %d):\n\n", root, maxDepth))

	const maxFilesPerDir = 15 // show only first N files per folder

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		if rel == "." {
			return nil
		}

		name := filepath.Base(path)

		// Skip noisy directories
		if name == ".git" || name == "node_modules" || name == "__pycache__" || name == ".venv" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		depth := strings.Count(rel, string(os.PathSeparator))
		if depth >= maxDepth {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		indent := strings.Repeat("  ", depth)

		if d.IsDir() {
			b.WriteString(fmt.Sprintf("%s📁 %s/\n", indent, name))
			return nil
		}

		// For files: count siblings so we can limit output
		// (simple approach: just print files, truncation handled by caller size limits)
		info, _ := d.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		b.WriteString(fmt.Sprintf("%s📄 %s  (%d bytes)\n", indent, name, size))
		return nil
	})

	if err != nil {
		return b.String(), err
	}

	result := b.String()
	// Hard cap so it doesn't explode agent context
	if len(result) > 20000 {
		result = result[:20000] + "\n\n...[tree truncated for size]...\n"
	}
	return result, nil
}

func (f FileSystemTool) readFile(path string, maxBytes int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	// Skip likely binary files
	if !isMostlyText(data) {
		return fmt.Sprintf("[Binary or non-text file: %s (%d bytes) — skipped]", path, len(data)), nil
	}

	content := string(data)
	truncated := false
	if len(content) > maxBytes {
		content = content[:maxBytes]
		truncated = true
	}

	header := fmt.Sprintf("=== File: %s (%d bytes) ===\n", path, len(data))
	if truncated {
		header += fmt.Sprintf("[Truncated to first %d bytes]\n", maxBytes)
	}

	return header + content, nil
}

func (f FileSystemTool) readMany(files []string, maxBytesPerFile int) (string, error) {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Reading %d files:\n\n", len(files)))

	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		content, err := f.readFile(file, maxBytesPerFile)
		if err != nil {
			b.WriteString(fmt.Sprintf("=== File: %s ===\nError: %v\n\n", file, err))
			continue
		}
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	return b.String(), nil
}

func (f FileSystemTool) editFile(path string, args map[string]string) (string, error) {
	oldSnippet := firstNonEmpty(args["old_snippet"], args["old"], args["find"])
	newSnippet, ok := args["new_snippet"]
	if !ok {
		newSnippet, ok = args["new"]
	}
	if !ok {
		newSnippet, ok = args["replace"]
	}
	if oldSnippet == "" {
		return "", fmt.Errorf("missing 'old_snippet' for edit")
	}
	if !ok {
		return "", fmt.Errorf("missing 'new_snippet' for edit")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !isMostlyText(data) {
		return "", fmt.Errorf("refusing to edit likely binary/non-text file: %s", path)
	}
	content := string(data)

	if err := validateEditChecksum(content, args); err != nil {
		return "", err
	}

	matches := strings.Count(content, oldSnippet)
	if matches != 1 {
		return "", fmt.Errorf("old_snippet must appear exactly once in %s; found %d matches", path, matches)
	}

	updated := strings.Replace(content, oldSnippet, newSnippet, 1)
	if updated == content {
		return fmt.Sprintf("No changes made to %s; old_snippet and new_snippet produce identical content.", path), nil
	}
	diff := unifiedEditHunk(path, content, updated)
	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited successfully\n\n%s", diff), nil
}

func validateEditChecksum(content string, args map[string]string) error {
	startRaw := strings.TrimSpace(args["start_line"])
	endRaw := strings.TrimSpace(args["end_line"])
	checksum := strings.TrimSpace(firstNonEmpty(args["checksum"], args["line_checksum"]))
	if startRaw == "" && endRaw == "" && checksum == "" {
		return nil
	}
	if startRaw == "" || endRaw == "" || checksum == "" {
		return fmt.Errorf("start_line, end_line and checksum must be provided together")
	}

	var startLine, endLine int
	if _, err := fmt.Sscanf(startRaw, "%d", &startLine); err != nil {
		return fmt.Errorf("invalid start_line %q: %w", startRaw, err)
	}
	if _, err := fmt.Sscanf(endRaw, "%d", &endLine); err != nil {
		return fmt.Errorf("invalid end_line %q: %w", endRaw, err)
	}
	if startLine < 1 || endLine < startLine {
		return fmt.Errorf("invalid line range: start_line=%d end_line=%d", startLine, endLine)
	}

	lines := splitLinesKeepEnd(content)
	if endLine > len(lines) {
		return fmt.Errorf("line range %d-%d exceeds file length %d", startLine, endLine, len(lines))
	}
	rangeText := strings.Join(lines[startLine-1:endLine], "")
	sum := sha256.Sum256([]byte(rangeText))
	got := fmt.Sprintf("%x", sum)
	if !strings.EqualFold(checksum, got) {
		return fmt.Errorf("checksum mismatch for lines %d-%d: expected %s, got %s", startLine, endLine, checksum, got)
	}
	return nil
}

func unifiedEditHunk(path, before, after string) string {
	oldLines := splitLinesKeepEnd(before)
	newLines := splitLinesKeepEnd(after)

	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}

	suffix := 0
	for suffix < len(oldLines)-prefix &&
		suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	const contextLines = 3
	oldStart := maxInt(0, prefix-contextLines)
	newStart := maxInt(0, prefix-contextLines)
	oldChangeEnd := len(oldLines) - suffix
	newChangeEnd := len(newLines) - suffix
	oldEnd := minInt(len(oldLines), oldChangeEnd+contextLines)
	newEnd := minInt(len(newLines), newChangeEnd+contextLines)

	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n", path, path)
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oldStart+1, oldEnd-oldStart, newStart+1, newEnd-newStart)

	for i := oldStart; i < prefix; i++ {
		writeDiffLine(&b, " ", oldLines[i])
	}
	for i := prefix; i < oldChangeEnd; i++ {
		writeDiffLine(&b, "-", oldLines[i])
	}
	for i := prefix; i < newChangeEnd; i++ {
		writeDiffLine(&b, "+", newLines[i])
	}
	for i := oldChangeEnd; i < oldEnd; i++ {
		writeDiffLine(&b, " ", oldLines[i])
	}
	return strings.TrimRight(b.String(), "\n")
}

func splitLinesKeepEnd(s string) []string {
	if s == "" {
		return []string{}
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func writeDiffLine(b *strings.Builder, prefix, line string) {
	b.WriteString(prefix)
	b.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		b.WriteString("\n")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (f FileSystemTool) fileInfo(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Info for: %s\n", path))
	b.WriteString(fmt.Sprintf("Size:     %d bytes\n", info.Size()))
	b.WriteString(fmt.Sprintf("Mode:     %s\n", info.Mode()))
	b.WriteString(fmt.Sprintf("ModTime:  %s\n", info.ModTime().Format("2006-01-02 15:04:05")))
	b.WriteString(fmt.Sprintf("IsDir:    %v\n", info.IsDir()))
	return b.String(), nil
}

// --- helpers ---

func splitFiles(pathArg string) []string {
	// support both comma and space separated
	pathArg = strings.ReplaceAll(pathArg, ",", " ")
	return strings.Fields(pathArg)
}

func isMostlyText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	// Check a sample for invalid UTF-8 or lots of null bytes
	sample := data
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	if !utf8.Valid(sample) {
		return false
	}
	nullCount := 0
	for _, b := range sample {
		if b == 0 {
			nullCount++
		}
	}
	return nullCount < 5
}

//
// ======================
// Shell Tool
// ======================
// WARNING: This can be dangerous. Use only in trusted environments.
//

type ShellTool struct{}

func (s ShellTool) Name() string { return "shell" }

func (s ShellTool) Execute(ctx context.Context, args map[string]string) (string, error) {
	cmdStr := args["cmd"]
	if cmdStr == "" {
		return "", fmt.Errorf("missing 'cmd' argument")
	}

	// If the model tries to call 'fs ...' via shell, intercept and execute FS directly
	trimmed := strings.TrimSpace(cmdStr)
	if strings.HasPrefix(trimmed, "fs ") {
		// Try to parse: fs <op> path=<path> content='...'
		// Support content in single or double quotes and multi-line.
		reSingle := regexp.MustCompile(`(?s)^fs\s+(\w+)\s+path=([^\s]+)\s+content='(.*)'`)
		reDouble := regexp.MustCompile(`(?s)^fs\s+(\w+)\s+path=([^\s]+)\s+content="(.*)"`)
		if m := reSingle.FindStringSubmatch(trimmed); len(m) == 4 {
			op := m[1]
			path := m[2]
			content := m[3]
			fsArgs := map[string]string{"op": op, "path": path, "content": content}
			// execute fs directly
			var f FileSystemTool
			return f.Execute(ctx, fsArgs)
		}
		if m := reDouble.FindStringSubmatch(trimmed); len(m) == 4 {
			op := m[1]
			path := m[2]
			content := m[3]
			fsArgs := map[string]string{"op": op, "path": path, "content": content}
			var f FileSystemTool
			return f.Execute(ctx, fsArgs)
		}
		// Fallback: try to parse simple fs write path content without quotes
		reSimple := regexp.MustCompile(`^fs\s+(\w+)\s+path=([^\s]+)\s+content=(.+)$`)
		if m := reSimple.FindStringSubmatch(trimmed); len(m) == 4 {
			op := m[1]
			path := m[2]
			content := strings.TrimSpace(m[3])
			content = strings.Trim(content, "'\"")
			fsArgs := map[string]string{"op": op, "path": path, "content": content}
			var f FileSystemTool
			return f.Execute(ctx, fsArgs)
		}
		// If parsing failed, fall back to executing shell as usual below
	}

	// Very basic safety: block some dangerous commands
	dangerous := []string{"rm -rf", "mkfs", "dd if=", ":(){", "shutdown", "reboot"}
	lower := strings.ToLower(cmdStr)
	for _, d := range dangerous {
		if strings.Contains(lower, d) {
			return "", fmt.Errorf("command blocked for safety: %s", cmdStr)
		}
	}

	cmd := runShell(ctx, cmdStr)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("command failed: %w", err)
	}

	return string(output), nil
}
