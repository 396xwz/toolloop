package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/ollama/ollama/api"
)

func responseTestModel(t *testing.T, backend string, replies ...string) (engine.ChatModel, *[]LlamaMessage) {
	t.Helper()
	var messages []LlamaMessage
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apply-template" || r.URL.Path == "/api/chat" || r.URL.Path == "/v1/chat/completions" {
			var request struct {
				Messages []LlamaMessage `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			messages = request.Messages
		}
		if r.URL.Path == "/apply-template" {
			_ = json.NewEncoder(w).Encode(map[string]string{"prompt": "rendered prompt"})
			return
		}
		if requests >= len(replies) {
			t.Errorf("unexpected extra response request %d", requests+1)
			http.Error(w, "no reply", http.StatusInternalServerError)
			return
		}
		reply := replies[requests]
		requests++
		if r.URL.Path == "/completion" {
			_ = json.NewEncoder(w).Encode(completionResponse{Content: reply, Stop: true})
		} else if r.URL.Path == "/v1/chat/completions" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []interface{}{map[string]interface{}{
					"message":       map[string]string{"role": "assistant", "content": reply},
					"finish_reason": "stop",
				}},
			})
		} else if r.URL.Path == "/api/chat" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message": map[string]string{"role": "assistant", "content": reply}, "done": true,
			})
		} else {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if backend == "ollama" {
		baseURL, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		return &OllamaModel{Client: api.NewClient(baseURL, server.Client())}, &messages
	}
	return &LlamaCppModel{Client: NewLlamaClient(server.URL)}, &messages
}

func TestResponseHandling(t *testing.T) {
	const search = `{"tool":"web_search","args":{"query":"current temperature Chicago IL"}}`
	tests := []struct {
		name       string
		content    string
		wantAnswer string
		wantTool   bool
		blocked    bool
	}{
		{name: "plain answer", content: "Chicago is 42°F.", wantAnswer: "Chicago is 42°F."},
		{name: "completion note", content: "No more tools needed.", wantAnswer: "No more tools needed."},
		{name: "reported channel action", content: "<|channel>thought\nNeed current data.\n<channel|>" + search, wantTool: true},
		{name: "reported channel plus echoed history", content: "<|channel>thought\nNeed current data.\n<channel|>" + search +
			"\n | Tool: web_search map[query:Chicago] | Result: (507 chars, see tool output above)\n" +
			"Call a tool if needed. Otherwise respond with a short note that no tool is required.", wantTool: true},
		{name: "thought-only action", content: "<think>" + search + "</think>", blocked: true},
		{name: "fenced thought-only action", content: "```text\n<think>" + search + "</think>\n```", blocked: true},
		{name: "mistral thought-only action", content: "[THINK]" + search + "[/THINK]", blocked: true},
		{name: "unfinished thought action", content: "<think>" + search, blocked: true},
		{name: "unfinished channel action", content: "<|channel>thought\n" + search, blocked: true},
		{name: "thought action then answer", content: "<thought>" + search + "</thought>Chicago is 42°F.", wantAnswer: "Chicago is 42°F."},
		{name: "prefilled thought", content: search + "</think>Chicago is 42°F.", wantAnswer: "Chicago is 42°F."},
		{name: "harmony channels", content: "<|start|>assistant<|channel|>analysis<|message|>" + search +
			"<|end|><|start|>assistant<|channel|>final<|message|>Chicago is 42°F.<|return|>", wantAnswer: "Chicago is 42°F."},
		{name: "nested reasoning", content: "<think>before<reasoning>" + search + "</reasoning>after</think>Chicago is 42°F.", wantAnswer: "Chicago is 42°F."},
		{name: "chatml answer", content: "<|im_start|>assistant\n<think>" + search + "</think>Chicago is 42°F.<|im_end|>", wantAnswer: "Chicago is 42°F."},
		{name: "llama header answer", content: "<|start_header_id|>assistant<|end_header_id|>\nChicago is 42°F.<|eot_id|>", wantAnswer: "Chicago is 42°F."},
		{name: "fenced action", content: "Next action:\n```json\n" + search + "\n```", wantTool: true},
		{name: "invalid args", content: `{"tool":"web_search","args":{}}`, blocked: true},
		{name: "invalid tool not reinterpreted as fs", content: `Next action: {"tool":"web_search","op":"read","path":"plan.md"}`, blocked: true},
		{name: "incomplete action", content: `{"tool":"web_search","args":{"query":"Chicago"`, blocked: true},
		{name: "malformed named action", content: `Next action: {"name":"web_search","arguments":{}}`, blocked: true},
		{name: "malformed custom action", content: `Next action: {"name":"custom_tool","arguments":null}`, blocked: true},
		{name: "echoed protocol", content: "- Step 5: | Tool: web_search map[query:Chicago] | Result: (507 chars, see tool output above)", blocked: true},
		{name: "echoed instruction", content: "Call a tool if needed. Otherwise respond with a short note that no tool is required.", blocked: true},
		{name: "unknown channel", content: "<|channel|>unknown<|message|>not a final answer", blocked: true},
		{name: "ordinary code example", content: "Use `fmt.Println(\"<think>\")` to print the tag.", wantAnswer: "Use `fmt.Println(\"<think>\")` to print the tag."},
		{name: "quoted marker", content: `The "<think>" string is a reasoning marker.`, wantAnswer: `The "<think>" string is a reasoning marker.`},
	}
	for _, backend := range []string{"llama", "ollama"} {
		for _, test := range tests {
			t.Run(backend+"/"+test.name, func(t *testing.T) {
				model, _ := responseTestModel(t, backend, test.content, test.content)
				task := &engine.Task{Description: "What is the current weather in Chicago?"}
				step, err := model.PlanNextStep(context.Background(), task)
				if err != nil {
					t.Fatal(err)
				}
				if (step.ToolCall != nil) != test.wantTool || strings.HasPrefix(step.Plan, "Blocked:") != test.blocked {
					t.Fatalf("unexpected planning result: %+v", step)
				}
				if test.wantTool {
					if step.ToolCall.Name != "web_search" || step.ToolCall.Args["query"] != "current temperature Chicago IL" {
						t.Fatalf("tool changed: %+v", step.ToolCall)
					}
					if strings.Contains(step.Plan, "<") || strings.Contains(step.Plan, "| Result:") {
						t.Fatalf("raw planning response persisted: %q", step.Plan)
					}
				}
				answer, err := model.GenerateFinalAnswer(context.Background(), task)
				if test.wantAnswer == "" {
					if err == nil || answer != "" {
						t.Fatalf("invalid final answer accepted: %q, err=%v", answer, err)
					}
				} else if err != nil || answer != test.wantAnswer {
					t.Fatalf("answer=%q err=%v, want %q", answer, err, test.wantAnswer)
				}
			})
		}
	}
}

func TestToolExtractionPreservesVisibleActionAndArguments(t *testing.T) {
	hidden := `{"tool":"shell","args":{"cmd":"not an action"}}`
	visible := `{"tool_name":"browser","arguments":{"url":"https://example.com/?q=<think>literal</think>"}}`
	for _, content := range []string{
		"<think>" + hidden + "</think>" + visible,
		`<think>{"tool":"shell","args":{"cmd":"echo </think>"}}</think>` + visible,
		"<|channel|>analysis<|message|>" + hidden + "<|channel|>commentary<|message|>" + visible,
		`{"tool":"web_search","args":{}}` + "\n" + visible,
	} {
		calls := extractLlamaToolCalls(content)
		if len(calls) != 1 || calls[0].Function.Name != "browser" ||
			calls[0].Function.Arguments["url"] != "https://example.com/?q=<think>literal</think>" {
			t.Fatalf("visible action or argument corrupted: %+v", calls)
		}
	}
	calls := extractLlamaToolCalls(`<think>` + hidden + `</think>{"name":"custom_tool","arguments":{"value":"unchanged"}}`)
	if len(calls) != 1 || calls[0].Function.Name != "custom_tool" || calls[0].Function.Arguments["value"] != "unchanged" {
		t.Fatalf("custom tool changed: %+v", calls)
	}
	for _, content := range []string{
		`{"tool":"fs","args":{"op":"read","path":"plan.md"}}`,
		"```json\n" + `{"op":"read","path":"plan.md"}` + "\n```",
		"fs op=read path=plan.md",
	} {
		calls := extractLlamaToolCalls(content)
		if len(calls) != 1 || calls[0].Function.Name != "fs" || calls[0].Function.Arguments["path"] != "plan.md" {
			t.Fatalf("existing fs format no longer supported: %q -> %+v", content, calls)
		}
	}
}

func TestReasoningProseQuotesPreserveVisibleActions(t *testing.T) {
	const search = `{"tool":"web_search","args":{"query":"15 inch display"}}`
	for _, backend := range []string{"llama", "ollama"} {
		for _, prefix := range []string{
			`<think>The display is 15" wide.</think>`,
			`<think>An unmatched " quotation.<reasoning>Nested reasoning.</reasoning></think>`,
			"<|channel>thought\nThe display is 15\" wide.\n<channel|>",
			"<|channel|>analysis<|message|>An unmatched \" quotation.<|channel|>commentary<|message|>",
			"<think>\n```json\n{\"message\":\"</think>\"}\n```\nThe display is 15\" wide.</think>",
			`The display is 15" wide. `,
		} {
			t.Run(backend+"/"+prefix, func(t *testing.T) {
				model, _ := responseTestModel(t, backend, prefix+search)
				step, err := model.PlanNextStep(context.Background(), &engine.Task{Description: "Find a 15 inch display"})
				if err != nil || step.ToolCall == nil || step.ToolCall.Name != "web_search" ||
					step.ToolCall.Args["query"] != "15 inch display" {
					t.Fatalf("visible action lost after prose quotation: step=%+v err=%v", step, err)
				}
			})
		}
		for _, content := range []string{
			`<think>The display is 15" wide. ` + search + `</think>`,
			`<|channel>thought The display is 15" wide. ` + search + `<channel|>`,
			`<think>{"message":"</think>","example":` + search + `}</think>`,
		} {
			t.Run(backend+"/reasoning-only/"+content, func(t *testing.T) {
				model, _ := responseTestModel(t, backend, content)
				step, err := model.PlanNextStep(context.Background(), &engine.Task{Description: "Find a 15 inch display"})
				if err != nil || step.ToolCall != nil || !strings.HasPrefix(step.Plan, "Blocked:") {
					t.Fatalf("reasoning-only action escaped: step=%+v err=%v", step, err)
				}
			})
		}
	}
}

func TestFinalAnswerPreservesExplanatoryJSONAndCode(t *testing.T) {
	const search = `{"tool":"web_search","args":{"query":"Chicago"}}`
	for _, backend := range []string{"llama", "ollama"} {
		for _, answer := range []string{
			"Configure the runtime as follows:\n```json\n{\"name\":\"python\",\"version\":\"3.12\"}\n````",
			`{"name":"python","version":"3.12"}`,
			`{"tool":"python","version":"3.12"}`,
			`{"name":"main.py","content":"print('hello')"}`,
			`{"name":"python","note":"<|channel|>analysis is a marker"}`,
			"Example tool-call syntax:\n```json\n" + search + "\n```",
			"Malformed input example:\n```json\n{\"tool\":\"web_search\",\"args\":\n```",
			"Here is the JSON data:\n```json\n{\"tool\":\"brush\",\"name\":\"python\"}\n```",
			"```python\npayload = {\"tool\": \"web_search\", \"args\": {\"query\": \"Chicago\"}}\nprint(payload)\n```",
			"The `<|channel|>analysis` token starts a reasoning channel.",
			"Use `" + search + "` as an example payload.",
		} {
			t.Run(backend+"/"+answer, func(t *testing.T) {
				model, _ := responseTestModel(t, backend, answer)
				got, err := model.GenerateFinalAnswer(context.Background(), &engine.Task{Description: "Explain the configuration or code"})
				if err != nil || got != answer {
					t.Fatalf("explanatory answer changed: got=%q err=%v want=%q", got, err, answer)
				}
			})
		}
		for _, answer := range []string{
			search, "```json\n" + search + "\n```", "I will search now:\n" + search,
			"I will search now:\n```json\n" + search + "\n```",
			"Next action: `" + search + "`", "I will search now: `" + search + "`",
			`{"function":{"name":"web_search","arguments":{"query":"Chicago"}}}`,
		} {
			t.Run(backend+"/active/"+answer, func(t *testing.T) {
				model, _ := responseTestModel(t, backend, answer)
				if got, err := model.GenerateFinalAnswer(context.Background(), &engine.Task{Description: "Current Chicago weather"}); err == nil || got != "" {
					t.Fatalf("active request accepted as final answer: got=%q err=%v", got, err)
				}
			})
		}
	}
}

func TestInvalidPlanningResponseRetriesWithoutPoisoningHistory(t *testing.T) {
	const discarded = "<|channel>thought\nPRIVATE discarded reasoning\n<channel|>" +
		"| Tool: web_search map[query:Chicago] | Result: (507 chars, see tool output above)"
	for _, backend := range []string{"llama", "ollama"} {
		t.Run(backend, func(t *testing.T) {
			model, messages := responseTestModel(t, backend, discarded, "No more tools needed.", "Chicago is 42°F.")
			task := &engine.Task{Description: "Current Chicago weather", MaxSteps: 2}
			if err := (&engine.Engine{Model: model}).RunTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			if len(task.Steps) != 2 || !strings.HasPrefix(task.Steps[0].Plan, "Blocked:") {
				t.Fatalf("invalid response completed the task instead of retrying: %+v", task.Steps)
			}
			foundBlock := false
			for _, msg := range *messages {
				if strings.Contains(msg.Content, "PRIVATE") || strings.Contains(msg.Content, "507 chars") {
					t.Fatalf("discarded text fed back to planner: %q", msg.Content)
				}
				foundBlock = foundBlock || ((msg.Role == "assistant" || msg.Role == "user") && strings.HasPrefix(msg.Content, "Blocked:"))
			}
			if !foundBlock {
				t.Fatal("retry did not include safe blocked-response feedback")
			}
			// Old or externally supplied steps must not reintroduce raw plans.
			task.Steps = append(task.Steps, &engine.Step{
				Plan: discarded, ToolCall: &engine.ToolCall{Name: "web_search"}, Result: "Chicago is 42°F.",
			}, &engine.Step{Plan: discarded})
			if _, err := model.GenerateFinalAnswer(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			for _, msg := range *messages {
				if strings.Contains(msg.Content, "PRIVATE") || strings.Contains(msg.Content, "507 chars") {
					t.Fatalf("discarded plan leaked into final context: %q", msg.Content)
				}
			}
			model, _ = responseTestModel(t, backend, discarded)
			task = &engine.Task{Description: "Current Chicago weather", MaxSteps: 1}
			if err := (&engine.Engine{Model: model}).RunTask(context.Background(), task); err == nil || task.Status != engine.TaskFailed {
				t.Fatalf("persistently invalid response completed the task: status=%s, err=%v", task.Status, err)
			}
		})
	}
}

func TestOllamaNativeToolCallHandling(t *testing.T) {
	var args api.ToolCallFunctionArguments
	args.Set("query", "current temperature Chicago IL")
	msg := api.Message{
		Content: "<think>reasoning only</think>",
		ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{
			Name: "web_search", Arguments: args,
		}}},
	}
	name, got, ok := resolveToolFromResponse(msg)
	if !ok || name != "web_search" || got["query"] != "current temperature Chicago IL" {
		t.Fatalf("native call changed: %q %v %v", name, got, ok)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"message": msg, "done": true})
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	model := &OllamaModel{Client: api.NewClient(baseURL, server.Client())}
	if answer, err := model.GenerateFinalAnswer(context.Background(), &engine.Task{Description: "Current Chicago weather"}); err == nil || answer != "" {
		t.Fatalf("native final tool request accepted: %q, %v", answer, err)
	}
}
