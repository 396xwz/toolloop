package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/396xwz/toolloop/internal/agent"
	modelpkg "github.com/396xwz/toolloop/internal/model"
	"github.com/ollama/ollama/api"
)

const (
	defaultModel     = "llama.cpp"
	embedModel       = "nomic-embed-text"
	memoryDBPath     = "agent_memory.db"
	ragDBPath        = "agent_rag.db"
	Timeout          = 240
	defaultIndexRoot = "documents"

	maxStepResultChars         = 4000
	maxFinalAnswerHistoryChars = 24000
	gitSectionOutputLimit      = 64 * 1024
	gitRenderedOutputLimit     = 128 * 1024
	gitErrorOutputLimit        = 4 * 1024
	gitTruncationMarker        = "\n...[truncated]\n"
	gitRevisionMinimumVersion  = "git 2.30 or later is required for /git <revision>"

	// Default system prompt when no custom prompt is provided
	defaultSystemPrompt = `You are a local tool-using agent.
Use tools when needed to complete the task.
Prefer the exact path and operation mentioned in the task.
For directories use fs op=list or op=tree.
For files use fs op=read or fs op=read_many.
For Python scripts or Python code, use the python tool. Example: python path=python/wnba2.py.
The python tool can run scripts under the repo's python/ directory using the project's .venv interpreter.
When calling fs, always set op and path as plain strings (e.g. op=read, path=README.md).
If you already have enough information from previous steps or retrieved context, do not call any tool.`
)

