package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/ollama/ollama/api"
)

func TestCurrentTimeContext(t *testing.T) {
	tests := []struct {
		now  time.Time
		want string
	}{
		{
			time.Date(2000, 12, 31, 23, 59, 59, 0, time.FixedZone("Test/West", -8*60*60)),
			"2000-12-31T23:59:59-08:00 (Sunday; timezone: Test/West, Test/West)",
		},
		{
			time.Date(2001, 1, 1, 0, 0, 0, 0, time.FixedZone("Test/East", 5*60*60+45*60)),
			"2001-01-01T00:00:00+05:45 (Monday; timezone: Test/East, Test/East)",
		},
	}
	for _, test := range tests {
		got := currentTimeContext(test.now)
		if !strings.Contains(got, test.want) {
			t.Errorf("currentTimeContext(%v) = %q, want %q", test.now, got, test.want)
		}
		if !strings.Contains(got, "rather than dates assumed from training data") {
			t.Errorf("missing runtime-clock precedence instruction: %q", got)
		}
	}
	if currentTimeContext(tests[0].now) == currentTimeContext(tests[0].now.Add(time.Second)) {
		t.Fatal("runtime context did not refresh across local midnight")
	}
}

func TestModelRequestsIncludeFreshRuntimeContext(t *testing.T) {
	for _, backend := range []string{"llama-native", "llama-template", "llama-chatml", "ollama"} {
		for _, custom := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/custom=%t", backend, custom), func(t *testing.T) {
				const taskText = "Explain this unique user request."
				system := ""
				wantSystem := defaultSystemPrompt
				if custom {
					system = "Custom system constraint: stay concise."
					wantSystem = system
				}
				var before time.Time
				checked := 0
				check := func(content string) {
					t.Helper()
					const prefix = "Runtime clock (host local time): "
					if strings.Count(content, prefix) != 1 {
						t.Errorf("want exactly one runtime context: %q", content)
						return
					}
					clock := strings.SplitN(strings.SplitN(content, prefix, 2)[1], " ", 2)[0]
					now, err := time.Parse(time.RFC3339, clock)
					if err != nil {
						t.Errorf("invalid runtime timestamp %q: %v", clock, err)
						return
					}
					if now.Before(before.Truncate(time.Second)) || now.After(time.Now()) {
						t.Errorf("runtime timestamp %v is outside this request's clock window", now)
					}
					if !strings.Contains(content, wantSystem) {
						t.Errorf("missing original system prompt: %q", content)
					}
					checked++
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v1/chat/completions", "/apply-template", "/api/chat":
						if (r.URL.Path == "/v1/chat/completions" && backend != "llama-native") ||
							(r.URL.Path == "/apply-template" && backend == "llama-chatml") {
							http.NotFound(w, r)
							return
						}
						var request struct {
							Messages []LlamaMessage `json:"messages"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
							http.Error(w, "bad request", http.StatusBadRequest)
							return
						}
						if len(request.Messages) != 2 || request.Messages[0].Role != "system" ||
							request.Messages[1].Role != "user" || !strings.Contains(request.Messages[1].Content, taskText) {
							t.Errorf("original system/user messages not preserved: %+v", request.Messages)
							http.Error(w, "bad messages", http.StatusBadRequest)
							return
						}
						check(request.Messages[0].Content)
						if strings.Contains(request.Messages[1].Content, "Runtime clock (host local time):") {
							t.Error("runtime context leaked into user message")
						}
						switch r.URL.Path {
						case "/apply-template":
							_, _ = fmt.Fprint(w, `{"prompt":"rendered prompt"}`)
						case "/api/chat":
							_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"No tool is required."},"done":true}`)
						default:
							_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"No tool is required."},"finish_reason":"stop"}]}`)
						}
					case "/completion":
						var request completionRequest
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						if backend == "llama-chatml" {
							check(request.Prompt)
							if !strings.Contains(request.Prompt, taskText) {
								t.Error("fallback prompt lost user request")
							}
						}
						_, _ = fmt.Fprint(w, `{"content":"No tool is required.","stop":true}`)
					default:
						t.Errorf("unexpected request: %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()

				var model engine.ChatModel
				if backend == "ollama" {
					baseURL, err := url.Parse(server.URL)
					if err != nil {
						t.Fatal(err)
					}
					model = &OllamaModel{Client: api.NewClient(baseURL, server.Client()), SystemPrompt: system}
				} else {
					client := NewLlamaClient(server.URL)
					client.HTTP = server.Client()
					model = &LlamaCppModel{Client: client, SystemPrompt: system}
				}
				task := &engine.Task{Description: taskText}
				for turn := 0; turn < 2; turn++ {
					before = time.Now()
					if _, err := model.PlanNextStep(context.Background(), task); err != nil {
						t.Fatal(err)
					}
					before = time.Now()
					if _, err := model.GenerateFinalAnswer(context.Background(), task); err != nil {
						t.Fatal(err)
					}
					if model.SystemPromptValue() != system || task.Description != taskText {
						t.Fatal("request mutated stored system prompt or user task")
					}
				}
				if checked != 4 {
					t.Errorf("checked %d requests, want planning and final for both turns", checked)
				}
			})
		}
	}
}
