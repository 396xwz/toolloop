package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/eval"
	"github.com/396xwz/toolloop/internal/memory"
	"github.com/396xwz/toolloop/internal/tools"
)

const defaultLlamaURL = "http://localhost:8080"

type LlamaMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []LlamaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type LlamaToolCallFunction struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

func (f *LlamaToolCallFunction) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	args := raw.Arguments
	if len(args) > 0 && args[0] == '"' {
		var text string
		if err := json.Unmarshal(args, &text); err != nil {
			return err
		}
		args = []byte(text)
	}
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, &f.Arguments)
}

type LlamaToolCall struct {
	ID       string                `json:"id,omitempty"`
	Type     string                `json:"type,omitempty"`
	Function LlamaToolCallFunction `json:"function"`
}

type llamaHTTPError struct {
	status  int
	message string
}

func (e *llamaHTTPError) Error() string { return e.message }

type LlamaChatResponse struct {
	Message         LlamaMessage
	Done            bool
	Raw             string
	Stopped         bool // true when the server stopped because of the token budget (no stop token)
	NativeToolCalls bool
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
		return &llamaHTTPError{resp.StatusCode, fmt.Sprintf("%s %s: %s: %s", http.MethodPost, c.url(path), resp.Status, truncate(string(data), 500))}
	}
	if path == "/completion" || path == "/v1/chat/completions" {
		debugf("DEBUG: %s response: %s\n", path, data)
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
	body := map[string]interface{}{
		"messages":              messages,
		"add_generation_prompt": true,
		"chat_template_kwargs":  map[string]interface{}{"enable_thinking": false},
	}
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
	Prompt      string          `json:"prompt"`
	NPredict    int             `json:"n_predict"`
	Temperature float64         `json:"temperature"`
	Stream      bool            `json:"stream"`
	CachePrompt bool            `json:"cache_prompt"`
	Stop        []string        `json:"stop,omitempty"`
	JSONSchema  json.RawMessage `json:"json_schema,omitempty"`
}

type completionResponse struct {
	Content         string                    `json:"content"`
	Stop            bool                      `json:"stop"`
	Model           string                    `json:"model"`
	TokensPredicted *int                      `json:"tokens_predicted"`
	TokensEvaluated *int                      `json:"tokens_evaluated"`
	NTokens         *int                      `json:"n_tokens"`
	Timings         completionResponseTimings `json:"timings"`
	Usage           completionResponseUsage   `json:"usage"`
}

type completionResponseTimings struct {
	PromptN    *int `json:"prompt_n"`
	PredictedN *int `json:"predicted_n"`
}

type completionResponseUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
}

func (r completionResponse) tokenUsage() eval.TokenUsage {
	var usage eval.TokenUsage
	prompt, promptKnown := firstNonNegative(r.Usage.PromptTokens, r.TokensEvaluated, r.Timings.PromptN)
	output, outputKnown := firstNonNegative(r.Usage.CompletionTokens, r.TokensPredicted, r.Timings.PredictedN)
	total, totalKnown := firstNonNegative(r.Usage.TotalTokens, r.NTokens)

	if promptKnown {
		usage.Prompt = prompt
	}
	if outputKnown {
		usage.Output = output
	}
	if totalKnown {
		// llama.cpp's slot-release n_tokens value is prompt.tokens.size():
		// the final context size, not the number of generated tokens.
		usage.Total = total
	} else if promptKnown && outputKnown {
		usage.Total = prompt + output
	}
	return usage
}

func firstNonNegative(values ...*int) (int, bool) {
	for _, value := range values {
		if value != nil && *value >= 0 {
			return *value, true
		}
	}
	return 0, false
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
	return c.completeN(ctx, prompt, temperature, nPredict, nil)
}

func (c *LlamaClient) completeN(ctx context.Context, prompt string, temperature float64, nPredict int, schema json.RawMessage) (string, bool, error) {
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
		JSONSchema:  schema,
	}
	var out completionResponse
	debugf("DEBUG: POST %s prompt_len=%d n_predict=%d\n", c.url("/completion"), len(prompt), req.NPredict)
	if err := c.postJSON(ctx, "/completion", req, &out); err != nil {
		return "", false, err
	}
	recordTokenUsage(ctx, out.tokenUsage())
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
	return c.chatBudget(ctx, messages, temperature, nPredict, nil, fn)
}

