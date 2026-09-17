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
)

func TestLlamaGenerateFinalAnswerIncludesSystemPromptAndPlatformGuidance(t *testing.T) {
	var messages []LlamaMessage

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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

const seattleTask = "Create a project, that checks the weather every 5min in Seattle WA in python."

// runPlanNextStep runs PlanNextStep against a fake llama.cpp server that
// records the apply-template messages and returns fixedContent as the
// completion.
func runPlanNextStep(t *testing.T, task *engine.Task, fixedContent string) (*engine.Step, []LlamaMessage) {
	t.Helper()
	var messages []LlamaMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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

// AC3: blocked steps (Plan "Blocked: ...", no ToolCall) appear as assistant
// messages in the history of subsequent PlanNextStep calls, in step order,
// alongside executed tool steps.
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
	if len(messages) != 4 {
		t.Fatalf("message count = %d, want 4: %#v", len(messages), messages)
	}
	if messages[0].Role != "system" {
		t.Errorf("messages[0].Role = %q, want system", messages[0].Role)
	}
	if messages[1].Role != "assistant" || messages[1].Content != "Tool fs returned: no such file" {
		t.Errorf("messages[1] = %#v, want assistant tool result", messages[1])
	}
	if messages[2].Role != "assistant" || messages[2].Content != "Blocked: fs op not allowed by task text" {
		t.Errorf("messages[2] = %#v, want assistant block reason", messages[2])
	}
	if messages[3].Role != "user" {
		t.Errorf("messages[3].Role = %q, want user", messages[3].Role)
	}
}
