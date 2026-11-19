package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/eval"
	"github.com/396xwz/toolloop/internal/tools"
)

func TestLlamaGenerateFinalAnswerIncludesSystemPromptAndPlatformGuidance(t *testing.T) {
	var messages []LlamaMessage

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			http.NotFound(w, r)
		case "/apply-template":
			var request struct {
				Messages            []LlamaMessage         `json:"messages"`
				AddGenerationPrompt bool                   `json:"add_generation_prompt"`
				TemplateKwargs      map[string]interface{} `json:"chat_template_kwargs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode apply-template request: %v", err)
			}
			messages = request.Messages
			if !request.AddGenerationPrompt || request.TemplateKwargs["enable_thinking"] != false {
				t.Errorf("fallback must request an answer prefix without thinking: %+v", request)
			}
			_, _ = w.Write([]byte(`{"prompt":"rendered prompt"}`))
		case "/completion":
			_, _ = w.Write([]byte(`{"content":"done","stop":true}`))
		default:
			t.Errorf("request path = %q, want /apply-template or /completion", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	model := &LlamaCppModel{
		Client:       NewLlamaClient(server.URL),
		Model:        "test-model",
		SystemPrompt: "Custom agent constraint: answer in haiku.",
	}
	model.Client.HTTP = server.Client()

	answer, err := model.GenerateFinalAnswer(context.Background(), &engine.Task{Description: "What is Go?"})
	if err != nil {
		t.Fatalf("GenerateFinalAnswer: %v", err)
	}
	if answer != "done" {
		t.Errorf("answer = %q, want done", answer)
	}
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2: %#v", len(messages), messages)
	}
	if messages[0].Role != "system" || messages[1].Role != "user" {
		t.Fatalf("message roles = %q, %q; want system, user", messages[0].Role, messages[1].Role)
	}
	if !strings.Contains(messages[0].Content, model.SystemPrompt) {
		t.Errorf("system message missing custom prompt: %q", messages[0].Content)
	}
	if want := hostPlatformInstructions(runtime.GOOS); !strings.Contains(messages[0].Content, want) {
		t.Errorf("system message missing platform guidance %q: %q", want, messages[0].Content)
	}
	if strings.Contains(messages[0].Content, toolInstructions) {
		t.Errorf("system message unexpectedly contains tool instructions: %q", messages[0].Content)
	}
}

func TestLlamaCompleteNUsesValidStopTokens(t *testing.T) {
	const nPredict = 321
	var request completionRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/completion" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode completion request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"content":"A normal completion.","stop":true}`))
	}))
	defer server.Close()

	client := NewLlamaClient(server.URL)
	client.HTTP = server.Client()
	content, stopped, err := client.CompleteN(context.Background(), "Tell me a story.", 0.7, nPredict)
	if err != nil {
		t.Fatalf("CompleteN: %v", err)
	}
	if content != "A normal completion." {
		t.Errorf("content = %q, want normal response", content)
	}
	if !stopped {
		t.Error("stopped = false, want true")
	}
	if request.NPredict != nPredict {
		t.Errorf("n_predict = %d, want %d", request.NPredict, nPredict)
	}

	wantStops := []string{"<|im_end|>", "<|endoftext|>", "</s>", "<|eot_id|>"}
	if len(request.Stop) != len(wantStops) {
		t.Fatalf("stop count = %d, want %d: %q", len(request.Stop), len(wantStops), request.Stop)
	}
	gotStops := make(map[string]bool, len(request.Stop))
	for _, stop := range request.Stop {
		if stop == "" {
			t.Error("completion request contains an empty stop sequence")
		}
		gotStops[stop] = true
	}
	for _, want := range wantStops {
		if !gotStops[want] {
			t.Errorf("completion request missing stop sequence %q: %q", want, request.Stop)
		}
	}
}

func TestLlamaCompleteNRejectsEmptyCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":" \n\t","stop":true}`))
	}))
	defer server.Close()

	client := NewLlamaClient(server.URL)
	client.HTTP = server.Client()
	content, stopped, err := client.CompleteN(context.Background(), "Tell me a story.", 0.7, 32)
	if err == nil {
		t.Fatal("CompleteN error = nil, want empty completion error")
	}
	if !strings.Contains(err.Error(), "empty completion") {
		t.Errorf("CompleteN error = %q, want clear empty completion error", err)
	}
	if content != "" {
		t.Errorf("content = %q, want empty", content)
	}
	if stopped {
		t.Error("stopped = true, want false on error")
	}
}