var (
	gitCommandTimeout = 10 * time.Second
	gitVersionPattern = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)(?:[.-][0-9A-Za-z][0-9A-Za-z.+-]*)?(?: \([^\r\n]*\))?$`)
)

type OllamaModel struct {
	Client       *api.Client
	Model        string
	SystemPrompt string // custom system prompt if provided via -prompt flag
}

func (m *OllamaModel) ModelName() string {
	if m.Model != "" {
		return m.Model
	}
	return "qwen2.5:14b"
}

func (m *OllamaModel) SetModel(name string) { m.Model = name }

func (m *OllamaModel) SystemPromptValue() string { return m.SystemPrompt }

func (m *OllamaModel) SetSystemPrompt(prompt string) { m.SystemPrompt = prompt }

var debugMode bool

func debugf(format string, a ...interface{}) {
	if !debugMode {
		return
	}
	fmt.Printf(format, a...)
}

type ModelResponse struct {
	Plan     string                 `json:"plan"`
	ToolName string                 `json:"tool_name"`
	Args     map[string]interface{} `json:"args"`
}

type ChatModel = modelpkg.ChatModel

// ─── FS / TOOL ARG HELPERS ──────────────────────────────────────────

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

func resolveToolFromResponse(msg api.Message) (name string, args map[string]string, ok bool) {
	if len(msg.ToolCalls) > 0 {
		tc := msg.ToolCalls[0]
		name = strings.TrimSpace(tc.Function.Name)
		args = toolCallArgsToMap(tc.Function.Arguments)
		if name == "fs" {
			args = normalizeFSArgs(args)
		}
		if name == "shell" {
			if _, has := args["cmd"]; !has {
				if c := args["command"]; c != "" {
					args["cmd"] = c
				}
			}
		}
		if name != "" && !argsLookCorrupt(args) && hasMinimumArgs(name, args) {
			return name, args, true
		}
	}

	content := strings.TrimSpace(msg.Content)
	if content != "" {
		if tn, fa, err := parseToolJSON(content); err == nil {
			if tn == "fs" {
				fa = normalizeFSArgs(fa)
			}
			if tn == "shell" {
				if _, has := fa["cmd"]; !has {
					if c := fa["command"]; c != "" {
						fa["cmd"] = c
					}
				}
			}
			if !argsLookCorrupt(fa) && hasMinimumArgs(tn, fa) {
				return tn, fa, true
			}
		}

		if strings.HasPrefix(content, "{") {
			cleaned := cleanJSON(content)
			var parsed ModelResponse
			if err := json.Unmarshal([]byte(cleaned), &parsed); err == nil {
				if parsed.ToolName != "" && parsed.ToolName != "none" {
					a := normalizeArgs(parsed.Args)
					if parsed.ToolName == "fs" {
						a = normalizeFSArgs(a)
					}
					if !argsLookCorrupt(a) && hasMinimumArgs(parsed.ToolName, a) {
						return parsed.ToolName, a, true
					}
				}
			}
		}
	}

	// Last resort: native after normalize only
	if len(msg.ToolCalls) > 0 {
		tc := msg.ToolCalls[0]
		name = strings.TrimSpace(tc.Function.Name)
		args = toolCallArgsToMap(tc.Function.Arguments)
		if name == "fs" {
			args = normalizeFSArgs(args)
		}
		if name != "" && !argsLookCorrupt(args) && hasMinimumArgs(name, args) {
			return name, args, true
		}
	}
	return "", nil, false
}

// ─── RAG / MEMORY HELPERS ───────────────────────────────────────────

func filterRAGChunks(chunks []agent.RAGChunk, minScore float64) []agent.RAGChunk {
	if minScore <= 0 {
		return chunks
	}
	out := make([]agent.RAGChunk, 0, len(chunks))
	for _, c := range chunks {
		if c.Score >= minScore {
			out = append(out, c)
		}
	}
	return out
}

func formatRAGContext(chunks []agent.RAGChunk) string {
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

func formatMemoryContext(items []agent.MemoryItem) string {
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

func isWeakREPLInput(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	// allow direct tool lines
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "fs ") || strings.HasPrefix(low, "shell ") ||
		strings.HasPrefix(low, "browser ") || strings.HasPrefix(low, "scrape ") ||
		strings.HasPrefix(low, "python ") {
		return false
	}
	if utf8.RuneCountInString(s) < 4 {
		return true
	}
	letters := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			letters++
		}
	}
	if letters < 3 {
		return true
	}
	switch strings.ToLower(s) {
	case "foo", "bar", "test", "asdf", "hello", "hi", "hey", "ok", "yes", "no":
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

// ─── OLLAMA TOOLS ───────────────────────────────────────────────────

func buildOllamaTools() api.Tools {
	fsProps := api.NewToolPropertiesMap()
	fsProps.Set("op", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Operation: list | tree | read | read_many | info | write | edit",
	})
	fsProps.Set("path", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "File or directory path. For read_many, space-separated paths.",
	})
	fsProps.Set("depth", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Max depth for tree (e.g. \"2\")",
	})
	fsProps.Set("max_bytes", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Max bytes to read from a file",
	})
	fsProps.Set("content", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Content for write operation",
	})
	fsProps.Set("old_snippet", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "For edit: unique substring to replace; must appear exactly once",
	})
	fsProps.Set("new_snippet", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "For edit: replacement text",
	})
	fsProps.Set("start_line", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Optional for edit: first line covered by checksum",
	})
	fsProps.Set("end_line", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Optional for edit: last line covered by checksum",
	})
	fsProps.Set("checksum", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Optional for edit: SHA-256 hex of original start_line..end_line text",
	})

	shellProps := api.NewToolPropertiesMap()
	shellProps.Set("cmd", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Shell command to execute",
	})

	browserProps := api.NewToolPropertiesMap()
	browserProps.Set("url", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "URL to fetch",
	})

	webProps := api.NewToolPropertiesMap()
	webProps.Set("query", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Search query",
	})

	scrapeProps := api.NewToolPropertiesMap()
	scrapeProps.Set("url", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "URL to scrape",
	})
	scrapeProps.Set("output", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Output markdown/text file path",
	})
	scrapeProps.Set("mode", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Scrape mode: fetch | stealthy-fetch | get",
	})
	scrapeProps.Set("selector", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Optional CSS selector",
	})
	scrapeProps.Set("timeout", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Timeout in seconds",
	})
	scrapeProps.Set("wait", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Optional wait time/condition",
	})
	scrapeProps.Set("max_chars", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Maximum characters to return",
	})

	pythonProps := api.NewToolPropertiesMap()
	pythonProps.Set("path", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Path to a Python script to run",
	})
	pythonProps.Set("code", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Inline Python source to run (used if path is not set)",
	})
	pythonProps.Set("args", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Extra command-line arguments, space or comma separated",
	})
	pythonProps.Set("cwd", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Working directory to run the script from",
	})
	pythonProps.Set("timeout", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Timeout in seconds (default 60)",
	})

	return api.Tools{
		{
			Type: "function",
			Function: api.ToolFunction{
				Name:        "fs",
				Description: "Filesystem operations: list, tree, read, read_many, info, write, edit",
				Parameters: api.ToolFunctionParameters{
					Type:       "object",
					Required:   []string{"op", "path"},
					Properties: fsProps,
				},
			},
		},
		{
			Type: "function",
			Function: api.ToolFunction{
				Name:        "shell",
				Description: "Run a shell command",
				Parameters: api.ToolFunctionParameters{
					Type:       "object",
					Required:   []string{"cmd"},
					Properties: shellProps,
				},
			},
		},
		{
			Type: "function",
			Function: api.ToolFunction{
				Name:        "browser",
				Description: "Fetch a URL and return text content",
				Parameters: api.ToolFunctionParameters{
					Type:       "object",
					Required:   []string{"url"},
					Properties: browserProps,
				},
			},
		},
		{
			Type: "function",
			Function: api.ToolFunction{
				Name:        "web_search",
				Description: "Search the web for a query",
				Parameters: api.ToolFunctionParameters{
					Type:       "object",
					Required:   []string{"query"},
					Properties: webProps,
				},
			},
		},
		{
			Type: "function",
			Function: api.ToolFunction{
				Name:        "scrape",
				Description: "Use Scrapling to extract dynamic website content into markdown/text",
				Parameters: api.ToolFunctionParameters{
					Type:       "object",
					Required:   []string{"url"},
					Properties: scrapeProps,
				},
			},
		},
		{
			Type: "function",
			Function: api.ToolFunction{
				Name:        "python",
				Description: "Run a Python script (path) or inline code (code) using the project's .venv interpreter",
				Parameters: api.ToolFunctionParameters{
					Type:       "object",
					Required:   []string{},
					Properties: pythonProps,
				},
			},
		},
	}
}

func toolCallArgsToMap(args api.ToolCallFunctionArguments) map[string]string {
	raw := args.ToMap()
	if raw == nil {
		b, err := json.Marshal(args)
		if err != nil {
			return map[string]string{}
		}
		var m map[string]interface{}
		if err := json.Unmarshal(b, &m); err != nil {
			return map[string]string{}
		}
		return normalizeArgs(m)
	}
	return normalizeArgs(raw)
}

// ─── PLAN NEXT STEP ─────────────────────────────────────────────────

func (m *OllamaModel) PlanNextStep(ctx context.Context, task *agent.Task) (*agent.Step, error) {
	history := ""
	for _, s := range task.Steps {
		toolInfo := "none"
		if s.ToolCall != nil {
			toolInfo = fmt.Sprintf("%s %v", s.ToolCall.Name, s.ToolCall.Args)
		}
		history += fmt.Sprintf("- Step %d: %s | Tool: %s | Result: %s\n",
			s.Index, s.Plan, toolInfo, truncate(s.Result, 8000))
	}
	if history == "" {
		history = "(no previous steps)"
	}

	ragBlock := ""
	memBlock := ""
	if task.RAG != nil {
		if chunks, err := task.RAG.Retrieve(ctx, task.Description, 5); err == nil {
			ragBlock = formatRAGContext(chunks)
		}
	}
	if task.Memory != nil {
		if items, err := task.Memory.Recent(ctx, 5); err == nil {
			memBlock = formatMemoryContext(items)
		}
	}

	system := `You are a local tool-using agent.
Use tools when needed to complete the task.
Prefer the exact path and operation mentioned in the task.
For directories use fs op=list or op=tree.
For files use fs op=read or op=read_many.
	For Python scripts or Python code, use the python tool. Example: python path=python/wnba2.py.
	The python tool can run scripts under the repo's python/ directory using the project's .venv interpreter.
	When calling fs, always set op and path as plain strings (e.g. op=read, path=README.md).
	If you already have enough information from previous steps or retrieved context, do not call any tool.`

	user := fmt.Sprintf(`Task:
%s

Previous steps:
%s

%s

%s

