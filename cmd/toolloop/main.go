package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/memory"
	"github.com/396xwz/toolloop/internal/tools"
	"github.com/396xwz/toolloop/internal/topology"
	"github.com/ollama/ollama/api"
)

const (
	defaultModel     = "llama.cpp"
	embedModel       = "nomic-embed-text"
	memoryDBPath     = "agent_memory.db"
	ragDBPath        = "agent_rag.db"
	Timeout          = 480
	defaultIndexRoot = "documents"

	maxStepResultChars         = 4000
	maxFinalAnswerHistoryChars = 24000

	// Default system prompt when no custom prompt is provided
	defaultSystemPrompt = `You are a local tool-using agent.
Use tools when needed to complete the task.
Prefer the exact path and operation mentioned in the task.
For directories use fs op=list or op=tree.
For files use fs op=read or fs op=read_many.
For Python scripts or Python code, use the python tool. Example: python path=python/wnba2.py.
The python tool can run scripts under the repo's python/ directory using the project's .venv interpreter.
When calling fs, always set op and path as plain strings (e.g. op=read, path=README.md).
If you already have enough information from previous steps or retrieved context, do not call any tool.
If the agent tool is listed under Available tools, use it as described there instead of doing that role's work yourself.`
)

var debugMode bool

func debugf(format string, a ...interface{}) {
	if !debugMode {
		return
	}
	fmt.Printf(format, a...)
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
	model engine.ChatModel,
	registry tools.ToolRegistry,
	tasks []string,
	output *strings.Builder,
	initialContext string,
	mem memory.Memory,
	rag memory.RAG,
	agentMgr *agentManager,
	agentTarget *replAgent,
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
		var finalAnswer string
		var err error
		if agentTarget != nil {
			finalAnswer, err = runAgentTask(ctx, agentMgr, model, registry, mem, rag, agentTarget, fullDescription)
			if err != nil {
				msg := fmt.Sprintf("Task failed: %v\n", err)
				fmt.Print(msg)
				if output != nil {
					output.WriteString(msg)
				}
				continue
			}
		} else {
			task := &engine.Task{
				ID:          fmt.Sprintf("task-%d-%d", time.Now().Unix(), i),
				Description: fullDescription,
				CreatedAt:   time.Now(),
				Status:      engine.TaskPending,
				Steps:       []*engine.Step{},
				Memory:      mem,
				RAG:         rag,
				Tools:       registry,
				Label:       "main",
			}
			taskEngine := &engine.Engine{Model: model}
			if err := taskEngine.RunTask(ctx, task); err != nil {
				msg := fmt.Sprintf("Task failed: %v\n", err)
				fmt.Print(msg)
				if output != nil {
					output.WriteString(msg)
				}
				continue
			}
			fmt.Printf("  → main thinking… (final answer)\n")
			finalAnswer, err = model.GenerateFinalAnswer(ctx, task)
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

// printTopologyReport renders the maf-pipeline style printout: one line per
// executed node visit, in execution order (cycle node ids repeat).
func printTopologyReport(report *topology.Report) {
	for _, n := range report.Nodes {
		dur := 0.0
		if !n.StartedAt.IsZero() {
			dur = n.FinishedAt.Sub(n.StartedAt).Seconds()
		}
		fmt.Printf("[%s] verdict=%s steps=%d %.1fs\n", n.ID, n.Verdict, n.Steps, dur)
	}
}

// topologyExport emits the executed-graph export: to the -topology-report
// file when set, and to the debug log when -debug is on.
func topologyExport(file string, g *topology.Graph, report *topology.Report) {
	if file == "" && !debugMode {
		return
	}
	dot := topology.DOT(g, report)
	mer := topology.Mermaid(g, report)
	if file != "" {
		content := dot
		switch strings.ToLower(filepath.Ext(file)) {
		case ".mmd", ".mermaid", ".md":
			content = mer
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			log.Printf("writing topology report: %v", err)
			return
		}
		fmt.Printf("topology report written to %s\n", file)
	}
	if debugMode {
		debugf("[topology] exported graph (DOT):\n%s", dot)
		debugf("[topology] exported graph (mermaid):\n%s", mer)
	}
}

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
	agentName := flag.String("agent", "", "Run -task as the named agent (role from AGENTS.md or built-in roles)")
	// prompt file option to override default system prompt
	promptFile := flag.String("prompt", "", "Prompt file to use as system prompt")
	topologyPath := flag.String("topology", "", "Path to a topology graph (YAML); with -task, run the graph walk instead of the task loop")
	topologyReportFile := flag.String("topology-report", "", "Write a DOT/mermaid export of the executed graph to this file (.mmd/.mermaid/.md selects mermaid; default DOT)")
	debugFlag := flag.Bool("debug", false, "Enable debug logging")
	flag.Parse()
	debugMode = *debugFlag

	hasDirectAction := *webQuery != "" || *browserAction != "" || *scrapeURL != "" || *fsOp != "" || *shellCmd != ""
	if len(tasks) == 0 && *indexPath == "" && !*replMode && !hasDirectAction && *topologyPath == "" {
		log.Fatal(`Usage:
  go run ./cmd/toolloop -repl
  go run ./cmd/toolloop -task "..."
  go run ./cmd/toolloop -agent orchestrator -task "..."
  go run ./cmd/toolloop -index documents
  go run ./cmd/toolloop -backend ollama -model qwen2.5:14b -task "..."
go run ./cmd/toolloop -topology pipeline.yaml -task "Build the report"
  go run ./cmd/toolloop -scrape "https://example.com" -scrape-output sports.md

Notes:
  If ./documents exists, it is indexed automatically on startup.
  Use -skip-index to disable that optional pass.

Options:
  -backend llama.cpp|ollama
  -server <url>    llama.cpp server URL (default http://localhost:8080)
  -prompt <file>   Prompt file to use as system prompt
  -topology <file> YAML graph; with -task, run the graph walk
  -topology-report <file> Write a DOT/mermaid export of the executed graph (.mmd/.mermaid/.md selects mermaid)
  -agent <name>   Run -task as the named agent (roles from AGENTS.md)`)
	}

	ctx := context.Background()

	var model engine.ChatModel
	var embedder memory.Embedder
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
		embedder = &memory.OllamaEmbedder{Client: client, Model: embedModel}
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

	mem, err := memory.SQLiteMemoryV2(memoryDBPath, embedder)
	if err != nil {
		log.Fatalf("memory: %v", err)
	}
	defer mem.Close()

	rag, err := memory.NewSQLiteRAG(ragDBPath, embedder)
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

	registry := tools.NewToolRegistry()
	registry.Register("web_search", tools.WebSearchTool{})
	registry.Register("browser", tools.BrowserTool{})
	registry.Register("scrape", tools.ScrapeTool{})
	registry.Register("fs", tools.FileSystemTool{})
	registry.Register("shell", tools.ShellTool{})
	registry.Register("python", tools.PythonTool{})
	registry.Register("verdict", tools.VerdictTool{})

	if *replMode {
		runREPL(ctx, model, registry, mem, rag)
		return
	}

	// -topology: run the entry task as a graph walk instead of the task loop.
	if *topologyPath != "" {
		if len(tasks) != 1 {
			log.Fatalf("-topology requires exactly one -task (got %d)", len(tasks))
		}
		loadAgentsMarkdown()
		graph, err := topology.Load(*topologyPath)
		if err != nil {
			log.Fatalf("loading topology: %v", err)
		}
		if err := graph.ValidateRoles(builtinRoleNames()); err != nil {
			log.Fatalf("validating topology: %v", err)
		}
		if err := graph.ValidateTools(registry.Names()); err != nil {
			log.Fatalf("validating topology: %v", err)
		}
		stepHook := func(nodeID string, steps []*engine.Step) {
			if !debugMode {
				return
			}
			for _, st := range steps {
				dur := 0.0
				if !st.StartedAt.IsZero() {
					dur = st.FinishedAt.Sub(st.StartedAt).Seconds()
				}
				debugf("[topology] node=%s step=%d", nodeID, st.Index)
				if st.Plan != "" {
					debugf("[topology]   plan: %s", truncate(st.Plan, 200))
				}
				if st.ToolCall != nil {
					debugf("[topology]   tool: %s args=%v output=%s", st.ToolCall.Name, st.ToolCall.Args, truncate(st.ToolCall.Output, 300))
				}
				debugf("[topology]   result: %s err=%v (%.2fs)", truncate(st.Result, 300), st.Err, dur)
			}
		}
		report, runErr := topology.Run(ctx, graph, tasks[0], model, registry, mem, rag, builtinAgentPrompts, nil, stepHook)
		if runErr != nil {
			if report != nil {
				printTopologyReport(report)
				topologyExport(*topologyReportFile, graph, report)
			}
			fmt.Printf("PIPELINE FAILED: %v\n", runErr)
			os.Exit(1)
		}
		printTopologyReport(report)
		topologyExport(*topologyReportFile, graph, report)
		if report.Status == "ok" {
			fmt.Println("PIPELINE SUCCESS")
			os.Exit(0)
		}
		if len(report.Nodes) > 0 {
			last := report.Nodes[len(report.Nodes)-1]
			fmt.Printf("PIPELINE FAILED: %s: %s\n", last.ID, report.Reason)
		} else {
			fmt.Printf("PIPELINE FAILED: %s\n", report.Reason)
		}
		os.Exit(1)
	}

	// -agent: run -task under the named agent's system prompt, the same
	// path the REPL's /agent run uses. The agent tool is registered so
	// roles like orchestrator can delegate to subagents.
	var agentMgr *agentManager
	var agentTarget *replAgent
	if *agentName != "" {
		if len(tasks) == 0 {
			log.Fatal("-agent requires -task")
		}
		loadAgentsMarkdown()
		agentMgr = newAgentManager(model.SystemPromptValue())
		registry.Register("agent", agentTool{mgr: agentMgr, model: model, registry: registry, mem: mem, rag: rag})
		target, ok := agentMgr.get(*agentName)
		if !ok {
			log.Fatalf("unknown agent: %s (available: %s)", *agentName, agentMgr.roleNames())
		}
		agentTarget = target
		fmt.Printf("Running task(s) as agent %q\n", target.Name)
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
	runTasks(ctx, model, registry, tasks, &outputBuilder, initialContext.String(), mem, rag, agentMgr, agentTarget)
	if *outputFile != "" {
		if err := os.WriteFile(*outputFile, []byte(outputBuilder.String()), 0644); err != nil {
			log.Fatalf("Failed to write output file: %v", err)
		}
		fmt.Printf("\nOutput saved to %s\n", *outputFile)
	}
}
