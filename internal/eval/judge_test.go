package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJudgeRetryAndErrorDistinction(t *testing.T) {
	transportErr := errors.New("judge unavailable")
	for _, path := range []string{"scorer", "rubric"} {
		t.Run(path, func(t *testing.T) {
			for _, tt := range []struct {
				name      string
				replies   []string
				callErr   error
				wantCalls int
				wantErr   bool
				wantZero  bool
			}{
				{"valid zero", []string{`{"score":0,"reason":"wrong","rationale":"wrong"}`}, nil, 1, false, true},
				{"valid fractional scorer", []string{`{"score":0.5,"reason":"partial"}`}, nil, 1, false, false},
				{"malformed then valid", []string{"correct", `{"score":1,"reason":"good","rationale":"good"}`}, nil, 2, false, false},
				{"missing score then valid", []string{`{"reason":"bad"}`, `{"score":1}`}, nil, 2, false, false},
				{"null score then valid", []string{`{"score":null}`, `{"score":1}`}, nil, 2, false, false},
				{"wrong type then valid", []string{`{"score":"1"}`, `{"score":1}`}, nil, 2, false, false},
				{"negative score then valid", []string{`{"score":-1}`, `{"score":1}`}, nil, 2, false, false},
				{"retry exhaustion", []string{"correct", `{"score":`}, nil, 2, true, true},
				{"invalid score exhaustion", []string{`{}`, `{"score":null}`}, nil, 2, true, true},
				{"transport error", nil, transportErr, 1, true, true},
				{"transport on retry", []string{"correct"}, transportErr, 2, true, true},
			} {
				if path == "rubric" && tt.name == "valid fractional scorer" {
					continue
				}
				t.Run(tt.name, func(t *testing.T) {
					calls := 0
					var firstPrompt string
					judge := func(prompt string) (string, error) {
						calls++
						if calls == 1 {
							firstPrompt = prompt
						} else if prompt != firstPrompt {
							t.Error("retry changed the grading prompt")
						}
						if tt.callErr != nil && calls > len(tt.replies) {
							return "", tt.callErr
						}
						if calls > len(tt.replies) {
							t.Fatalf("unexpected extra judge call %d", calls)
						}
						return tt.replies[calls-1], nil
					}
					var score float64
					var reason string
					var err error
					if path == "scorer" {
						detail := (LLMJudgeScorer{
							Criterion: "correct", MinScore: 0.05,
							Judge: func(_ context.Context, _, prompt string) (string, error) { return judge(prompt) },
						}).Score(context.Background(), Input{Task: "task"}, Result{Output: "answer"})
						score, reason, err = detail.Score, detail.Reason, detail.Err
						if detail.Passed != (!tt.wantErr && !tt.wantZero) {
							t.Errorf("passed = %v, detail = %+v", detail.Passed, detail)
						}
					} else {
						r := &Rubric{Criteria: []Criterion{
							{Name: "correct", Description: "correct", Type: "llm", Weight: 1},
						}}
						results, total := EvaluateRubric(r, "task", "answer", "", "", judge, "judge-model")
						score, reason, err = total, results[0].Rationale, results[0].Err
					}
					if calls != tt.wantCalls || (err != nil) != tt.wantErr {
						t.Fatalf("calls = %d, error = %v; want %d, error=%v", calls, err, tt.wantCalls, tt.wantErr)
					}
					if (score == 0) != tt.wantZero {
						t.Errorf("score = %v, want zero=%v", score, tt.wantZero)
					}
					if strings.HasPrefix(reason, "judge_error:") != tt.wantErr {
						t.Errorf("reason = %q, want judge_error=%v", reason, tt.wantErr)
					}
					if tt.callErr != nil && !errors.Is(err, tt.callErr) {
						t.Errorf("transport error not preserved: %v", err)
					}
					if tt.wantErr && tt.callErr == nil && !strings.Contains(reason, "after 2 attempts") {
						t.Errorf("missing exhaustion diagnostic: %q", reason)
					}
				})
			}
		})
	}
}

func TestJudgeErrorsPropagateToReports(t *testing.T) {
	transportErr := errors.New("judge offline")
	for _, mode := range []string{"scorer transport", "scorer parse", "rubric transport", "rubric parse", "zero", "valid"} {
		t.Run(mode, func(t *testing.T) {
			suite := Suite{Name: "judge", Component: "task", Cases: []Case{{
				Name:  "answer",
				Input: Input{Task: "answer"},
				Scorers: []Scorer{LLMJudgeScorer{
					Criterion: "correct", MinScore: 5,
					Judge: func(_ context.Context, _, prompt string) (string, error) {
						rubricCall := strings.HasPrefix(prompt, "You are grading")
						shouldFail := (strings.HasPrefix(mode, "rubric") && rubricCall) ||
							(strings.HasPrefix(mode, "scorer") && !rubricCall)
						if shouldFail {
							if strings.HasSuffix(mode, "transport") {
								return "", transportErr
							}
							return "not a verdict", nil
						}
						if mode == "zero" {
							return `{"score":0,"reason":"wrong","rationale":"wrong"}`, nil
						}
						return `{"score":10,"reason":"correct","rationale":"correct"}`, nil
					},
				}},
				// Even a threshold of zero must not hide infrastructure failures.
				Rubric: &Rubric{Threshold: 0, Criteria: []Criterion{
					{Name: "correct", Description: "correct", Type: "llm", Weight: 1},
				}},
			}}}
			outputDir := t.TempDir()
			metrics, err := RunSuite(context.Background(), suite, RunOptions{
				OutputDir: outputDir,
				Runner: ComponentRunnerFunc(func(context.Context, string, Input, string) (ComponentResult, error) {
					return ComponentResult{Output: "A prose answer."}, nil
				}),
			})
			wantError := mode != "zero" && mode != "valid"
			if (err != nil) != wantError {
				t.Fatalf("RunSuite error = %v; want error=%v", err, wantError)
			}
			if strings.HasSuffix(mode, "transport") && !errors.Is(err, transportErr) {
				t.Errorf("RunSuite lost transport error: %v", err)
			}
			wantPassed := 0
			if mode == "valid" {
				wantPassed = 1
			}
			if metrics.Passed != wantPassed {
				t.Errorf("metrics = %+v", metrics)
			}
			data, readErr := os.ReadFile(filepath.Join(outputDir, "judge.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			var report report
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			result := report.Cases[0]
			if (result.Error != "") != wantError || strings.Contains(string(data), "judge_error:") != wantError {
				t.Errorf("JSON error distinction missing: %s", data)
			}
			if strings.HasPrefix(mode, "rubric") && result.RubricPassed {
				t.Error("rubric infrastructure failure passed")
			}
			markdown, readErr := os.ReadFile(filepath.Join(outputDir, "judge.md"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(markdown), "judge_error:") != wantError {
				t.Errorf("Markdown error distinction missing: %s", markdown)
			}
		})
	}
}