Call a tool if needed. Otherwise respond with a short note that no tool is required.`,
		task.Description, history, memBlock, ragBlock)

	modelName := m.ModelName()
	stream := false
	// Build messages: include system, then assistant messages for previous tool results, then the user prompt
	messages := []api.Message{{Role: "system", Content: system}}
	for _, s := range task.Steps {
		if s.ToolCall != nil && s.ToolCall.Name != "" {
			// include tool result as assistant message
			res := truncate(s.Result, 8000)
			messages = append(messages, api.Message{Role: "assistant", Content: fmt.Sprintf("Tool %s returned: %s", s.ToolCall.Name, res)})
		}
	}
	messages = append(messages, api.Message{Role: "user", Content: user})
	req := &api.ChatRequest{
		Model:    modelName,
		Messages: messages,
		Tools:    buildOllamaTools(),
		Stream:   &stream,
		Options: map[string]interface{}{
			"temperature": 0.0,
		},
	}

	var last api.ChatResponse
	var toolful []api.ChatResponse
	debugf("DEBUG: starting Chat request to model %s\n", modelName)
	ctx2, cancel := context.WithTimeout(ctx, Timeout*time.Second)
	defer cancel()
	err := m.Client.Chat(ctx2, req, func(resp api.ChatResponse) error {
		debugf("DEBUG: received chat chunk content_len=%d toolcalls=%d\n", len(resp.Message.Content), len(resp.Message.ToolCalls))
		last = resp
		if len(resp.Message.ToolCalls) > 0 {
			toolful = append(toolful, resp)
			debugf("DEBUG: appended toolful response (now %d)\n", len(toolful))
		}
		return nil
	})
	if err != nil {
		debugf("DEBUG: chat error: %v\n", err)
		if last.Message.Content == "" && len(toolful) == 0 {
			return nil, err
		}
		debugf("DEBUG: proceeding with partial response\n")
	}
	// Prefer a fs write tool call if any toolful responses existed
	if len(toolful) > 0 {
		// search for any toolful resp that resolves to fs write
		for _, resp := range toolful {
			if name, args, ok := resolveToolFromResponse(resp.Message); ok {
				if name == "fs" && args["op"] == "write" {
					last = resp
					debugf("DEBUG: selected fs write toolful response\n")
					goto selected_toolful
				}
			}
		}
		// otherwise pick the last toolful
		last = toolful[len(toolful)-1]
		debugf("DEBUG: selected last toolful response (no fs write found)\n")
	selected_toolful:
		// no-op label used for goto
	} else {
		debugf("DEBUG: no toolful responses; falling back to last message content\n")
	}

	step := &agent.Step{
		Index: len(task.Steps),
		Plan:  strings.TrimSpace(last.Message.Content),
	}

	name, args, ok := resolveToolFromResponse(last.Message)
	if !ok {
		if step.Plan == "" {
			step.Plan = "No more tools needed"
		}
		return step, nil
	}

	if name == "fs" {
		args = normalizeFSArgs(args)
		op := args["op"]
		path := sanitizePath(args["path"])
		args["path"] = path

		if op != "" && !taskAllowsFSOp(task.Description, op) {
			d := strings.ToLower(task.Description)
			if !(strings.Contains(d, "read") || strings.Contains(d, "open") ||
				strings.Contains(d, "file") || strings.Contains(d, "list") ||
				strings.Contains(d, "tree") || strings.Contains(d, "write")) {
				step.Plan = "Blocked: fs op not allowed by task text"
				return step, nil
			}
		}
		if path != "" && !isAllowedFSPath(task.Description, path) {
			step.Plan = "Blocked: path not mentioned in task"
			return step, nil
		}
	}

	step.ToolCall = &agent.ToolCall{Name: name, Args: args}
	if step.Plan == "" || strings.HasPrefix(strings.TrimSpace(step.Plan), "{") {
		step.Plan = fmt.Sprintf("call %s %v", name, args)
	}
	return step, nil
}

// ─── FINAL ANSWER ───────────────────────────────────────────────────

func (m *OllamaModel) GenerateFinalAnswer(ctx context.Context, task *agent.Task) (string, error) {
	history := ""
	usedTools := false
	for _, s := range task.Steps {
		if s.ToolCall != nil && s.ToolCall.Name != "" {
			usedTools = true
		}
		history += fmt.Sprintf("Step %d:\nPlan: %s\nResult:\n%s\n\n",
			s.Index, s.Plan, truncate(s.Result, 1200000))
	}
	if history == "" {
		history = "(no steps were executed)"
	}

	ragBlock := ""
	if task.RAG != nil {
		if chunks, err := task.RAG.Retrieve(ctx, task.Description, 5); err == nil {
			chunks = filterRAGChunks(chunks, 0.35)
			ragBlock = formatRAGContext(chunks)
		}
	}

	var prompt string
	if usedTools || ragBlock != "" {
		prompt = fmt.Sprintf(`The user asked:
%s

Tool/step data:
%s
%s

Answer using ONLY the data above.
Do NOT invent files or folders.
Do NOT output JSON.`, task.Description, history, ragBlock)
	} else if isKnowledgeQuestion(task.Description) {
		prompt = fmt.Sprintf(`The user asked:
%s

No external tools were required.
Answer helpfully from your knowledge.
If it's a coding question, include a short, correct example.
Do NOT mention tools, RAG, or steps.`, task.Description)
	} else {
		prompt = fmt.Sprintf(`The user asked:
%s

