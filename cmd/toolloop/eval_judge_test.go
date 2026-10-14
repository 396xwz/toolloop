package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/eval"
	"github.com/ollama/ollama/api"
)

func newTaskJudgeTestModel(t *testing.T, backend, raw string, status int, wantPrompt string) (engine.ChatModel, *int) {
	t.Helper()
	calls := 0
	checkMessages := func(messages []LlamaMessage) {
		t.Helper()
		if len(messages) != 2 || messages[0].Role != "system" ||
			messages[0].Content != taskJudgeSystemPrompt || messages[1].Role != "user" {
			t.Errorf("judge messages = %+v, want dedicated system and user only", messages)
		}
		if wantPrompt != "" && len(messages) == 2 && messages[1].Content != wantPrompt {
			t.Errorf("user message = %q, want unchanged %q", messages[1].Content, wantPrompt)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apply-template":
			if backend == "llama-fallback" {
				http.NotFound(w, r)
				return
			}
			var request struct {
				Messages []LlamaMessage `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			checkMessages(request.Messages)
			_ = json.NewEncoder(w).Encode(map[string]string{"prompt": fallbackPrompt(request.Messages)})
		case "/completion":
			calls++
			var request completionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			if !strings.HasPrefix(request.Prompt, "<|im_start|>system\n"+taskJudgeSystemPrompt+"<|im_end|>\n<|im_start|>user\n") ||
				strings.Contains(request.Prompt, "AGENT PROMPT MUST NOT LEAK") ||
				strings.Contains(request.Prompt, "Otherwise respond with a short note") {
				t.Errorf("agent planning leaked into judge prompt: %q", request.Prompt)
			}
			if request.Temperature != 0 {
				t.Errorf("temperature = %v, want 0", request.Temperature)
			}
			if string(request.JSONSchema) != `{"type":"object"}` {
				t.Errorf("judge JSON schema = %s, want object constraint", request.JSONSchema)
			}
			if wantPrompt != "" && request.Prompt != fallbackPrompt([]LlamaMessage{
				{Role: "system", Content: taskJudgeSystemPrompt},
				{Role: "user", Content: wantPrompt},
			}) {
				t.Errorf("completion prompt changed the judge's user message: %q", request.Prompt)
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(completionResponse{Content: raw, Stop: true})
		case "/api/chat":
			calls++
			var request struct {
				Model    string          `json:"model"`
				Messages []LlamaMessage  `json:"messages"`
				Tools    []any           `json:"tools"`
				Stream   bool            `json:"stream"`
				Format   json.RawMessage `json:"format"`
				Options  map[string]any  `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			checkMessages(request.Messages)
			if request.Model != "judge-test-model" || len(request.Tools) != 0 || request.Stream {
				t.Errorf("unexpected plain judge request: %+v", request)
			}
			if string(request.Format) != `"json"` || request.Options["temperature"] != float64(0) {
				t.Errorf("judge must request JSON at temperature zero: %+v", request)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]string{"role": "assistant", "content": raw}, "done": true,
			})
		default:
			t.Errorf("unexpected judge endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	const agentPrompt = "AGENT PROMPT MUST NOT LEAK"
	if backend == "ollama" {
		baseURL, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		return &OllamaModel{
			Client: api.NewClient(baseURL, server.Client()), Model: "judge-test-model", SystemPrompt: agentPrompt,
		}, &calls
	}
	return &LlamaCppModel{
		Client: NewLlamaClient(server.URL), Model: "judge-test-model", SystemPrompt: agentPrompt,
	}, &calls
}

func TestTaskJudgeRunPlainCompletion(t *testing.T) {
	for _, backend := range []string{"ollama", "llama", "llama-fallback"} {
		t.Run(backend, func(t *testing.T) {
			for _, raw := range []string{
				" \n<think>raw reasoning</think>\n{\"score\": 0, \"reason\": \"wrong\"}\n ",
				`{"tool":"fs","args":{"op":"read","path":"x"}}`,
				"correct, no tool is required",
				"",
			} {
				t.Run(raw, func(t *testing.T) {
					prompt := "Grade this task. Reply only with JSON."
					model, calls := newTaskJudgeTestModel(t, backend, raw, http.StatusOK, prompt)
					got, err := taskJudgeRun(model)(context.Background(), "ignored", prompt)
					if raw == "" && backend != "ollama" {
						if err == nil {
							t.Fatal("empty llama completion should return the client's error")
						}
					} else if err != nil || got != raw {
						t.Fatalf("judge = %q, %v; want unmodified %q", got, err, raw)
					}
					if *calls != 1 {
						t.Errorf("completion calls = %d, want 1", *calls)
					}
				})
			}
		})
	}
}

func TestTaskJudgeRunTransportErrors(t *testing.T) {
	for _, backend := range []string{"ollama", "llama"} {
		t.Run(backend, func(t *testing.T) {
			model, calls := newTaskJudgeTestModel(t, backend, `{"score":10}`, http.StatusInternalServerError, "grade")
			if _, err := taskJudgeRun(model)(context.Background(), "", "grade"); err == nil {
				t.Fatal("transport error was swallowed")
			}
			if *calls != 1 {
				t.Fatalf("transport calls = %d, want 1", *calls)
			}
		})
	}
}

func TestTaskJudgeRunDoesNotFallBackToPlanning(t *testing.T) {
	model := &evalTestModel{output: `{"score":10}`}
	if _, err := taskJudgeRun(model)(context.Background(), "", "grade"); err == nil {
		t.Fatal("unsupported plain completion should fail")
	}
	if model.calls != 0 || model.finalCalls != 0 {
		t.Fatalf("judge used agent methods: %+v", model)
	}
}

func TestTaskJudgeRunVerdictContracts(t *testing.T) {
	for _, backend := range []string{"ollama", "llama", "llama-fallback"} {
		t.Run(backend, func(t *testing.T) {
			for _, rawScore := range []int{0, 2} {
				raw, _ := json.Marshal(map[string]any{"score": rawScore, "rationale": "fixed evidence"})
				model, calls := newTaskJudgeTestModel(t, backend, string(raw), http.StatusOK, "")
				judge := taskJudgeRun(model)
				rubric := &eval.Rubric{Criteria: []eval.Criterion{{
					Name: "correctness", Description: "Matches the evidence", Type: "llm", Weight: 1, Scale: 2,
				}}}
				results, total := eval.EvaluateRubric(rubric, "What is 2+2?", "4", "", "",
					func(prompt string) (string, error) { return judge(context.Background(), "", prompt) }, "")
				if results[0].Err != nil || total != float64(rawScore)/2 || results[0].Rationale != "fixed evidence" || *calls != 1 {
					t.Fatalf("rubric verdict lost: results=%+v total=%v calls=%d", results, total, *calls)
				}
			}
			for _, field := range []string{"reason", "rationale"} {
				for _, score := range []float64{0, 8.5} {
					raw, _ := json.Marshal(map[string]any{"score": score, field: "fixed evidence"})
					model, calls := newTaskJudgeTestModel(t, backend, string(raw), http.StatusOK, "")
					detail := (eval.LLMJudgeScorer{
						Criterion: "correct", MinScore: 5, Judge: taskJudgeRun(model),
					}).Score(context.Background(), eval.Input{Task: "What is 2+2?"}, eval.Result{Output: "4"})
					if detail.Err != nil || detail.Score != score/10 || detail.Passed != (score >= 5) ||
						detail.Reason != "fixed evidence" || *calls != 1 {
						t.Fatalf("scorer verdict lost: detail=%+v calls=%d", detail, *calls)
					}
				}
			}
		})
	}
}

func TestTaskJudgeRunInvalidVerdicts(t *testing.T) {
	for _, backend := range []string{"ollama", "llama"} {
		for _, raw := range []string{"invalid verdict", `{"score":null}`, `{"score":"2"}`} {
			t.Run(backend+"/"+raw, func(t *testing.T) {
				model, calls := newTaskJudgeTestModel(t, backend, raw, http.StatusOK, "")
				detail := (eval.LLMJudgeScorer{
					Criterion: "correct", MinScore: 5, Judge: taskJudgeRun(model),
				}).Score(context.Background(), eval.Input{Task: "What is 2+2?"}, eval.Result{Output: "4"})
				if detail.Err == nil || detail.Passed || !strings.Contains(detail.Reason, "judge_error: invalid judge response after 2 attempts") || *calls != 2 {
					t.Fatalf("invalid scorer response not rejected: %+v calls=%d", detail, *calls)
				}
				*calls = 0
				judge := taskJudgeRun(model)
				results, _ := eval.EvaluateRubric(&eval.Rubric{Criteria: []eval.Criterion{{
					Name: "correct", Description: "Equals four", Type: "llm", Weight: 1, Scale: 2,
				}}}, "What is 2+2?", "4", "", "",
					func(prompt string) (string, error) { return judge(context.Background(), "", prompt) }, "")
				if results[0].Err == nil || !strings.Contains(results[0].Rationale, "after 2 attempts") || *calls != 2 {
					t.Fatalf("invalid rubric response not rejected: %+v calls=%d", results, *calls)
				}
			})
		}
	}
}

func TestLlamaOrdinaryChatRemainsUnconstrained(t *testing.T) {
	for _, budget := range []bool{false, true} {
		t.Run(map[bool]string{false: "Chat", true: "ChatBudget"}[budget], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/apply-template" {
					http.NotFound(w, r)
					return
				}
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				for _, key := range []string{"json_schema", "grammar", "response_format"} {
					if _, ok := request[key]; ok {
						t.Errorf("ordinary completion contains %s", key)
					}
				}
				_ = json.NewEncoder(w).Encode(completionResponse{Content: "ordinary prose", Stop: true})
			}))
			defer server.Close()
			client := NewLlamaClient(server.URL)
			messages := []LlamaMessage{{Role: "user", Content: "Say hello"}}
			var err error
			if budget {
				err = client.ChatBudget(context.Background(), messages, 0.7, 100, nil)
			} else {
				err = client.Chat(context.Background(), messages, 0.7, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Opt in only against an already running local server; no external task data.
func TestTaskJudgeLocalLlama(t *testing.T) {
	server := os.Getenv("TOOLLOOP_TEST_JUDGE_LLAMA_URL")
	if server == "" {
		t.Skip("set TOOLLOOP_TEST_JUDGE_LLAMA_URL to an existing local llama.cpp server")
	}
	model := &LlamaCppModel{Client: NewLlamaClient(server), Model: "local-judge"}
	judge := taskJudgeRun(model)
	for _, prompt := range []string{
		eval.BuildJudgePrompt("What is 2+2?", "4", "", "", eval.Criterion{
			Name: "correctness", Description: "The answer must equal four", Scale: 2,
		}),
		`Task: What is 2+2?
Output: 4
Criterion: The answer must equal four.
Score the output from 0 to 10. Reply with only a JSON object:
{"score": <number>, "reason": "<one sentence>"}`,
	} {
		raw, err := judge(context.Background(), "", prompt)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid([]byte(raw)) {
			t.Fatalf("backend did not constrain JSON: %q", raw)
		}
		t.Logf("local constrained judgment: %s", raw)
		if strings.Contains(prompt, "rationale") {
			score, rationale, err := eval.ParseJudgeResponse(raw)
			if err != nil || score != 2 || rationale == "" {
				t.Fatalf("rubric verdict = %d, %q, %v", score, rationale, err)
			}
		} else {
			var verdict struct {
				Score  float64 `json:"score"`
				Reason string  `json:"reason"`
			}
			if err := json.Unmarshal([]byte(raw), &verdict); err != nil || verdict.Score != 10 || verdict.Reason == "" {
				t.Fatalf("scorer verdict = %+v, %v", verdict, err)
			}
		}
	}
}
