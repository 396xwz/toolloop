package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/memory"
)

const defaultLlamaURL = "http://localhost:8080"

type LlamaMessage struct {
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	ToolCalls []LlamaToolCall `json:"-"`
}

type LlamaToolCallFunction struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type LlamaToolCall struct {
	Function LlamaToolCallFunction `json:"function"`
}

type LlamaChatResponse struct {
	Message LlamaMessage
	Done    bool
	Raw     string
	Stopped bool // true when the server stopped because of the token budget (no stop token)
}

type LlamaClient struct {
	BaseURL     string
	HTTP        *http.Client
	NPredict    int
	tmplChecked bool
	tmplOK      bool
}

func NewLlamaClient(baseURL string) *LlamaClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultLlamaURL
	}
	return &LlamaClient{
		BaseURL:  baseURL,
		HTTP:     &http.Client{Timeout: (Timeout + 30) * time.Second},
		NPredict: 2048,
	}
}

func (c *LlamaClient) url(path string) string { return c.BaseURL + path }

func (c *LlamaClient) postJSON(ctx context.Context, path string, body interface{}, out interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", http.MethodPost, c.url(path), resp.Status, truncate(string(data), 500))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *LlamaClient) applyTemplate(ctx context.Context, messages []LlamaMessage) (string, bool) {
	if c.tmplChecked && !c.tmplOK {
		return "", false
	}
	var out struct {
		Prompt string `json:"prompt"`
	}
	body := map[string]interface{}{"messages": messages}
	if err := c.postJSON(ctx, "/apply-template", body, &out); err != nil {
		debugf("DEBUG: /apply-template unavailable: %v\n", err)
		c.tmplChecked = true
		c.tmplOK = false
		return "", false
	}
	c.tmplChecked = true
	c.tmplOK = out.Prompt != ""
	if !c.tmplOK {
		return "", false
	}
	return out.Prompt, true
}

func fallbackPrompt(messages []LlamaMessage) string {
	var b strings.Builder
	for _, m := range messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		b.WriteString("<|im_start|>" + role + "\n")
		b.WriteString(m.Content)
		b.WriteString("<|im_end|>\n")
	}
	b.WriteString("<|im_start|>assistant\n")
	return b.String()
}

type completionRequest struct {
	Prompt      string   `json:"prompt"`
	NPredict    int      `json:"n_predict"`
	Temperature float64  `json:"temperature"`
	Stream      bool     `json:"stream"`
	CachePrompt bool     `json:"cache_prompt"`
	Stop        []string `json:"stop,omitempty"`
}

type completionResponse struct {
	Content string `json:"content"`
	Stop    bool   `json:"stop"`
	Model   string `json:"model"`
}

func (c *LlamaClient) Complete(ctx context.Context, prompt string, temperature float64) (string, error) {
	content, _, err := c.CompleteN(ctx, prompt, temperature, c.NPredict)
	return content, err
}

// CompleteN runs a completion with an explicit token budget. It returns the
// content and whether the server stopped because of the budget: llama.cpp
// reports stop=false when n_predict is hit (no stop token was produced),
// which means the output was truncated mid-thought.
func (c *LlamaClient) CompleteN(ctx context.Context, prompt string, temperature float64, nPredict int) (string, bool, error) {
	if nPredict <= 0 {
		nPredict = c.NPredict
	}
	req := completionRequest{
		Prompt:      prompt,
		NPredict:    nPredict,
		Temperature: temperature,
		Stream:      false,
		CachePrompt: true,
		Stop:        []string{"<|im_end|>", "<|endoftext|>", "</s>", "<|eot_id|>"},
	}
	var out completionResponse
	debugf("DEBUG: POST %s prompt_len=%d n_predict=%d\n", c.url("/completion"), len(prompt), req.NPredict)
	if err := c.postJSON(ctx, "/completion", req, &out); err != nil {
		return "", false, err
	}
	if strings.TrimSpace(out.Content) == "" {
		return "", false, fmt.Errorf("llama.cpp returned an empty completion")
	}
	debugf("DEBUG: completion content_len=%d stop=%v\n", len(out.Content), out.Stop)
	return out.Content, out.Stop, nil
}

func (c *LlamaClient) Chat(ctx context.Context, messages []LlamaMessage, temperature float64, fn func(LlamaChatResponse) error) error {
	return c.ChatBudget(ctx, messages, temperature, c.NPredict, fn)
}

// ChatBudget is Chat with an explicit token budget. Chat uses the client's
// NPredict; callers may provide a different budget for this request.
func (c *LlamaClient) ChatBudget(ctx context.Context, messages []LlamaMessage, temperature float64, nPredict int, fn func(LlamaChatResponse) error) error {
	prompt, ok := c.applyTemplate(ctx, messages)
	if !ok {
		prompt = fallbackPrompt(messages)
	}
	content, stopped, err := c.CompleteN(ctx, prompt, temperature, nPredict)
	if err != nil {
		return err
	}
	msg := LlamaMessage{Role: "assistant", Content: stripReasoning(content)}
	msg.ToolCalls = extractLlamaToolCalls(msg.Content)
	if fn == nil {
		return nil
	}
	return fn(LlamaChatResponse{Message: msg, Done: true, Stopped: stopped, Raw: content})
}