No tool data was retrieved.
If the request is unclear, ask a clarifying question.
Otherwise answer briefly from knowledge.
Do NOT invent local files or directories.`, task.Description)
	}

	modelName := m.ModelName()
	stream := false
	req := &api.ChatRequest{
		Model: modelName,
		Messages: []api.Message{
			{Role: "user", Content: prompt},
		},
		Stream: &stream,
		Options: map[string]interface{}{
			"temperature": 0.0,
		},
	}

	var fullResponse string
	var lastResp api.ChatResponse
	debugf("DEBUG: starting GenerateFinalAnswer Chat to model %s\n", m.Model)
	start := time.Now()
	ctx2, cancel := context.WithTimeout(ctx, Timeout*time.Second)
	defer cancel()
	err := m.Client.Chat(ctx2, req, func(resp api.ChatResponse) error {
		debugf("DEBUG: finalAnswer chunk content_len=%d toolcalls=%d\n", len(resp.Message.Content), len(resp.Message.ToolCalls))
		fullResponse += resp.Message.Content
		lastResp = resp
		return nil
	})
	if err != nil {
		debugf("DEBUG: GenerateFinalAnswer chat error: %v elapsed=%s deadlineExceeded=%v\n", err, time.Since(start), errors.Is(err, context.DeadlineExceeded))
		if lastResp.Message.Content == "" {
			return "", err
		}
		debugf("DEBUG: proceeding with partial final answer (len=%d)\n", len(fullResponse))
	}
	answer := strings.TrimSpace(fullResponse)
	if strings.HasPrefix(answer, "{") {
		return "The model returned structured data instead of a final answer. Raw output:\n" + answer, nil
	}
	return answer, nil
}

// ─── REPL ───────────────────────────────────────────────────────────

func tryDirectToolLine(ctx context.Context, line string, registry agent.ToolRegistry) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	head := strings.ToLower(fields[0])

	switch head {
	case "fs":
		if len(fields) < 3 {
			fmt.Println("usage: fs <op> <path> [depth]")
			return true
		}
		args := map[string]string{
			"op":   mapFSOp(fields[1]),
			"path": fields[2],
		}
		if len(fields) >= 4 && args["op"] == "tree" {
			args["depth"] = fields[3]
		}
		args = normalizeFSArgs(args)
		tool, ok := registry.Get("fs")
		if !ok {
			fmt.Println("fs tool not registered")
			return true
		}
		fmt.Printf("  → Tool fs executed (args: %v)\n", args)
		out, err := tool.Execute(ctx, args)
		if err != nil {
			fmt.Println("FS error:", err)
			return true
		}
		fmt.Println(out)
		return true

	case "shell":
		cmd := strings.TrimSpace(line[len(fields[0]):])
		if cmd == "" {
			fmt.Println("usage: shell <command>")
			return true
		}
		tool, ok := registry.Get("shell")
		if !ok {
			return true
		}
		out, err := tool.Execute(ctx, map[string]string{"cmd": cmd})
		if err != nil {
			fmt.Println("Shell error:", err)
			return true
		}
		fmt.Println(out)
		return true

	case "browser":
		if len(fields) < 2 {
			fmt.Println("usage: browser <url>")
			return true
		}
		tool, ok := registry.Get("browser")
		if !ok {
			return true
		}
		out, err := tool.Execute(ctx, map[string]string{"url": fields[1]})
		if err != nil {
			fmt.Println("Browser error:", err)
			return true
		}
		fmt.Println(truncate(out, 4000))
		return true

	case "scrape":
		if len(fields) < 2 {
			fmt.Println("usage: scrape <url> [output.md]")
			return true
		}
		args := map[string]string{"url": fields[1]}
		if len(fields) >= 3 {
			args["output"] = fields[2]
		}
		tool, ok := registry.Get("scrape")
		if !ok {
			fmt.Println("scrape tool not registered")
			return true
		}
		out, err := tool.Execute(ctx, args)
		if err != nil {
			fmt.Println("Scrape error:", err)
			if out != "" {
				fmt.Println(out)
			}
			return true
		}
		fmt.Println(truncate(out, 8000))
		return true

	case "python":
		if len(fields) < 2 {
			fmt.Println("usage: python <script.py> [args...]")
			return true
		}
		tool, ok := registry.Get("python")
		if !ok {
			fmt.Println("python tool not registered")
			return true
		}
		args := map[string]string{"path": fields[1]}
		if len(fields) > 2 {
			args["args"] = strings.Join(fields[2:], " ")
		}
		out, err := tool.Execute(ctx, args)
		if err != nil {
			fmt.Println("Python error:", err)
			if out != "" {
				fmt.Println(out)
			}
			return true
		}
		fmt.Println(truncate(out, 8000))
		return true
	}
	return false
}

func limitGitOutput(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + gitTruncationMarker
}

// runGitLimited executes git with bounded output and a per-command timeout.
func runGitLimited(ctx context.Context, dir string, maxBytes int, args ...string) (string, error) {
	if maxBytes <= 0 {
		return "", errors.New("invalid git output limit")
	}
	if len(args) == 0 {
		return "", errors.New("missing git command")
	}
	gitCtx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	gitArgs := []string{
		"-c", "color.ui=false",
		"-c", "core.pager=cat",
		"-c", "diff.external=",
		"-c", "core.fsmonitor=false",
		"--no-pager",
	}
	gitArgs = append(gitArgs, args...)
	cmd := exec.CommandContext(gitCtx, "git", gitArgs...)
	cmd.Dir = dir
	cancellation, err := newGitCommandCancellation(cmd)
	if err != nil {
		return "", fmt.Errorf("configure git cancellation: %w", err)
	}
	defer func() {
		_ = cancellation.close()
	}()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("prepare git command: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("prepare git command: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start git command: %w", err)
	}
	if err := cancellation.attach(cmd); err != nil {
		_ = cancellation.terminate()
		_ = cmd.Process.Kill()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = cmd.Wait()
		return "", fmt.Errorf("configure git cancellation: %w", err)
	}

	type gitReadResult struct {
		output []byte
		err    error
	}
	stdoutResult := make(chan gitReadResult, 1)
	go func() {
		output, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes+1)))
		stdoutResult <- gitReadResult{output: output, err: readErr}
	}()
	stderrResult := make(chan gitReadResult, 1)
	go func() {
		output, readErr := io.ReadAll(io.LimitReader(stderr, int64(gitErrorOutputLimit+1)))
		stderrResult <- gitReadResult{output: output, err: readErr}
	}()

	var stdoutRead, stderrRead gitReadResult
	var gotStdout, gotStderr, outputTruncated, stderrTruncated, stopped bool
	contextDone := gitCtx.Done()
	stopCommand := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cancellation.terminate()
		_ = stdout.Close()
		_ = stderr.Close()
		contextDone = nil
	}
	for !gotStdout || !gotStderr {
		select {
		case stdoutRead = <-stdoutResult:
			gotStdout = true
			outputTruncated = len(stdoutRead.output) > maxBytes
			if outputTruncated {
				stopCommand()
			}
		case stderrRead = <-stderrResult:
			gotStderr = true
			stderrTruncated = len(stderrRead.output) > gitErrorOutputLimit
			if stderrTruncated {
				stopCommand()
			}
		case <-contextDone:
			stopCommand()
		}
	}
	waitErr := cmd.Wait()
	if errors.Is(gitCtx.Err(), context.DeadlineExceeded) {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git %s timed out: %w", args[0], ctx.Err())
		}
		return "", fmt.Errorf("git %s timed out after %s", args[0], gitCommandTimeout)
	}
	if errors.Is(gitCtx.Err(), context.Canceled) {
		return "", fmt.Errorf("git %s canceled", args[0])
	}
	if outputTruncated {
		return string(stdoutRead.output[:maxBytes]) + gitTruncationMarker, nil
	}
	if stdoutRead.err != nil && !stderrTruncated {
		return "", fmt.Errorf("read git output: %w", stdoutRead.err)
	}
	if stderrRead.err != nil {
		return "", fmt.Errorf("read git output: %w", stderrRead.err)
	}
	if waitErr != nil {
		detail := strings.TrimSpace(string(stderrRead.output))
		if stderrTruncated {
			detail += gitTruncationMarker
		}
		if detail != "" {
			return "", fmt.Errorf("git %s failed: %w: %s", args[0], waitErr, detail)
		}
		return "", fmt.Errorf("git %s failed: %w", args[0], waitErr)
	}
	return string(stdoutRead.output), nil
}

func gitAvailableError(err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("git not available")
	}
	return err
}

func gitCommandInterrupted(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, " timed out") || strings.Contains(message, " canceled")
}

func gitStatusHasChanges(status string) bool {
	for _, line := range strings.Split(status, "\n") {
		if line != "" && !strings.HasPrefix(line, "## ") {
			return true
		}
	}
	return false
}

func appendGitSection(b *strings.Builder, title, content, empty string) {
	b.WriteString(title)
	b.WriteString(":\n")
	content = strings.TrimRight(content, "\r\n")
	if strings.TrimSpace(content) == "" {
		b.WriteString(empty)
		b.WriteString("\n")
		return
	}
	b.WriteString(content)
	b.WriteString("\n")
}

func gitRepositoryRoot(ctx context.Context, dir string) (string, error) {
	root, err := runGitLimited(ctx, dir, 4096, "rev-parse", "--show-toplevel")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git not available")
		}
		if gitCommandInterrupted(err) {
			return "", err
		}
		return "", errors.New("not inside a git repository")
	}
	return strings.TrimSpace(root), nil
}

func gitBranchSummary(ctx context.Context, dir string) (string, error) {
	branch, err := runGitLimited(ctx, dir, 4096, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil {
		return strings.TrimSpace(branch), nil
	}
	if gitCommandInterrupted(err) {
		return "", err
	}
	head, headErr := runGitLimited(ctx, dir, 4096, "rev-parse", "--short", "HEAD")
	if headErr != nil {
		return "", gitAvailableError(headErr)
	}
	return "HEAD detached at " + strings.TrimSpace(head), nil
}

func gitOverview(ctx context.Context, dir string) (string, error) {
	root, err := gitRepositoryRoot(ctx, dir)
	if err != nil {
		return "", err
	}
	branch, err := gitBranchSummary(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("git branch lookup failed: %w", err)
	}
	status, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"status", "--short", "--branch", "--untracked-files=all")
	if err != nil {
		return "", gitAvailableError(err)
	}
	staged, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"diff", "--cached", "--stat", "--patch", "--unified=3", "--no-ext-diff", "--no-color", "--no-textconv")
	if err != nil {
		return "", gitAvailableError(err)
	}
	unstaged, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"diff", "--stat", "--patch", "--unified=3", "--no-ext-diff", "--no-color", "--no-textconv")
	if err != nil {
		return "", gitAvailableError(err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Repository root: %s\nBranch: %s\n\n", root, branch)
	appendGitSection(&b, "Status", status, "(no status)")
	if !gitStatusHasChanges(status) {
		b.WriteString("(no changes)\n")
	}
	b.WriteString("\n")
	appendGitSection(&b, "Staged changes", staged, "(no staged changes)")
	b.WriteString("\n")
	appendGitSection(&b, "Unstaged changes", unstaged, "(no unstaged changes)")
	return limitGitOutput(b.String(), gitRenderedOutputLimit), nil
}

type gitVersion struct {
	major int
	minor int
}

func parseGitVersion(output string) (gitVersion, error) {
	matches := gitVersionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if matches == nil {
		return gitVersion{}, fmt.Errorf("unrecognized git version output: %q", strings.TrimSpace(output))
	}
	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return gitVersion{}, fmt.Errorf("parse git major version: %w", err)
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return gitVersion{}, fmt.Errorf("parse git minor version: %w", err)
	}
	return gitVersion{major: major, minor: minor}, nil
}

func (version gitVersion) atLeast(major, minor int) bool {
	return version.major > major || (version.major == major && version.minor >= minor)
}

func gitInstalledVersion(ctx context.Context, dir string) (gitVersion, error) {
	output, err := runGitLimited(ctx, dir, 4096, "version")
	if err != nil {
		return gitVersion{}, err
	}
	return parseGitVersion(output)
}

func gitRevision(ctx context.Context, dir, revision string) (string, error) {
	if _, err := gitRepositoryRoot(ctx, dir); err != nil {
		return "", err
	}
	version, err := gitInstalledVersion(ctx, dir)
	if err != nil {
		if gitCommandInterrupted(err) {
			return "", err
		}
		return "", errors.New(gitRevisionMinimumVersion)
	}
	if !version.atLeast(2, 30) {
		return "", errors.New(gitRevisionMinimumVersion)
	}
	resolved, err := runGitLimited(ctx, dir, 4096,
		"rev-parse", "--verify", "--quiet", "--end-of-options", revision+"^{commit}")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git not available")
		}
		if gitCommandInterrupted(err) {
			return "", err
		}
		return "", fmt.Errorf("invalid git revision: %s", revision)
	}
	shaFields := strings.Fields(resolved)
	if len(shaFields) != 1 {
		return "", fmt.Errorf("invalid git revision: %s", revision)
	}
	sha := shaFields[0]
	metadata, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"show", "--no-patch", "--format=fuller", "--decorate=short", "--no-ext-diff", "--no-color", sha)
	if err != nil {
		return "", gitAvailableError(err)
	}
	patch, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"show", "--stat", "--patch", "--unified=3", "--no-ext-diff", "--no-color", "--no-textconv", "--format=", sha)
	if err != nil {
		return "", gitAvailableError(err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Resolved revision: %s\n\n", sha)
	appendGitSection(&b, "Commit metadata", metadata, "(no commit metadata)")
	b.WriteString("\n")
	appendGitSection(&b, "Commit changes", patch, "(no changes in this commit)")
	return limitGitOutput(b.String(), gitRenderedOutputLimit), nil
}

func handleGitREPLCommand(ctx context.Context, revisions []string) (string, error) {
	if len(revisions) > 1 {
		return "", errors.New("usage: /git [revision]")
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine current directory: %w", err)
	}
	if len(revisions) == 1 {
		return gitRevision(ctx, dir, revisions[0])
	}
	return gitOverview(ctx, dir)
}

func runREPL(ctx context.Context, model ChatModel, registry agent.ToolRegistry, mem agent.Memory, rag agent.RAG) {
	fmt.Print(`toolloop REPL
