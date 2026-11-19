package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/ollama/ollama/api"
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

	return resolveLlamaToolFromResponse(LlamaMessage{ToolCalls: extractLlamaToolCalls(msg.Content)})
}

// ─── RAG / MEMORY HELPERS ───────────────────────────────────────────

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
		Description: shellToolDescription(runtime.GOOS),
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

func (m *OllamaModel) PlanNextStep(ctx context.Context, task *engine.Task) (*engine.Step, error) {
	history := ""
	for _, s := range task.Steps {
		toolInfo := "none"
		if s.ToolCall != nil {
			toolInfo = fmt.Sprintf("%s %v", s.ToolCall.Name, s.ToolCall.Args)
		}
		history += fmt.Sprintf("- Step %d: %s | Tool: %s | Result: %s\n",
			s.Index, stepHistoryPlan(s), toolInfo, truncate(s.Result, 8000))
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
		if items, err := task.Memory.Search(ctx, task.Description, 5); err == nil {
			items = filterMemoryItems(items, 0.35)
			memBlock = formatMemoryContext(items)
		}
	}

	system := m.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = defaultSystemPrompt
	}
	system = system + "\n\n" + toolInstructionsFor(task.Tools) + "\n\n" + hostPlatformInstructions(runtime.GOOS) + "\n\n" + currentTimeContext(time.Now())

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
		} else if strings.HasPrefix(s.Plan, "Blocked:") {
			messages = append(messages, api.Message{Role: "assistant", Content: s.Plan})
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

	step := &engine.Step{
		Index: len(task.Steps),
		Plan:  stripReasoning(last.Message.Content),
	}

	name, args, ok := resolveToolFromResponse(last.Message)
	if !ok {
		if problem := responseProblem(step.Plan); problem != "" || len(last.Message.ToolCalls) > 0 {
			step.Plan = "Blocked: invalid model response; return a valid tool call or a plain-text completion note"
		}
		return step, nil
	}

	if name == "fs" {
		args = normalizeFSArgs(args)
		op := args["op"]
		path := sanitizePath(args["path"])
		args["path"] = path

		if !isWorkspaceFSRead(op, path) {
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
	}

	step.ToolCall = &engine.ToolCall{Name: name, Args: args}
	step.Plan = fmt.Sprintf("call %s %v", name, args)
	return step, nil
}

// ─── FINAL ANSWER ───────────────────────────────────────────────────

func (m *OllamaModel) GenerateFinalAnswer(ctx context.Context, task *engine.Task) (string, error) {
	history := ""
	usedTools := false
	for _, s := range task.Steps {
		if s.ToolCall != nil && s.ToolCall.Name != "" {
			usedTools = true
		}
		history += fmt.Sprintf("Step %d:\nPlan: %s\nResult:\n%s\n\n",
			s.Index, stepHistoryPlan(s), truncate(s.Result, 1200000))
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
	system := m.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = defaultSystemPrompt
	}
	system = system + "\n\n" + hostPlatformInstructions(runtime.GOOS) + "\n\n" + currentTimeContext(time.Now())
	req := &api.ChatRequest{
		Model: modelName,
		Messages: []api.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: prompt},
		},
		Stream: &stream,
		Options: map[string]interface{}{
			"temperature": 0.0,
		},
	}

	var fullResponse string
	var lastResp api.ChatResponse
	hasToolCalls := false
	debugf("DEBUG: starting GenerateFinalAnswer Chat to model %s\n", m.Model)
	start := time.Now()
	ctx2, cancel := context.WithTimeout(ctx, Timeout*time.Second)
	defer cancel()
	err := m.Client.Chat(ctx2, req, func(resp api.ChatResponse) error {
		debugf("DEBUG: finalAnswer chunk content_len=%d toolcalls=%d\n", len(resp.Message.Content), len(resp.Message.ToolCalls))
		fullResponse += resp.Message.Content
		hasToolCalls = hasToolCalls || len(resp.Message.ToolCalls) > 0
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
	return finalAnswer(fullResponse, hasToolCalls)
}

var _ engine.ChatModel = (*OllamaModel)(nil)
