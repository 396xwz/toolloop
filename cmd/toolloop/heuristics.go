package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/396xwz/toolloop/internal/memory"
)

func mapFSOp(s string) string {
	s = strings.ToLower(strings.Trim(s, "\"' "))
	switch s {
	case "read", "readfile", "read_file", "get", "cat":
		return "read"
	case "write", "writefile", "write_file", "put":
		return "write"
	case "edit", "replace", "patch":
		return "edit"
	case "list", "ls", "dir":
		return "list"
	case "tree":
		return "tree"
	case "info", "stat":
		return "info"
	case "read_many", "readmany":
		return "read_many"
	default:
		return s
	}
}

func normalizeFSArgs(args map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range args {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		v = strings.Trim(v, "\"'`")
		v = strings.ReplaceAll(v, "</parameter>", "")
		v = strings.ReplaceAll(v, "<parameter>", "")
		if i := strings.Index(strings.ToLower(v), "<parameter"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		out[k] = strings.TrimSpace(v)
	}

	if op, ok := out["op"]; ok {
		out["op"] = mapFSOp(op)
	} else if op, ok := out["operation"]; ok {
		out["op"] = mapFSOp(op)
		delete(out, "operation")
	} else if op, ok := out["action"]; ok {
		out["op"] = mapFSOp(op)
		delete(out, "action")
	}

	if _, ok := out["path"]; !ok {
		for _, k := range []string{"file", "filepath", "filename", "name"} {
			if v, ok := out[k]; ok && v != "" {
				out["path"] = v
				break
			}
		}
	}

	if op := out["op"]; op != "" {
		fields := strings.Fields(op)
		if len(fields) > 0 {
			out["op"] = mapFSOp(fields[0])
		}
	}
	if p := out["path"]; p != "" {
		if i := strings.Index(p, " path:"); i > 0 {
			p = p[:i]
		}
		if i := strings.Index(p, "\n"); i > 0 {
			p = p[:i]
		}
		out["path"] = strings.Trim(p, "\"' ")
	}
	return out
}

func argsLookCorrupt(args map[string]string) bool {
	for _, v := range args {
		if strings.Contains(v, "</parameter>") || strings.Contains(v, "<parameter") {
			return true
		}
	}
	if op, ok := args["op"]; ok && (strings.Contains(op, "\"") || strings.Contains(op, "\n")) {
		return true
	}
	return false
}

func hasMinimumArgs(name string, args map[string]string) bool {
	switch strings.ToLower(name) {
	case "fs":
		return args["op"] != "" && args["path"] != ""
	case "shell":
		return args["cmd"] != ""
	case "browser":
		return args["url"] != ""
	case "web_search":
		return args["query"] != ""
	case "scrape":
		return args["url"] != ""
	case "python":
		return args["path"] != "" || args["code"] != ""
	default:
		return len(args) > 0
	}
}

func parseToolJSON(messageContent string) (toolName string, args map[string]string, err error) {
	raw := cleanJSON(messageContent)
	if raw == "" {
		return "", nil, fmt.Errorf("empty content")
	}
	var bag map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &bag); err != nil {
		return "", nil, fmt.Errorf("json: %w", err)
	}

	toolName = firstString(bag, "tool", "tool_name", "name")
	toolName = strings.TrimSpace(strings.ToLower(toolName))
	// Infer fs when model emits bare op/path/content (common with some fine-tunes)
	if toolName == "" || toolName == "none" {
		op := rawToString(bag["op"])
		if op == "" {
			op = rawToString(bag["operation"])
		}
		path := rawToString(bag["path"])
		if path == "" {
			path = rawToString(bag["file"])
		}
		if op != "" && path != "" {
			toolName = "fs"
		}
	}

	if toolName == "" || toolName == "none" {
		return "", nil, fmt.Errorf("no tool name in json")
	}

	args = map[string]string{}
	if rawArgs, ok := bag["args"]; ok {
		mergeArgs(args, rawArgs)
	}
	if rawArgs, ok := bag["arguments"]; ok {
		mergeArgs(args, rawArgs)
	}
	for _, k := range []string{
		"op", "operation", "action",
		"path", "file", "filepath", "filename",
		"content", "cmd", "command",
		"url", "query", "depth", "output", "mode", "selector", "timeout", "wait", "max_chars",
		"code", "cwd",
	} {
		if v := rawToString(bag[k]); v != "" {
			args[k] = v
		}
	}
	if len(args) == 0 {
		return toolName, args, fmt.Errorf("no args in json")
	}

	if toolName == "fs" {
		args = normalizeFSArgs(args)
	}
	if toolName == "shell" {
		if _, ok := args["cmd"]; !ok {
			if c := args["command"]; c != "" {
				args["cmd"] = c
			}
		}
	}
	return toolName, args, nil
}