Commands:
  /help              Show help
  /git [revision]    Inspect repository state or one commit (read-only)
  /tools             List tools
  /agent             Manage agents (separate conversations + system prompts)
  /mem               Show recent memory
  /reset             Clear session history (not DB)
  /index <path>      Index a directory into RAG
  /model <name>      Switch chat model
  /quit              Exit

Direct tools (no model):
  fs read README.md
  fs list .
  shell go version
  browser https://go.dev
  scrape https://example.com sports.md
  python python/wnba.py

Type a task/question and press Enter.
`)
	in := bufio.NewScanner(os.Stdin)
	buf := make([]byte, 0, 1024*64)
	in.Buffer(buf, 1024*1024)

	loadAgentsMarkdown()
	if agentsMDPath != "" {
		var loaded []string
		for _, n := range builtinRoleNames() {
			if agentsMDRoles[n] {
				loaded = append(loaded, n)
			}
		}
		fmt.Printf("Loaded agent roles from %s (%s)\n", agentsMDPath, strings.Join(loaded, ", "))
	}
	mgr := newAgentManager(model.SystemPromptValue())

	for {
		fmt.Printf("%s> ", mgr.active)
		if !in.Scan() {
			fmt.Println()
			break
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		if isWeakREPLInput(line) {
			fmt.Println("Please enter a clearer task (e.g. a path, URL, or question). Type /help for commands.")
			continue
		}
		if strings.HasPrefix(line, "/") {
			if strings.HasPrefix(strings.ToLower(line), "/agent") {
				handleAgentCommand(ctx, line, mgr, in, model, registry, mem, rag)
				continue
			}
			if quit := handleREPLCommand(ctx, line, model, mem, rag, &mgr.current().Notes); quit {
				break
			}
			continue
		}
		if tryDirectToolLine(ctx, line, registry) {
			continue
		}

		currentAgent := mgr.current()
		answer, err := runAgentTask(ctx, model, registry, mem, rag, currentAgent, line)
		if err != nil {
			fmt.Println("Task failed:", err)
			continue
		}
		fmt.Println()
		fmt.Println(answer)
		fmt.Println()
	}
	if err := in.Err(); err != nil {
		fmt.Println("input error:", err)
	}
}

func handleREPLCommand(
	ctx context.Context,
	line string,
	model ChatModel,
	mem agent.Memory,
	rag agent.RAG,
	sessionNotes *strings.Builder,
) (quit bool) {
	parts := strings.Fields(line)
	cmd := strings.ToLower(parts[0])
	switch cmd {
	case "/quit", "/exit", "/q":
		fmt.Println("bye")
		return true
	case "/help", "/?":
		fmt.Println(`Commands:
  /help              Show help
  /git [revision]    Inspect repository state or one commit (read-only)
  /tools             List tools
  /agent             Manage agents (see /agent help)
  /mem               Show recent memory
  /reset             Clear session history (not DB)
  /index <path>      Index a directory into RAG
  /model <name>      Switch chat model
  /quit              Exit