// ChatJSON constrains this request to a JSON object without changing ordinary
// chat defaults. The caller's prompt and parser define the verdict's fields.
func (c *LlamaClient) ChatJSON(ctx context.Context, messages []LlamaMessage, temperature float64, fn func(LlamaChatResponse) error) error {
	return c.chatBudget(ctx, messages, temperature, c.NPredict, json.RawMessage(`{"type":"object"}`), fn)
}

func (c *LlamaClient) chatBudget(ctx context.Context, messages []LlamaMessage, temperature float64, nPredict int, schema json.RawMessage, fn func(LlamaChatResponse) error) error {
	prompt, ok := c.applyTemplate(ctx, messages)
	if !ok {
		prompt = fallbackPrompt(messages)
	}
	content, stopped, err := c.completeN(ctx, prompt, temperature, nPredict, schema)
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

// ChatTools lets llama.cpp apply the model's native tool grammar and decode
// calls separately from reasoning, instead of guessing that grammar in text.
func (c *LlamaClient) ChatTools(ctx context.Context, model string, messages []LlamaMessage, definitions []interface{}, fn func(LlamaChatResponse) error) error {
	body := map[string]interface{}{
		"model": model, "messages": messages, "stream": false,
		"temperature": 0.0, "max_tokens": c.NPredict,
		"chat_template_kwargs": map[string]interface{}{"enable_thinking": false},
	}
	if len(definitions) > 0 {
		body["tools"] = definitions
	}
	var out struct {
		Choices []struct {
			Message      LlamaMessage `json:"message"`
			FinishReason string       `json:"finish_reason"`
		} `json:"choices"`
		Usage completionResponseUsage `json:"usage"`
	}
	if err := c.postJSON(ctx, "/v1/chat/completions", body, &out); err != nil {
		if status, ok := err.(*llamaHTTPError); ok && (status.status == http.StatusNotFound || status.status == http.StatusNotImplemented) {
			return c.Chat(ctx, messages, 0, fn)
		}
		return err
	}
	recordTokenUsage(ctx, (completionResponse{Usage: out.Usage}).tokenUsage())
	if len(out.Choices) == 0 {
		return fmt.Errorf("llama.cpp returned no chat choices")
	}
	choice := out.Choices[0]
	if choice.FinishReason == "length" {
		return fmt.Errorf("llama.cpp chat response exceeded the token budget")
	}
	msg := choice.Message
	nativeToolCalls := len(msg.ToolCalls) > 0
	raw := msg.Content
	msg.Content = stripReasoning(msg.Content)
	if len(msg.ToolCalls) == 0 {
		msg.ToolCalls = extractLlamaToolCalls(msg.Content)
	}
	if fn == nil {
		return nil
	}
	return fn(LlamaChatResponse{Message: msg, Done: true, Raw: raw, NativeToolCalls: nativeToolCalls})
}

func llamaToolDefinitions(reg tools.ToolRegistry) []interface{} {
	names := reg.Names()
	allowed := make(map[string]bool)
	for _, name := range names {
		allowed[name] = true
	}
	var definitions []interface{}
	for _, tool := range buildOllamaTools() {
		if len(names) == 0 || allowed[tool.Function.Name] {
			definitions = append(definitions, tool)
			delete(allowed, tool.Function.Name)
		}
	}
	for _, name := range names {
		if !allowed[name] {
			continue
		}
		properties := map[string]interface{}{}
		required := []string{}
		switch name {
		case "agent":
			required = []string{"name", "task"}
			for _, key := range []string{"name", "task", "prompt"} {
				properties[key] = map[string]string{"type": "string"}
			}
		case "verdict":
			required = []string{"status"}
			for _, key := range []string{"status", "reason"} {
				properties[key] = map[string]string{"type": "string"}
			}
		}
		definitions = append(definitions, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": name, "description": "Use the arguments described in the system instructions.",
				"parameters": map[string]interface{}{
					"type": "object", "properties": properties, "required": required,
					"additionalProperties": map[string]string{"type": "string"},
				},
			},
		})
	}
	return definitions
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
	content = normalizeResponse(content, false)
	if content == "" {
		return nil
	}
	for _, obj := range append([]string{content}, extractJSONObjects(content)...) {
		if !isToolEnvelope(obj) {
			continue
		}
		name, args, err := parseToolJSON(obj)
		if err == nil && name != "" && name != "none" &&
			!argsLookCorrupt(args) && hasMinimumArgs(name, args) {
			return []LlamaToolCall{{Function: LlamaToolCallFunction{Name: name, Arguments: stringMapToAny(args)}}}
		}
		if (name == "" || name == "fs") && strings.HasPrefix(cleanJSON(obj), "{") {
			if args, ok := forceFSArgsFromContent(obj); ok && hasMinimumArgs("fs", args) {
				return []LlamaToolCall{{Function: LlamaToolCallFunction{Name: "fs", Arguments: stringMapToAny(args)}}}
			}
		}
	}
	if args, ok := extractFSCommandFromText(content); ok && !argsLookCorrupt(args) && hasMinimumArgs("fs", args) {
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

var responseMarker = regexp.MustCompile(`^(?:</?(?:think|reasoning|thought)>|\[/?THINK\]|<\|channel\|?>\s*(analysis|thought|reasoning|final|answer|commentary)\b|<channel\|>|<\|(?:start|im_start)\|>assistant|<\|start_header_id\|>assistant<\|end_header_id\|>|<\|(?:message|im_sep|end|im_end|eot_id|endoftext|fim_suffix|return)\|>)`)

// Keep reasoning out of both tool extraction and user-visible answers. An
// unfinished reasoning block is discarded, never used as a fallback answer.
func stripReasoning(s string) string {
	return normalizeResponse(s, true)
}

func normalizeResponse(s string, preserveCode bool) string {
	var out strings.Builder
	var tags []string
	channelThought := false
	for i := 0; i < len(s); {
		hidden := len(tags) > 0 || channelThought
		if preserveCode && s[i] == '`' {
			n := 1
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			if end := strings.Index(s[i+n:], s[i:i+n]); end >= 0 {
				end += i + 2*n
				if !hidden {
					out.WriteString(s[i:end])
				}
				i = end
				continue
			}
		}
		// Only structured values protect their strings. A prose inch mark or
		// unmatched quotation must not hide a subsequent reasoning boundary.
		if s[i] == '{' || s[i] == '[' {
			var value json.RawMessage
			decoder := json.NewDecoder(strings.NewReader(s[i:]))
			if decoder.Decode(&value) == nil {
				end := i + int(decoder.InputOffset())
				if !hidden {
					out.WriteString(s[i:end])
				}
				i = end
				continue
			}
		}
		if s[i] == '<' || s[i] == '[' {
			if match := responseMarker.FindStringSubmatch(s[i:]); match != nil {
				token := match[0]
				// A quoted marker literal in an explanation is not a wrapper.
				if i > 0 && s[i-1] == '"' && i+len(token) < len(s) && s[i+len(token)] == '"' {
					if !hidden {
						out.WriteString(token)
					}
					i += len(token)
					continue
				}
				switch {
				case strings.HasPrefix(token, "</") || token == "[/THINK]":
					tag := token[2 : len(token)-1]
					if token == "[/THINK]" {
						tag = "think"
					}
					if len(tags) > 0 {
						if tags[len(tags)-1] == tag {
							tags = tags[:len(tags)-1]
						}
					} else {
						// Some templates prefill the opening think tag.
						out.Reset()
					}
				case token == "[THINK]":
					tags = append(tags, "think")
				case token == "<think>" || token == "<reasoning>" || token == "<thought>":
					tags = append(tags, token[1:len(token)-1])
				case match[1] != "":
					channelThought = match[1] == "analysis" || match[1] == "thought" || match[1] == "reasoning"
				case token == "<channel|>":
					channelThought = false
				}
				i += len(token)
				continue
			}
		}
		if !hidden {
			out.WriteByte(s[i])
		}
		i++
	}
	return strings.TrimSpace(out.String())
}

var toolIntent = regexp.MustCompile(`(?mi)(?:"(?:tool|tool_name|tool_calls|args|arguments)"\s*:|^\s*(?:tool|tool_name)\s*[:=]|^\s*to=\S+|^\s*fs\s+(?:op=|read|write|edit|list|tree|info)\b|<tool_call\b|<function=)`)
var protocolEcho = regexp.MustCompile(`(?mi)(\|\s*(?:Tool|Result):|see tool output above|Call a tool if needed\. Otherwise respond with a short note that no tool is required\.|<\||<channel\||^\s*(?:</?(?:think|reasoning|thought)\b|\[/?THINK\]|Previous steps:))`)

func isToolEnvelope(content string) bool {
	raw := cleanJSON(content)
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return false
	}
	explicit := fields["tool"] != nil || fields["tool_name"] != nil
	if fields["tool_calls"] != nil ||
		((explicit || fields["name"] != nil) && (fields["args"] != nil || fields["arguments"] != nil)) ||
		(explicit && (len(fields) == 1 || fields["plan"] != nil)) {
		return true
	}
	if function := fields["function"]; function != nil && isToolEnvelope(string(function)) {
		return true
	}
	name, _, err := parseToolJSON(raw)
	if err == nil {
		if explicit {
			return true
		}
		switch name {
		case "fs", "shell", "browser", "web_search", "scrape", "python":
			return true
		}
	}
	if fields["name"] == nil {
		_, ok := forceFSArgsFromContent(raw)
		return ok
	}
	return false
}