type LlamaEmbedder struct {
	Client *LlamaClient
}

func (e *LlamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e == nil || e.Client == nil {
		return nil, fmt.Errorf("no llama client")
	}
	var raw json.RawMessage
	if err := e.Client.postJSON(ctx, "/embedding", map[string]interface{}{"content": text}, &raw); err != nil {
		return nil, err
	}
	vec, err := parseEmbedding(raw)
	if err != nil {
		return nil, err
	}
	if len(vec) == 0 {
		return nil, fmt.Errorf("empty embedding")
	}
	return vec, nil
}

func parseEmbedding(raw json.RawMessage) ([]float32, error) {
	var single struct {
		Embedding json.RawMessage `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && len(single.Embedding) > 0 {
		return flattenFloats(single.Embedding)
	}
	var arr []struct {
		Embedding json.RawMessage `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 && len(arr[0].Embedding) > 0 {
		return flattenFloats(arr[0].Embedding)
	}
	return nil, fmt.Errorf("unrecognized embedding response")
}

func flattenFloats(raw json.RawMessage) ([]float32, error) {
	var flat []float32
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat, nil
	}
	var nested [][]float32
	if err := json.Unmarshal(raw, &nested); err == nil && len(nested) > 0 {
		return nested[0], nil
	}
	return nil, fmt.Errorf("unrecognized embedding vector")
}

func extractLlamaToolCalls(content string) []LlamaToolCall {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if name, args, err := parseToolJSON(content); err == nil && name != "" && name != "none" {
		return []LlamaToolCall{{Function: LlamaToolCallFunction{Name: name, Arguments: stringMapToAny(args)}}}
	}
	if args, ok := forceFSArgsFromContent(content); ok {
		return []LlamaToolCall{{Function: LlamaToolCallFunction{Name: "fs", Arguments: stringMapToAny(args)}}}
	}
	for _, obj := range extractJSONObjects(content) {
		if name, args, err := parseToolJSON(obj); err == nil && name != "" && name != "none" {
			return []LlamaToolCall{{Function: LlamaToolCallFunction{Name: name, Arguments: stringMapToAny(args)}}}
		}
		if args, ok := forceFSArgsFromContent(obj); ok {
			return []LlamaToolCall{{Function: LlamaToolCallFunction{Name: "fs", Arguments: stringMapToAny(args)}}}
		}
	}
	if args, ok := extractFSCommandFromText(content); ok {
		return []LlamaToolCall{{Function: LlamaToolCallFunction{Name: "fs", Arguments: stringMapToAny(args)}}}
	}
	return nil
}