Direct: fs read <path> | shell <cmd> | browser <url> | scrape <url> [output.md] | python <script.py>`)
	case "/git":
		out, err := handleGitREPLCommand(ctx, parts[1:])
		if err != nil {
			fmt.Println("git error:", err)
			return false
		}
		fmt.Println(out)
	case "/tools":
		fmt.Println(`Tools:
  fs list | tree | read | read_many | info | write | edit
  fs edit args: path, old_snippet, new_snippet, optional start_line/end_line/checksum
  shell cmd=<command>
  browser url=<https://...>
  web_search query=<text>
  scrape url=<https://...> output=<file.md> mode=fetch|stealthy-fetch|get
  python path=<script.py>|code=<inline source> [args=... cwd=... timeout=...]`)
	case "/mem":
		if mem == nil {
			fmt.Println("memory not available")
			return false
		}
		items, err := mem.Recent(ctx, 10)
		if err != nil {
			fmt.Println("memory error:", err)
			return false
		}
		if len(items) == 0 {
			fmt.Println("(empty)")
			return false
		}
		for _, it := range items {
			fmt.Printf("[%s] %s\n%s\n\n", it.Kind, it.CreatedAt.Format(time.RFC3339), truncate(it.Content, 500))
		}
	case "/reset":
		sessionNotes.Reset()
		fmt.Println("session history cleared (DB unchanged)")
	case "/index":
		if len(parts) < 2 {
			fmt.Println("usage: /index <path>")
			return false
		}
		if rag == nil {
			fmt.Println("rag not available")
			return false
		}
		path := parts[1]
		fmt.Println("Indexing", path, "...")
		n, err := rag.IndexPath(ctx, path)
		if err != nil {
			fmt.Println("index error:", err)
			return false
		}
		fmt.Printf("Indexed %d chunks\n", n)
	case "/model":
		if len(parts) < 2 {
			fmt.Println("current model:", model.ModelName())
			fmt.Println("usage: /model <name>")
			return false
		}
		model.SetModel(parts[1])
		fmt.Println("model set to", model.ModelName())
	default:
		fmt.Println("unknown command:", cmd, "(try /help)")
	}
	return false
}

// ─── MULTI-TASK ─────────────────────────────────────────────────────

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ", ") }
func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func runTasks(
	ctx context.Context,
	model ChatModel,
	registry agent.ToolRegistry,
	tasks []string,
	output *strings.Builder,
	initialContext string,
	mem agent.Memory,
	rag agent.RAG,
) {
	var previousResults strings.Builder
	if initialContext != "" {
		previousResults.WriteString(initialContext)
	}
	for i, taskDesc := range tasks {
		header := fmt.Sprintf("\n========== Task %d/%d ==========\nRunning task: %s\n", i+1, len(tasks), taskDesc)
		fmt.Print(header)
		if output != nil {
			output.WriteString(header)
		}
		fullDescription := taskDesc
		if previousResults.Len() > 0 {
			fullDescription = fmt.Sprintf("%s\n\nContext from previous tasks:\n%s", taskDesc, previousResults.String())
		}
		task := &agent.Task{
			ID:          fmt.Sprintf("task-%d-%d", time.Now().Unix(), i),
			Description: fullDescription,
			CreatedAt:   time.Now(),
			Status:      agent.TaskPending,
			Steps:       []*agent.Step{},
			Memory:      mem,
			RAG:         rag,
			Tools:       registry,
		}
		engine := &agent.Engine{Model: model}
		if err := engine.RunTask(ctx, task); err != nil {
			msg := fmt.Sprintf("Task failed: %v\n", err)
			fmt.Print(msg)
			if output != nil {
				output.WriteString(msg)
			}
			continue
		}
		finalAnswer, err := model.GenerateFinalAnswer(ctx, task)
		if err != nil {
			finalAnswer = "Failed to generate final answer: " + err.Error()
		}
		// Debug output for final answer parsing
		debugf("DEBUG: Final answer (raw):\n")
		debugf("%s\n", finalAnswer)
		if tn, args, perr := parseToolJSON(finalAnswer); perr == nil {
			debugf("DEBUG: parseToolJSON -> tool=%s args=%v\n", tn, args)
		} else {
			debugf("DEBUG: parseToolJSON failed: %v\n", perr)
		}
		if fa, ok := forceFSArgsFromContent(finalAnswer); ok {
			debugf("DEBUG: forceFSArgsFromContent -> %v\n", fa)
		} else {
			debugf("DEBUG: forceFSArgsFromContent -> no\n")
		}
		if ex, ok := extractFSCommandFromText(finalAnswer); ok {
			debugf("DEBUG: extractFSCommandFromText -> %v\n", ex)
		} else {
			debugf("DEBUG: extractFSCommandFromText -> no\n")
		}

		// If the model returned a structured tool response only in the final answer (common with some models),
		// try to parse it and execute the fs write if needed and not already performed during steps.
		// This handles cases where the model emits: {"op":"write","path":"sample.go","content":"..."}
		didExecute := false
		// helper: check if task already ran an fs write for given path
		alreadyWrote := func(path string) bool {
			for _, s := range task.Steps {
				if s.ToolCall == nil {
					continue
				}
				if s.ToolCall.Name != "fs" {
					continue
				}
				if op, ok := s.ToolCall.Args["op"]; ok && op == "write" {
					if p, ok := s.ToolCall.Args["path"]; ok && p == path {
						return true
					}
				}
			}
			return false
		}

		// Try structured JSON parse first
		if tn, args, perr := parseToolJSON(finalAnswer); perr == nil && tn == "fs" {
			args = normalizeFSArgs(args)
			if args["op"] == "write" && args["path"] != "" && !alreadyWrote(args["path"]) {
				if tool, ok := registry.Get("fs"); ok {
					if res, err := tool.Execute(ctx, args); err != nil {
						finalAnswer = finalAnswer + "\n\n[fs execution failed: " + err.Error() + "]"
					} else {
						finalAnswer = finalAnswer + "\n\n[fs executed: " + res + "]"
					}
					didExecute = true
				}
			}
		}

		// If not executed yet, try the forced bare-JSON/content parser
		if !didExecute {
			if fa, ok := forceFSArgsFromContent(finalAnswer); ok {
				if fa["op"] == "write" && fa["path"] != "" && !alreadyWrote(fa["path"]) {
					if tool, ok := registry.Get("fs"); ok {
						if res, err := tool.Execute(ctx, fa); err != nil {
							finalAnswer = finalAnswer + "\n\n[fs execution failed: " + err.Error() + "]"
						} else {
							finalAnswer = finalAnswer + "\n\n[fs executed: " + res + "]"
						}
						didExecute = true
					}
				}
			}
		}

		if mem != nil {
			_ = mem.Save(ctx, task.ID, "task_result", finalAnswer)
		}
		fmt.Println("Task completed.")
		fmt.Println("Final Answer:")
		fmt.Println(finalAnswer)
		if output != nil {
			output.WriteString("Task completed.\nFinal Answer:\n" + finalAnswer + "\n")
		}
		previousResults.WriteString(fmt.Sprintf("From task '%s':\n%s\n\n", taskDesc, finalAnswer))
	}
}

// ─── MAIN ───────────────────────────────────────────────────────────

func main() {
	var tasks stringSlice
	flag.Var(&tasks, "task", "Task description (can be used multiple times)")
	backend := flag.String("backend", "llama.cpp", "Model backend: llama.cpp or ollama")
	serverURL := flag.String("server", defaultLlamaURL, "llama.cpp server base URL (uses POST /completion)")
	webQuery := flag.String("web", "", "Web search query")
	browserAction := flag.String("browser", "", "Browser URL")
	scrapeURL := flag.String("scrape", "", "Scrape URL with Scrapling into markdown/text")
	scrapeOutput := flag.String("scrape-output", "sports.md", "Scrape output file path")
	fsOp := flag.String("fs", "", "File system operation (e.g. 'list ./')")
	shellCmd := flag.String("shell", "", "Shell command")
	outputFile := flag.String("output", "", "Save full output to a file")
	indexPath := flag.String("index", "", "Index a directory into RAG")
	skipIndex := flag.Bool("skip-index", false, "Skip automatic indexing of ./documents when it exists")
	modelName := flag.String("model", defaultModel, "Model name/label; llama.cpp serves one loaded model")
	replMode := flag.Bool("repl", false, "Interactive REPL mode")
	// prompt file option to override default system prompt
	promptFile := flag.String("prompt", "", "Prompt file to use as system prompt")
	debugFlag := flag.Bool("debug", false, "Enable debug logging")
	flag.Parse()
	debugMode = *debugFlag

	hasDirectAction := *webQuery != "" || *browserAction != "" || *scrapeURL != "" || *fsOp != "" || *shellCmd != ""
	if len(tasks) == 0 && *indexPath == "" && !*replMode && !hasDirectAction {
		log.Fatal(`Usage:
  go run ./cmd/toolloop -repl
  go run ./cmd/toolloop -task "..."
  go run ./cmd/toolloop -index documents
  go run ./cmd/toolloop -backend ollama -model qwen2.5:14b -task "..."
  go run ./cmd/toolloop -scrape "https://example.com" -scrape-output sports.md

