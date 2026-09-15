package main

import (
	"context"
	"encoding/json"
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