func responseProblem(content string) string {
	if strings.TrimSpace(content) == "" {
		return "empty response after removing reasoning"
	}
	for _, obj := range extractJSONObjects(content) {
		if !json.Valid([]byte(obj)) {
			continue
		}
		if isToolEnvelope(obj) {
			return "tool request instead of plain text"
		}
		content = strings.Replace(content, obj, "", 1)
	}
	switch {
	case protocolEcho.MatchString(content):
		return "echoed or malformed response protocol"
	case toolIntent.MatchString(content):
		return "tool request instead of plain text"
	}
	return ""
}

var fencedResponseCode = regexp.MustCompile("(?ms)^[ \\t]*```+([^\\n`]*)\\n(.*?)^[ \\t]*```+[ \\t]*(?:\\n|$)")
var inlineResponseCode = regexp.MustCompile("`+[^`\\n]+`+")
var actionPreface = regexp.MustCompile(`(?mi)(?:^|\n)\s*(?:next action|action|tool call|next tool call)\s*:\s*$`)
var examplePreface = regexp.MustCompile(`(?i)\b(?:example|sample|syntax|configur(?:e|ation)|data|schema|code|script|snippet|function|program|implementation)\b`)

func finalResponseText(answer string) string {
	var out strings.Builder
	start := 0
	for _, match := range fencedResponseCode.FindAllStringSubmatchIndex(answer, -1) {
		prefix := strings.TrimSpace(answer[:match[0]])
		language := strings.ToLower(strings.TrimSpace(answer[match[2]:match[3]]))
		body := strings.TrimSpace(answer[match[4]:match[5]])
		explanation := examplePreface.MatchString(prefix) && !actionPreface.MatchString(prefix) &&
			!protocolEcho.MatchString(prefix) && !toolIntent.MatchString(prefix)
		code := language != "" && language != "json" && language != "text" &&
			!actionPreface.MatchString(prefix) && !protocolEcho.MatchString(body) &&
			!isToolEnvelope(body) && !(strings.HasPrefix(body, "{") && toolIntent.MatchString(body))
		if explanation || code {
			out.WriteString(answer[start:match[0]])
			out.WriteByte('\n')
			start = match[1]
		}
	}
	out.WriteString(answer[start:])
	text := out.String()
	out.Reset()
	start = 0
	for _, match := range inlineResponseCode.FindAllStringIndex(text, -1) {
		prefix, suffix := text[:match[0]], text[match[1]:]
		body := strings.Trim(text[match[0]:match[1]], "`")
		action := isToolEnvelope(body) || toolIntent.MatchString(body)
		if strings.TrimSpace(prefix+suffix) != "" && !actionPreface.MatchString(strings.TrimSpace(prefix)) &&
			(!action || examplePreface.MatchString(prefix+" "+suffix)) {
			out.WriteString(text[start:match[0]])
			out.WriteByte(' ')
			start = match[1]
		}
	}
	out.WriteString(text[start:])
	return out.String()
}