func stringMapToAny(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func stripReasoning(s string) string {
	original := strings.TrimSpace(s)
	s = original
	for _, tag := range []string{"think", "reasoning", "thought"} {
		open, close := "<"+tag+">", "</"+tag+">"
		for {
			i := strings.Index(s, open)
			j := strings.Index(s, close)
			if j < 0 {
				break
			}
			if i >= 0 && i < j {
				s = s[:i] + s[j+len(close):]
				continue
			}
			s = s[j+len(close):]
		}
	}
	stripped := strings.TrimSpace(s)
	if stripped == "" && original != "" {
		debugf("DEBUG: stripReasoning would return empty; keeping raw content (len=%d)\n", len(original))
		return original
	}
	return stripped
}

func extractJSONObjects(s string) []string {
	var out []string
	depth := 0
	start := -1
	inStr := false
	esc := false
	for i, r := range s {
		if inStr {
			switch {
			case esc:
				esc = false
			case r == '\\':
				esc = true
			case r == '"':
				inStr = false
			}
			continue
		}
		switch r {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
}

func toolCallArgsToMapLlama(args map[string]interface{}) map[string]string {
	return normalizeArgs(args)
}

func resolveLlamaToolFromResponse(msg LlamaMessage) (name string, args map[string]string, ok bool) {
	if len(msg.ToolCalls) > 0 {
		tc := msg.ToolCalls[0]
		name = strings.TrimSpace(tc.Function.Name)
		args = toolCallArgsToMapLlama(tc.Function.Arguments)
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
	return "", nil, false
}

type LlamaCppModel struct {
	Client       *LlamaClient
	Model        string
	SystemPrompt string
}

func (m *LlamaCppModel) ModelName() string {
	if m.Model != "" {
		return m.Model
	}
	return defaultModel
}

func (m *LlamaCppModel) SetModel(name string) { m.Model = name }

func (m *LlamaCppModel) SystemPromptValue() string { return m.SystemPrompt }

func (m *LlamaCppModel) SetSystemPrompt(prompt string) { m.SystemPrompt = prompt }

func (m *LlamaCppModel) PlanNextStep(ctx context.Context, task *engine.Task) (*engine.Step, error) {
	history := ""
	for _, s := range task.Steps {
		toolInfo := "none"
		if s.ToolCall != nil {
			toolInfo = fmt.Sprintf("%s %v", s.ToolCall.Name, s.ToolCall.Args)
		}
		resultInfo := truncate(s.Result, maxStepResultChars)
		if s.ToolCall != nil && s.ToolCall.Name != "" {
			resultInfo = fmt.Sprintf("(%d chars, see tool output above)", len(s.Result))
		}
		history += fmt.Sprintf("- Step %d: %s | Tool: %s | Result: %s\n",
			s.Index, s.Plan, toolInfo, resultInfo)
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

	system := m.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = defaultSystemPrompt
	}
	system = system + "\n\n" + toolInstructions + "\n\n" + hostPlatformInstructions(runtime.GOOS)

	user := fmt.Sprintf(`Task:
%s

Previous steps:
%s

%s

%s

Call a tool if needed. Otherwise respond with a short note that no tool is required.`,
		task.Description, history, memBlock, ragBlock)

	messages := []LlamaMessage{{Role: "system", Content: system}}
	for _, s := range task.Steps {
		if s.ToolCall != nil && s.ToolCall.Name != "" {
			res := truncate(s.Result, maxStepResultChars)
			messages = append(messages, LlamaMessage{Role: "assistant", Content: fmt.Sprintf("Tool %s returned: %s", s.ToolCall.Name, res)})
		}
	}
	messages = append(messages, LlamaMessage{Role: "user", Content: user})

	var last LlamaChatResponse
	var toolful []LlamaChatResponse
	debugf("DEBUG: starting llama.cpp completion request (model %s) to %s messages=%d\n",
		m.ModelName(), m.Client.BaseURL, len(messages))
	ctx2, cancel := context.WithTimeout(ctx, Timeout*time.Second)
	defer cancel()
	err := m.Client.Chat(ctx2, messages, 0.0, func(resp LlamaChatResponse) error {
		debugf("DEBUG: received chat chunk content_len=%d toolcalls=%d raw=%q\n", len(resp.Message.Content), len(resp.Message.ToolCalls), truncate(resp.Message.Content, 500))
		last = resp
		if len(resp.Message.ToolCalls) > 0 {
			toolful = append(toolful, resp)
		}
		return nil
	})
	if err != nil {
		if last.Message.Content == "" && len(toolful) == 0 {
			return nil, err
		}
	}
	if len(toolful) > 0 {
		last = toolful[len(toolful)-1]
	}

	step := &engine.Step{
		Index: len(task.Steps),
		Plan:  strings.TrimSpace(last.Message.Content),
	}

	name, args, ok := resolveLlamaToolFromResponse(last.Message)
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

	step.ToolCall = &engine.ToolCall{Name: name, Args: args}
	if step.Plan == "" || strings.HasPrefix(strings.TrimSpace(step.Plan), "{") {
		step.Plan = fmt.Sprintf("call %s %v", name, args)
	}
	return step, nil
}

func (m *LlamaCppModel) GenerateFinalAnswer(ctx context.Context, task *engine.Task) (string, error) {
	history := ""
	usedTools := false
	for _, s := range task.Steps {
		if s.ToolCall != nil && s.ToolCall.Name != "" {
			usedTools = true
		}
		history += fmt.Sprintf("Step %d:\nPlan: %s\nResult:\n%s\n\n",
			s.Index, s.Plan, truncate(s.Result, 8000))
	}
	if len(history) > maxFinalAnswerHistoryChars {
		history = history[:maxFinalAnswerHistoryChars] + "\n...[history truncated for size]...\n"
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

	system := m.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = defaultSystemPrompt
	}
	system = system + "\n\n" + hostPlatformInstructions(runtime.GOOS)
	messages := []LlamaMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: prompt},
	}

	var fullResponse string
	var lastResp LlamaChatResponse
	ctx2, cancel := context.WithTimeout(ctx, Timeout*time.Second)
	defer cancel()
	err := m.Client.Chat(ctx2, messages, 0.0, func(resp LlamaChatResponse) error {
		fullResponse += resp.Message.Content
		lastResp = resp
		return nil
	})
	if err != nil {
		if lastResp.Message.Content == "" {
			return "", err
		}
	}
	answer := strings.TrimSpace(fullResponse)
	if strings.HasPrefix(answer, "{") {
		return "The model returned structured data instead of a final answer. Raw output:\n" + answer, nil
	}
	return answer, nil
}

var _ engine.ChatModel = (*LlamaCppModel)(nil)
var _ engine.Model = (*LlamaCppModel)(nil)
var _ memory.Embedder = (*LlamaEmbedder)(nil)