Notes:
  If ./documents exists, it is indexed automatically on startup.
  Use -skip-index to disable that optional pass.

Options:
  -backend llama.cpp|ollama
  -server <url>    llama.cpp server URL (default http://localhost:8080)
  -prompt <file>   Prompt file to use as system prompt`)
	}

	ctx := context.Background()

	var model ChatModel
	var embedder agent.Embedder
	switch strings.ToLower(strings.TrimSpace(*backend)) {
	case "", "llama", "llamacpp", "llama.cpp", "llama-go":
		client := NewLlamaClient(*serverURL)
		model = &LlamaCppModel{Client: client, Model: *modelName}
		embedder = &LlamaEmbedder{Client: client}
	case "ollama":
		client, err := api.ClientFromEnvironment()
		if err != nil {
			log.Fatal(err)
		}
		name := *modelName
		if name == "" || name == defaultModel {
			name = "qwen2.5:14b"
		}
		model = &OllamaModel{Client: client, Model: name}
		embedder = &agent.OllamaEmbedder{Client: client, Model: embedModel}
	default:
		log.Fatalf("unknown backend %q (use llama.cpp or ollama)", *backend)
	}

	// Read prompt file if specified, otherwise use default system prompt
	if *promptFile != "" {
		data, err := os.ReadFile(*promptFile)
		if err == nil {
			customPrompt := strings.TrimSpace(string(data))
			if customPrompt != "" && customPrompt != " " {
				model.SetSystemPrompt(customPrompt)
			}
		} else {
			log.Printf("Warning: could not read prompt file %s: %v", *promptFile, err)
		}
	}

	// Ensure default system prompt is set if no custom prompt was provided
	if strings.TrimSpace(model.SystemPromptValue()) == "" {
		model.SetSystemPrompt(defaultSystemPrompt)
	}

	mem, err := agent.NewSQLiteMemory(memoryDBPath)
	if err != nil {
		log.Fatalf("memory: %v", err)
	}
	defer mem.Close()

	rag, err := agent.NewSQLiteRAG(ragDBPath, embedder)
	if err != nil {
		log.Fatalf("rag: %v", err)
	}
	defer rag.Close()

	if *indexPath != "" {
		fmt.Printf("Indexing %s ...\n", *indexPath)
		n, err := rag.IndexPath(ctx, *indexPath)
		if err != nil {
			log.Fatalf("index failed: %v", err)
		}
		fmt.Printf("Indexed %d chunks into %s\n", n, ragDBPath)
		if len(tasks) == 0 {
			return
		}
	}

	if !*skipIndex && *indexPath == "" {
		if st, err := os.Stat(defaultIndexRoot); err == nil && st.IsDir() {
			fmt.Printf("Indexing %s (use -skip-index to disable)...\n", defaultIndexRoot)
			n, err := rag.IndexPath(ctx, defaultIndexRoot)
			if err != nil {
				fmt.Println("Index warning:", err)
			} else {
				fmt.Printf("Indexed %d chunks\n", n)
			}
		}
	}

	registry := agent.NewToolRegistry()
	registry.Register("web_search", agent.WebSearchTool{})
	registry.Register("browser", agent.BrowserTool{})
	registry.Register("scrape", agent.ScrapeTool{})
	registry.Register("fs", agent.FileSystemTool{})
	registry.Register("shell", agent.ShellTool{})
	registry.Register("python", agent.PythonTool{})

	if *replMode {
		runREPL(ctx, model, registry, mem, rag)
		return
	}

	var initialContext strings.Builder
	if *webQuery != "" {
		tool, _ := registry.Get("web_search")
		res, err := tool.Execute(ctx, map[string]string{"query": *webQuery})
		if err != nil {
			fmt.Println("WebSearch error:", err)
		} else {
			fmt.Println("WebSearch:", truncate(res, 400))
			initialContext.WriteString("WebSearch result:\n" + res + "\n\n")
		}
	}
	if *browserAction != "" {
		tool, _ := registry.Get("browser")
		res, err := tool.Execute(ctx, map[string]string{"url": *browserAction})
		if err != nil {
			fmt.Println("Browser error:", err)
		} else {
			fmt.Println("Browser:", truncate(res, 400))
			initialContext.WriteString("Browser result:\n" + res + "\n\n")
		}
		if *scrapeURL != "" {
			tool, _ := registry.Get("scrape")
			res, err := tool.Execute(ctx, map[string]string{"url": *scrapeURL, "output": *scrapeOutput})
			if err != nil {
				fmt.Println("Scrape error:", err)
				if res != "" {
					fmt.Println(truncate(res, 4000))
				}
			} else {
				fmt.Println("Scrape:", truncate(res, 4000))
				initialContext.WriteString("Scrape result:\n" + res + "\n\n")
			}
		}
	}
	if *fsOp != "" {
		tool, _ := registry.Get("fs")
		parts := strings.Fields(*fsOp)
		if len(parts) >= 2 {
			args := normalizeFSArgs(map[string]string{"op": parts[0], "path": parts[1]})
			res, err := tool.Execute(ctx, args)
			if err != nil {
				fmt.Println("FS error:", err)
			} else {
				fmt.Println("FS:\n" + res)
				initialContext.WriteString("FS result:\n" + res + "\n\n")
			}
		}
	}
	if *shellCmd != "" {
		tool, _ := registry.Get("shell")
		res, err := tool.Execute(ctx, map[string]string{"cmd": *shellCmd})
		if err != nil {
			fmt.Println("Shell error:", err)
		} else {
			fmt.Println("Shell:", res)
			initialContext.WriteString("Shell result:\n" + res + "\n\n")
		}
	}

	if len(tasks) == 0 {
		return
	}
	var outputBuilder strings.Builder
	runTasks(ctx, model, registry, tasks, &outputBuilder, initialContext.String(), mem, rag)
	if *outputFile != "" {
		if err := os.WriteFile(*outputFile, []byte(outputBuilder.String()), 0644); err != nil {
			log.Fatalf("Failed to write output file: %v", err)
		}
		fmt.Printf("\nOutput saved to %s\n", *outputFile)
	}
}