func finalAnswer(content string, hasToolCalls bool) (string, error) {
	answer := stripReasoning(content)
	if hasToolCalls {
		return "", fmt.Errorf("model returned a tool request instead of a final answer")
	}
	if answer == "" {
		return "", fmt.Errorf("invalid final answer: empty response after removing reasoning")
	}
	// Explanatory code examples are data; standalone action envelopes and
	// blocks introduced as the next action still require actual execution.
	if text := finalResponseText(answer); strings.TrimSpace(text) != "" {
		if problem := responseProblem(text); problem != "" {
			return "", fmt.Errorf("invalid final answer: %s", problem)
		}
	}
	return answer, nil
}

func stepHistoryPlan(step *engine.Step) string {
	if step.ToolCall != nil && step.ToolCall.Name != "" {
		return fmt.Sprintf("call %s %v", step.ToolCall.Name, step.ToolCall.Args)
	}
	plan := stripReasoning(step.Plan)
	if responseProblem(plan) != "" {
		return ""
	}
	return plan
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
			inStr = depth > 0
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
			s.Index, stepHistoryPlan(s), toolInfo, resultInfo)
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

	messages := []LlamaMessage{{Role: "system", Content: system}, {Role: "user", Content: user}}
	for _, s := range task.Steps {
		if s.ToolCall != nil && s.ToolCall.Name != "" {
			res := truncate(s.Result, maxStepResultChars)
			id := fmt.Sprintf("call_%d", s.Index)
			messages = append(messages,
				LlamaMessage{Role: "assistant", ToolCalls: []LlamaToolCall{{
					ID: id, Type: "function",
					Function: LlamaToolCallFunction{Name: s.ToolCall.Name, Arguments: stringMapToAny(s.ToolCall.Args)},
				}}},
				LlamaMessage{Role: "tool", Name: s.ToolCall.Name, ToolCallID: id, Content: res},
			)
			debugf("DEBUG: tool result %s (%d chars):\n%s\n", s.ToolCall.Name, len(s.Result), s.Result)
		} else if strings.HasPrefix(strings.TrimSpace(s.Plan), "Blocked:") {
			// Blocked steps carry no ToolCall; surface the block reason so the
			// model sees its own blocked attempts and adapts.
			messages = append(messages, LlamaMessage{Role: "user", Content: s.Plan})
		}
	}

	var last LlamaChatResponse
	var toolful []LlamaChatResponse
	debugf("DEBUG: starting llama.cpp completion request (model %s) to %s messages=%d\n",
		m.ModelName(), m.Client.BaseURL, len(messages))
	ctx2, cancel := context.WithTimeout(ctx, Timeout*time.Second)
	defer cancel()
	err := m.Client.ChatTools(ctx2, m.ModelName(), messages, llamaToolDefinitions(task.Tools), func(resp LlamaChatResponse) error {
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

		// plan.md is the orchestrator's working file; exempt it from both
		// gates so the main REPL agent can always read/write its plan.
		if !isPlanFile(path) && !isWorkspaceFSRead(op, path) {
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

func (m *LlamaCppModel) GenerateFinalAnswer(ctx context.Context, task *engine.Task) (string, error) {
	history := ""
	usedTools := false
	for _, s := range task.Steps {
		if s.ToolCall != nil && s.ToolCall.Name != "" {
			usedTools = true
		}
		history += fmt.Sprintf("Step %d:\nPlan: %s\nResult:\n%s\n\n",
			s.Index, stepHistoryPlan(s), truncate(s.Result, 8000))
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
	system = system + "\n\n" + hostPlatformInstructions(runtime.GOOS) + "\n\n" + currentTimeContext(time.Now())
	messages := []LlamaMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: prompt},
	}

	var fullResponse string
	var lastResp LlamaChatResponse
	nativeToolCalls := false
	ctx2, cancel := context.WithTimeout(ctx, Timeout*time.Second)
	defer cancel()
	err := m.Client.ChatTools(ctx2, m.ModelName(), messages, nil, func(resp LlamaChatResponse) error {
		fullResponse += resp.Message.Content
		nativeToolCalls = nativeToolCalls || resp.NativeToolCalls
		lastResp = resp
		return nil
	})
	if err != nil {
		if lastResp.Message.Content == "" {
			return "", err
		}
	}
	return finalAnswer(fullResponse, nativeToolCalls)
}

var _ engine.ChatModel = (*LlamaCppModel)(nil)
var _ engine.Model = (*LlamaCppModel)(nil)
var _ memory.Embedder = (*LlamaEmbedder)(nil)