// forceFSArgsFromContent accepts:
//
//	{"op":"write","path":"sample.go","content":"..."}
//	even with no "tool" field, including ```json fences.
func forceFSArgsFromContent(content string) (map[string]string, bool) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, false
	}
	raw := cleanJSON(content)
	// If model added prose around JSON, try to slice object
	if !strings.HasPrefix(raw, "{") {
		if i := strings.Index(raw, "{"); i >= 0 {
			if j := strings.LastIndex(raw, "}"); j > i {
				raw = raw[i : j+1]
			}
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return nil, false
	}

	var bare map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &bare); err != nil {
		return nil, false
	}
	a := normalizeArgs(bare)
	a = normalizeFSArgs(a)

	op := a["op"]
	path := a["path"]
	if op == "" || path == "" {
		return nil, false
	}
	if argsLookCorrupt(a) {
		return nil, false
	}
	// write should keep content
	return a, true
}

// parseKeyValueArgs parses tokens like: key=value key2='some val' key3="other"
// respects single/double quotes and simple escaping; supports multi-line single-quoted values
func parseKeyValueArgs(s string) map[string]string {
	out := make(map[string]string)
	// regex captures key and one of three value groups: single-quoted, double-quoted, or unquoted
	re := regexp.MustCompile(`(?s)([A-Za-z0-9_\-]+)=(?:'([^']*)'|"([^"]*)"|([^\s]+))`)
	matches := re.FindAllStringSubmatch(s, -1)
	for _, m := range matches {
		k := m[1]
		var v string
		if m[2] != "" {
			v = m[2]
		} else if m[3] != "" {
			v = m[3]
		} else {
			v = m[4]
		}
		out[k] = v
	}
	return out
}

// extractFSCommandFromText looks for lines like: fs write path=sample.go content='...'
// returns args map if found
func extractFSCommandFromText(text string) (map[string]string, bool) {
	// 1) Look for a line starting with fs
	reLine := regexp.MustCompile(`(?m)^\s*fs\s+(.+)$`)
	if m := reLine.FindStringSubmatch(text); len(m) >= 2 {
		args := parseKeyValueArgs(m[1])
		if len(args) > 0 {
			return args, true
		}
	}

	// 2) Look inside code fences for a fs invocation
	reFence := regexp.MustCompile("(?s)```[a-zA-Z]*\\s*(.*?)\\s*```")
	if m := reFence.FindStringSubmatch(text); len(m) >= 2 {
		inside := m[1]
		if m2 := reLine.FindStringSubmatch(inside); len(m2) >= 2 {
			args := parseKeyValueArgs(m2[1])
			if len(args) > 0 {
				return args, true
			}
		}
	}

	// 3) Fallback: find first occurrence of "fs " anywhere and take the rest of the line
	reAnywhere := regexp.MustCompile(`(?s)fs\s+([^\n\r]+)`)
	if m := reAnywhere.FindStringSubmatch(text); len(m) >= 2 {
		args := parseKeyValueArgs(m[1])
		if len(args) > 0 {
			return args, true
		}
	}

	return nil, false
}

func firstString(bag map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if s := rawToString(bag[k]); s != "" {
			return s
		}
	}
	return ""
}

func rawToString(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var v interface{}
	if err := json.Unmarshal(r, &v); err == nil {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func mergeArgs(dst map[string]string, raw json.RawMessage) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	for k, v := range m {
		switch t := v.(type) {
		case string:
			dst[k] = t
		case float64:
			if t == float64(int64(t)) {
				dst[k] = fmt.Sprintf("%d", int64(t))
			} else {
				dst[k] = fmt.Sprint(t)
			}
		case bool:
			dst[k] = fmt.Sprint(t)
		default:
			b, _ := json.Marshal(t)
			dst[k] = string(b)
		}
	}
}

func normalizeArgs(raw map[string]interface{}) map[string]string {
	args := make(map[string]string)
	if raw == nil {
		return args
	}
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			args[k] = val
		case []interface{}:
			parts := make([]string, 0, len(val))
			for _, item := range val {
				parts = append(parts, fmt.Sprint(item))
			}
			args[k] = strings.Join(parts, " ")
		case []string:
			args[k] = strings.Join(val, " ")
		default:
			args[k] = fmt.Sprint(val)
		}
	}
	return args
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func cleanJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	re := regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")
	matches := re.FindStringSubmatch(raw)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}

