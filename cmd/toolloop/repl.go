package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/memory"
	"github.com/396xwz/toolloop/internal/tools"
)

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

// ─── REPL ───────────────────────────────────────────────────────────

func tryDirectToolLine(ctx context.Context, line string, registry tools.ToolRegistry) bool {
	fields := strings.Fields(line)
	for i := range fields {
		fields[i] = stripQuotes(fields[i])
	}
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

// stripQuotes removes one pair of matching surrounding quotes from a token so
// shell-style quoting (scrape "https://..." out.md) works in the REPL.
func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func runREPL(ctx context.Context, model engine.ChatModel, registry tools.ToolRegistry, mem memory.Memory, rag memory.RAG) {
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
	model engine.ChatModel,
	mem memory.Memory,
	rag memory.RAG,
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