func TestCompletionResponseTokenUsage(t *testing.T) {
	tests := []struct {
		name string
		json string
		want eval.TokenUsage
	}{
		{
			name: "native counters",
			json: `{"tokens_evaluated":101,"tokens_predicted":23}`,
			want: eval.TokenUsage{Prompt: 101, Output: 23, Total: 124},
		},
		{
			name: "timings fallback",
			json: `{"timings":{"prompt_n":77,"predicted_n":9}}`,
			want: eval.TokenUsage{Prompt: 77, Output: 9, Total: 86},
		},
		{
			name: "OpenAI usage takes precedence",
			json: `{
				"usage":{"prompt_tokens":50,"completion_tokens":8,"total_tokens":58},
				"tokens_evaluated":49,
				"tokens_predicted":7
			}`,
			want: eval.TokenUsage{Prompt: 50, Output: 8, Total: 58},
		},
		{
			name: "slot context total is not generated tokens",
			json: `{"tokens_evaluated":4000,"n_tokens":4307}`,
			want: eval.TokenUsage{Prompt: 4000, Output: 0, Total: 4307},
		},
		{
			name: "unknown usage stays zero",
			json: `{}`,
			want: eval.TokenUsage{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response completionResponse
			if err := json.Unmarshal([]byte(test.json), &response); err != nil {
				t.Fatal(err)
			}
			if got := response.tokenUsage(); got != test.want {
				t.Fatalf("tokenUsage() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestLlamaCompleteNAggregatesUsagePerContext(t *testing.T) {
	responses := []string{
		`{"content":"first","stop":true,"tokens_evaluated":11,"tokens_predicted":3}`,
		`{"content":"second","stop":true,"timings":{"prompt_n":17,"predicted_n":5}}`,
	}
	request := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if request >= len(responses) {
			t.Fatalf("unexpected completion request %d", request+1)
		}
		_, _ = w.Write([]byte(responses[request]))
		request++
	}))
	defer server.Close()

	client := NewLlamaClient(server.URL)
	client.HTTP = server.Client()
	ctx, collector := withTokenUsageCollector(context.Background())
	for range responses {
		if _, _, err := client.CompleteN(ctx, "prompt", 0, 32); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := collector.snapshot(), (eval.TokenUsage{Prompt: 28, Output: 8, Total: 36}); got != want {
		t.Fatalf("aggregated usage = %+v, want %+v", got, want)
	}
	if got := (&tokenUsageCollector{}).snapshot(); got != (eval.TokenUsage{}) {
		t.Fatalf("empty collector = %+v, want zero usage", got)
	}
}

const seattleTask = "Create a project, that checks the weather every 5min in Seattle WA in python."

// runPlanNextStep runs PlanNextStep against a fake llama.cpp server that
// records the apply-template messages and returns fixedContent as the
// completion.
func runPlanNextStep(t *testing.T, task *engine.Task, fixedContent string) (*engine.Step, []LlamaMessage) {
	t.Helper()
	var messages []LlamaMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			http.NotFound(w, r)
		case "/apply-template":
			var request struct {
				Messages []LlamaMessage `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode apply-template request: %v", err)
			}
			messages = request.Messages
			_, _ = w.Write([]byte(`{"prompt":"rendered prompt"}`))
		case "/completion":
			_, _ = fmt.Fprintf(w, `{"content":%q,"stop":true}`, fixedContent)
		default:
			t.Errorf("request path = %q, want /apply-template or /completion", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	model := &LlamaCppModel{
		Client: NewLlamaClient(server.URL),
		Model:  "test-model",
	}
	model.Client.HTTP = server.Client()
	step, err := model.PlanNextStep(context.Background(), task)
	if err != nil {
		t.Fatalf("PlanNextStep: %v", err)
	}
	return step, messages
}

// AC1/AC2: plan.md is exempt from both fs gates, even when the task text has
// no fs keywords and no extractable paths.
func TestPlanNextStepExemptsPlanFileFromFSGates(t *testing.T) {
	task := &engine.Task{Description: seattleTask}

	step, _ := runPlanNextStep(t, task, `{"tool":"fs","op":"read","path":"plan.md"}`)
	if step.ToolCall == nil {
		t.Fatalf("step.ToolCall = nil, want fs read plan.md; Plan = %q", step.Plan)
	}
	if step.ToolCall.Name != "fs" || step.ToolCall.Args["op"] != "read" || step.ToolCall.Args["path"] != "plan.md" {
		t.Errorf("step.ToolCall = %s %v, want fs read plan.md", step.ToolCall.Name, step.ToolCall.Args)
	}
	if strings.HasPrefix(step.Plan, "Blocked:") {
		t.Errorf("step.Plan = %q, want not blocked", step.Plan)
	}

	step, _ = runPlanNextStep(t, task, `{"tool":"fs","op":"write","path":"./plan.md","content":"# Plan"}`)
	if step.ToolCall == nil {
		t.Fatalf("step.ToolCall = nil, want fs write ./plan.md; Plan = %q", step.Plan)
	}
	if step.ToolCall.Args["path"] != "./plan.md" {
		t.Errorf("path = %q, want ./plan.md", step.ToolCall.Args["path"])
	}
}

// Safety: non-plan.md paths are not exempt — the op gate and path gate still
// block, and mentioned paths stay allowed.
func TestPlanNextStepStillBlocksNonPlanFiles(t *testing.T) {
	// Op gate: "read" is not keyworded in a task with no fs keywords.
	task := &engine.Task{Description: seattleTask}
	step, _ := runPlanNextStep(t, task, `{"tool":"fs","op":"read","path":"secrets.txt"}`)
	if step.ToolCall != nil || !strings.HasPrefix(step.Plan, "Blocked:") {
		t.Fatalf("expected blocked read of secrets.txt; step = %+v", step)
	}

	// Path gate: "read" is keyworded ("file") but the path is not mentioned.
	task2 := &engine.Task{Description: "Create the file weather.py"}
	step, _ = runPlanNextStep(t, task2, `{"tool":"fs","op":"read","path":"secrets.txt"}`)
	if step.ToolCall != nil || !strings.HasPrefix(step.Plan, "Blocked:") {
		t.Fatalf("expected blocked read of unmentioned path; step = %+v", step)
	}

	// A path mentioned in the task text stays allowed.
	step, _ = runPlanNextStep(t, task2, `{"tool":"fs","op":"read","path":"weather.py"}`)
	if step.ToolCall == nil {
		t.Fatalf("expected allowed read of weather.py; Plan = %q", step.Plan)
	}
}

// Blocked steps appear as feedback alongside native call/result history.
func TestPlanNextStepIncludesBlockedStepsInHistory(t *testing.T) {
	task := &engine.Task{
		Description: seattleTask,
		Steps: []*engine.Step{
			{
				Index:    0,
				Plan:     "call fs read plan.md",
				ToolCall: &engine.ToolCall{Name: "fs", Args: map[string]string{"op": "read", "path": "plan.md"}},
				Result:   "no such file",
			},
			{Index: 1, Plan: "Blocked: fs op not allowed by task text"},
		},
	}
	_, messages := runPlanNextStep(t, task, "no more tools needed")
	if len(messages) != 5 {
		t.Fatalf("message count = %d, want 5: %#v", len(messages), messages)
	}
	if messages[0].Role != "system" {
		t.Errorf("messages[0].Role = %q, want system", messages[0].Role)
	}
	if messages[1].Role != "user" {
		t.Errorf("messages[1] = %#v, want user task", messages[1])
	}
	if messages[2].Role != "assistant" || len(messages[2].ToolCalls) != 1 {
		t.Fatalf("messages[2] = %#v, want assistant tool call", messages[2])
	}
	if messages[3].Role != "tool" || messages[3].Content != "no such file" || messages[3].ToolCallID != messages[2].ToolCalls[0].ID {
		t.Errorf("messages[3] = %#v, want matching tool result", messages[3])
	}
	if messages[4].Role != "user" || messages[4].Content != "Blocked: fs op not allowed by task text" {
		t.Errorf("messages[4] = %#v, want blocked-response feedback", messages[4])
	}
}

func TestLlamaNativeToolProtocolAndHistory(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{"browser", "url", "https://www.google.com/search?q=current+weather+in+Seattle+WA"},
		{"scrape", "url", "https://www.weather.com/en-US/weather/today/l/USWA0997/5391847"},
		{"web_search", "query", "current temperature in Seattle, WA"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected legacy request: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				var request struct {
					Messages []LlamaMessage `json:"messages"`
					Tools    []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tools"`
					TemplateKwargs map[string]interface{} `json:"chat_template_kwargs"`
					MaxTokens      int                    `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				if request.TemplateKwargs["enable_thinking"] != false || request.MaxTokens != 2048 {
					t.Errorf("request must disable thinking, not raise the budget: %+v", request)
				}
				if len(request.Tools) != 1 || request.Tools[0].Function.Name != test.name {
					t.Errorf("tool definitions do not match registry: %+v", request.Tools)
				}
				messages := request.Messages
				if len(messages) != 4 || messages[0].Role != "system" || messages[1].Role != "user" ||
					messages[2].Role != "assistant" || len(messages[2].ToolCalls) != 1 ||
					messages[3].Role != "tool" {
					t.Fatalf("incorrect native history: %+v", messages)
				}
				if messages[3].ToolCallID != messages[2].ToolCalls[0].ID || messages[3].Content != "No good results found" {
					t.Errorf("tool response is not matched to its call: %+v", messages[3])
				}
				args, _ := json.Marshal(map[string]string{test.key: test.value})
				// Captured Gemma chat responses carry arguments as a JSON string;
				// native tool markers are decoded by llama.cpp, not text regexes.
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"choices": []interface{}{map[string]interface{}{
						"finish_reason": "tool_calls",
						"message": map[string]interface{}{
							"role": "assistant", "content": nil,
							"reasoning_content": `<|tool_call>call:shell{cmd: "not an action"}<tool_call|>`,
							"tool_calls": []interface{}{map[string]interface{}{
								"id": "native_call", "type": "function",
								"function": map[string]interface{}{"name": test.name, "arguments": string(args)},
							}},
						},
					}},
				})
			}))
			defer server.Close()
			reg := tools.NewToolRegistry()
			reg.Register(test.name, tools.WebSearchTool{})
			model := &LlamaCppModel{Client: NewLlamaClient(server.URL)}
			step, err := model.PlanNextStep(context.Background(), &engine.Task{
				Description: "Current Seattle weather", Tools: reg,
				Steps: []*engine.Step{{Index: 0, ToolCall: &engine.ToolCall{
					Name: "web_search", Args: map[string]string{"query": "Seattle"},
				}, Result: "No good results found"}},
			})
			if err != nil || step.ToolCall == nil || step.ToolCall.Name != test.name || step.ToolCall.Args[test.key] != test.value {
				t.Fatalf("native call not recovered: step=%+v err=%v", step, err)
			}
		})
	}
}

func TestLlamaNativeFinalRejectsCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if _, ok := request["tools"]; ok {
			t.Error("final-answer request must not offer tools")
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{
			"role":"assistant","content":"I need another search.",
			"tool_calls":[{"id":"native_final","type":"function","function":{
				"name":"web_search","arguments":{"query":"Seattle weather"}
			}}]
		}}]}`))
	}))
	defer server.Close()
	model := &LlamaCppModel{Client: NewLlamaClient(server.URL)}
	answer, err := model.GenerateFinalAnswer(context.Background(), &engine.Task{Description: "Seattle weather"})
	if err == nil || answer != "" {
		t.Fatalf("native final call accepted as an answer: %q, err=%v", answer, err)
	}
}

func TestLlamaNativeChatRejectsTruncationAndReasoningOnlyActions(t *testing.T) {
	for _, finish := range []string{"length", "stop"} {
		t.Run(finish, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"choices": []interface{}{map[string]interface{}{
						"finish_reason": finish,
						"message": map[string]string{
							"role": "assistant", "content": "",
							"reasoning_content": `<|tool_call>call:web_search{query:Seattle}<tool_call|>`,
						},
					}},
				})
			}))
			defer server.Close()
			model := &LlamaCppModel{Client: NewLlamaClient(server.URL)}
			step, err := model.PlanNextStep(context.Background(), &engine.Task{Description: "Seattle weather"})
			if finish == "length" {
				if err == nil || !strings.Contains(err.Error(), "token budget") || step != nil {
					t.Fatalf("truncation must fail instead of causing 32 retries: step=%+v err=%v", step, err)
				}
			} else if err != nil || step.ToolCall != nil || !strings.HasPrefix(step.Plan, "Blocked:") {
				t.Fatalf("reasoning-only action escaped: step=%+v err=%v", step, err)
			}
		})
	}
}