const toolInstructions = `Available tools:

1. fs - filesystem operations
   args: op (list|tree|read|read_many|info|write|edit), path, depth, max_bytes, content, old_snippet, new_snippet, start_line, end_line, checksum
   for op=edit: read the file first, require old_snippet to appear exactly once, replace it with new_snippet, and return a diff hunk; if old_snippet appears 0 or 2+ times, do not write
   for op=edit with start_line/end_line/checksum: checksum is SHA-256 hex of those original lines; reject the edit if it does not match
2. shell - run a shell command
   args: cmd
3. browser - fetch a URL and return text content
   args: url
4. web_search - search the web
   args: query
5. scrape - use Scrapling to extract dynamic website content into markdown/text
   args: url, output, mode, selector, timeout, wait, max_chars
6. python - run a Python script or inline code with the project's interpreter
   args: path (script file), or code (inline source), args (extra CLI args), cwd, timeout (seconds, default 60)
   use this instead of "shell cmd=python3 ..." so the project's own .venv interpreter is used

To call a tool, reply with ONLY a single JSON object, no prose, no markdown fences:
{"tool":"fs","args":{"op":"read","path":"README.md"}}

Rules:
- Emit exactly one JSON object and nothing else when calling a tool.
- All arg values must be plain strings.
- For fs op=write, put the complete file body in "content".
- For dynamic sports pages or pages where browser returns app shell HTML, prefer scrape over browser.
- To run a Python script or code, use the python tool, not shell.
- If no tool is needed, reply with a short plain-text note instead (no JSON).`

func filterRAGChunks(chunks []memory.RAGChunk, minScore float64) []memory.RAGChunk {
	if minScore <= 0 {
		return chunks
	}
	out := make([]memory.RAGChunk, 0, len(chunks))
	for _, c := range chunks {
		if c.Score >= minScore {
			out = append(out, c)
		}
	}
	return out
}

func formatRAGContext(chunks []memory.RAGChunk) string {
	if len(chunks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Retrieved context (top-k):\n")
	for i, c := range chunks {
		b.WriteString(fmt.Sprintf("\n[%d] %s (chunk %d, score %.3f)\n%s\n",
			i+1, c.Path, c.ChunkIdx, c.Score, truncate(c.Text, 1500)))
	}
	return b.String()
}

func formatMemoryContext(items []memory.MemoryItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Recent memory:\n")
	for _, it := range items {
		b.WriteString(fmt.Sprintf("- [%s] %s\n", it.Kind, truncate(it.Content, 400)))
	}
	return b.String()
}

func isKnowledgeQuestion(s string) bool {
	d := strings.ToLower(strings.TrimSpace(s))
	if strings.ContainsAny(d, "/\\") || strings.Contains(d, "http") {
		return false
	}
	keys := []string{
		"example", "what is", "what's", "how do", "how to",
		"write a", "show me", "explain", "why", "difference between",
		"+", "calculate", "meaning of", "can you", "who are",
	}
	for _, k := range keys {
		if strings.Contains(d, k) {
			return true
		}
	}
	if strings.HasSuffix(d, "?") && !strings.Contains(d, "shared-claude") {
		return true
	}
	return false
}

// ─── PATH / FS GUARDS ───────────────────────────────────────────────

func extractPaths(s string) []string {
	var paths []string
	for _, w := range strings.Fields(s) {
		w = strings.Trim(w, `"'.,;:!?()[]{}`)
		if w == "" {
			continue
		}
		if strings.Contains(w, "/") || strings.HasPrefix(w, ".") || strings.Contains(w, ".") {
			paths = append(paths, w)
		}
	}
	return paths
}

func isAllowedFSPath(taskDesc, path string) bool {
	paths := extractPaths(taskDesc)
	if len(paths) == 0 {
		return true
	}
	pathNorm := strings.TrimPrefix(path, "./")
	base := pathNorm
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	d := strings.ToLower(taskDesc)
	if strings.Contains(d, strings.ToLower(pathNorm)) || strings.Contains(d, strings.ToLower(base)) {
		return true
	}
	for _, p := range paths {
		p = strings.TrimPrefix(p, "./")
		if p == pathNorm || strings.HasPrefix(pathNorm, p) || strings.HasPrefix(p, pathNorm) {
			return true
		}
	}
	return false
}

func sanitizePath(path string) string {
	path = strings.TrimSpace(path)
	for len(path) > 0 && path[0] > 127 {
		path = path[1:]
	}
	return strings.TrimSpace(path)
}

func taskAllowsFSOp(taskDesc, op string) bool {
	d := strings.ToLower(taskDesc)
	switch op {
	case "list":
		return strings.Contains(d, "list") || strings.Contains(d, "ls")
	case "tree":
		return strings.Contains(d, "tree")
	case "read", "read_many":
		return strings.Contains(d, "read") ||
			strings.Contains(d, "open") ||
			strings.Contains(d, "file") ||
			strings.Contains(d, "show") ||
			strings.Contains(d, "cat")
	case "info":
		return strings.Contains(d, "info") || strings.Contains(d, "stat")
	case "write":
		return strings.Contains(d, "write") || strings.Contains(d, "create") || strings.Contains(d, "save")
	case "edit":
		return strings.Contains(d, "edit") || strings.Contains(d, "replace") || strings.Contains(d, "patch") || strings.Contains(d, "modify")
	default:
		return true
	}
}

// ─── GENERIC HELPERS ────────────────────────────────────────────────
