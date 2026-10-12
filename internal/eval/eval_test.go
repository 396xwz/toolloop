package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAggregate(t *testing.T) {
	results := []Result{
		{
			Passed:       true,
			Latency:      time.Millisecond,
			Cost:         0.25,
			PromptTokens: 10,
			OutputTokens: 2,
			TotalTokens:  12,
			ToolCalls: []ToolCallRecord{
				{Tool: "fs", Repeated: true},
				{Tool: "fs", Repeated: true},
			},
			ScoreDetails: []ScoreDetail{{ScorerName: "exact", Passed: true, Score: 1}},
		},
		{
			Latency:      4 * time.Millisecond,
			Cost:         0.75,
			PromptTokens: 20,
			OutputTokens: 3,
			TotalTokens:  23,
			ToolCalls: []ToolCallRecord{
				{Tool: "shell"},
			},
			ScoreDetails: []ScoreDetail{{ScorerName: "exact", Score: 0.5}},
		},
	}

	got := Aggregate("suite", "agent:test", results)
	if got.TotalCases != 2 || got.Passed != 1 || got.Failed != 1 || got.PassRate != 50 {
		t.Fatalf("unexpected counts: %+v", got)
	}
	if got.AvgLatency != 2500*time.Microsecond || got.P50Latency != time.Millisecond ||
		got.P95Latency != 4*time.Millisecond || got.P99Latency != 4*time.Millisecond {
		t.Fatalf("unexpected latencies: %+v", got)
	}
	if got.TotalCost != 1 || got.AvgCost != 0.5 || got.ToolCalls != 3 ||
		got.ToolErrors != 0 || got.AvgToolCalls != 1.5 || got.RepeatRate != 50 {
		t.Fatalf("unexpected aggregates: %+v", got)
	}
	if got.PromptTokens != 30 || got.OutputTokens != 5 || got.TotalTokens != 35 {
		t.Fatalf("unexpected token aggregates: %+v", got)
	}
	if stat := got.ScorerStats["exact"]; stat.PassRate != 50 || stat.AvgScore != 0.75 {
		t.Fatalf("unexpected scorer stats: %+v", stat)
	}

	empty := Aggregate("empty", "model:test", nil)
	if empty.PassRate != 0 || empty.AvgCost != 0 || empty.AvgLatency != 0 {
		t.Fatalf("empty aggregate contains non-zero averages: %+v", empty)
	}
}

func TestScorers(t *testing.T) {
	result := Result{
		Output:    " hello world ",
		Artifacts: map[string]string{"diff": "package main"},
		ToolCalls: []ToolCallRecord{
			{Tool: "fs", Args: map[string]any{"op": "read", "path": "plan.md"}},
			{Tool: "fs", Repeated: true},
		},
		Latency: 2 * time.Second,
	}
	tests := []struct {
		name   string
		scorer Scorer
		pass   bool
	}{
		{"exact", ExactScorer{Want: "hello world"}, true},
		{"contains", ContainsScorer{Substrings: []string{"hello", "world"}}, true},
		{"regex", RegexScorer{Pattern: `hello\s+world`}, true},
		{"invalid regex", RegexScorer{Pattern: `[`}, false},
		{"artifact", ArtifactScorer{Key: "diff", Pattern: `package`}, true},
		{"calls", ToolCallCountScorer{Tool: "fs", Min: 2, Max: 2}, true},
		{"structural tool call", ToolCallScorer{
			Tool: "fs",
			Args: map[string]string{"op": "read", "path": "plan.md"},
		}, true},
		{"wrong tool arguments", ToolCallScorer{
			Tool: "fs",
			Args: map[string]string{"op": "write", "path": "plan.md"},
		}, false},
		{"tool error", ToolErrorScorer{Tool: "fs", Contains: "denied"}, false},
		{"repeat", NoRepeatScorer{}, false},
		{"latency", LatencyScorer{Max: time.Second}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if detail := test.scorer.Score(context.Background(), Input{}, result); detail.Passed != test.pass {
				t.Fatalf("got %+v, want pass=%v", detail, test.pass)
			}
		})
	}
}

func TestToolErrorScorer(t *testing.T) {
	runErr := errors.New("open missing.txt: file not found")
	result := Result{ToolCalls: []ToolCallRecord{{Tool: "fs", Err: runErr}}}
	if detail := (ToolErrorScorer{Tool: "fs", Contains: "file not found"}).Score(
		context.Background(), Input{}, result,
	); !detail.Passed || detail.Score != 1 {
		t.Fatalf("expected matching tool error to pass: %+v", detail)
	}
	if detail := (ToolErrorScorer{Tool: "fs", Contains: "permission denied"}).Score(
		context.Background(), Input{}, result,
	); detail.Passed {
		t.Fatalf("unexpected non-matching tool error pass: %+v", detail)
	}
}

func TestLLMJudgeScorer(t *testing.T) {
	scorer := LLMJudgeScorer{
		Criterion:  "clear",
		MinScore:   7,
		JudgeModel: "judge",
		Judge: func(_ context.Context, model, prompt string) (string, error) {
			if model != "judge" || !strings.Contains(prompt, "answer") {
				t.Fatalf("unexpected judge input: model=%q prompt=%q", model, prompt)
			}
			return "```json\n{\"score\": 8, \"reason\": \"clear\"}\n```", nil
		},
	}
	detail := scorer.Score(context.Background(), Input{Task: "task"}, Result{Output: "answer"})
	if !detail.Passed || detail.Score != 0.8 || detail.Reason != "clear" {
		t.Fatalf("unexpected detail: %+v", detail)
	}
}

func TestRunSuiteWritesReportsAndFiltersTags(t *testing.T) {
	outputDir := t.TempDir()
	var setupCalled, teardownCalled bool
	suite := Suite{
		Name:      "test suite",
		Component: "agent:test",
		Setup: func(dir string) error {
			setupCalled = true
			return os.WriteFile(filepath.Join(dir, "setup.txt"), []byte("ready"), 0o644)
		},
		Teardown: func(string) error {
			teardownCalled = true
			return nil
		},
		Cases: []Case{
			{
				Name: "selected",
				Tags: []string{"fast", "agent"},
				Input: Input{
					Task:  "say hello",
					Files: map[string]string{"nested/input.txt": "fixture"},
				},
				Scorers: []Scorer{ExactScorer{Want: "hello"}},
			},
			{Name: "skipped", Tags: []string{"slow"}},
		},
	}
	runner := ComponentRunnerFunc(func(_ context.Context, component string, input Input, dir string) (ComponentResult, error) {
		if component != "agent:test" || input.Task != "say hello" {
			t.Fatalf("unexpected runner input: %q %+v", component, input)
		}
		content, err := os.ReadFile(filepath.Join(dir, "nested", "input.txt"))
		if err != nil || string(content) != "fixture" {
			t.Fatalf("fixture: content=%q err=%v", content, err)
		}
		return ComponentResult{
			Output:    "hello",
			Artifacts: map[string]string{"answer": "hello"},
			ToolCalls: []ToolCallRecord{{Tool: "fs", Duration: time.Millisecond}},
			Tokens:    TokenUsage{Prompt: 2, Output: 1, Total: 3},
			Cost:      0.01,
		}, nil
	})

	metrics, err := RunSuite(context.Background(), suite, RunOptions{
		Tags:      []string{"fast", "agent"},
		Parallel:  2,
		OutputDir: outputDir,
		Runner:    runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !setupCalled || !teardownCalled {
		t.Fatalf("setup=%v teardown=%v", setupCalled, teardownCalled)
	}
	if metrics.TotalCases != 1 || metrics.Passed != 1 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}

	jsonPath := filepath.Join(outputDir, "test-suite.json")
	markdownPath := filepath.Join(outputDir, "test-suite.md")
	jsonBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("invalid JSON report: %v", err)
	}
	if decoded["suite"] != "test suite" ||
		!strings.Contains(string(jsonBytes), `"prompt_tokens": 2`) ||
		!strings.Contains(string(jsonBytes), `"output_tokens": 1`) ||
		!strings.Contains(string(jsonBytes), `"total_tokens": 3`) {
		t.Fatalf("unexpected JSON report:\n%s", jsonBytes)
	}
	markdown, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(markdown), "# Evaluation: test suite") ||
		!strings.Contains(string(markdown), "Tokens (prompt / output / total): 2 / 1 / 3") ||
		!strings.Contains(string(markdown), "| selected | pass |") {
		t.Fatalf("unexpected Markdown report:\n%s", markdown)
	}
}

func TestRunSuitePropagatesErrors(t *testing.T) {
	t.Run("execution and teardown", func(t *testing.T) {
		runErr := errors.New("run failed")
		teardownErr := errors.New("teardown failed")
		suite := Suite{
			Name:      "errors",
			Component: "agent:test",
			Cases:     []Case{{Name: "case"}},
			Teardown:  func(string) error { return teardownErr },
		}
		metrics, err := RunSuite(context.Background(), suite, RunOptions{
			Runner: ComponentRunnerFunc(func(context.Context, string, Input, string) (ComponentResult, error) {
				return ComponentResult{}, runErr
			}),
		})
		if metrics.Failed != 1 || !errors.Is(err, runErr) || !errors.Is(err, teardownErr) {
			t.Fatalf("metrics=%+v err=%v", metrics, err)
		}
	})

	t.Run("fixture", func(t *testing.T) {
		suite := Suite{
			Name:      "fixture",
			Component: "agent:test",
			Cases: []Case{{
				Name:  "case",
				Input: Input{Files: map[string]string{"../escape": "bad"}},
			}},
		}
		metrics, err := RunSuite(context.Background(), suite, RunOptions{
			Runner: ComponentRunnerFunc(func(context.Context, string, Input, string) (ComponentResult, error) {
				t.Fatal("runner should not be called")
				return ComponentResult{}, nil
			}),
		})
		if metrics.Failed != 1 || err == nil || !strings.Contains(err.Error(), "invalid fixture path") {
			t.Fatalf("metrics=%+v err=%v", metrics, err)
		}
	})

	t.Run("report", func(t *testing.T) {
		parent := t.TempDir()
		outputPath := filepath.Join(parent, "not-a-directory")
		if err := os.WriteFile(outputPath, []byte("file"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := RunSuite(context.Background(), Suite{Name: "empty"}, RunOptions{OutputDir: outputPath})
		if err == nil || !strings.Contains(err.Error(), "write report") {
			t.Fatalf("expected report error, got %v", err)
		}
	})

	t.Run("scorer error reaches case report and caller", func(t *testing.T) {
		scorerErr := errors.New("stability run 2: backend unavailable")
		outputDir := t.TempDir()
		suite := Suite{
			Name:      "scorer-error",
			Component: "model:test",
			Cases: []Case{{
				Name: "case",
				Scorers: []Scorer{scorerFunc(func(context.Context, Input, Result) ScoreDetail {
					return ScoreDetail{ScorerName: "stability", Reason: scorerErr.Error(), Err: scorerErr}
				})},
			}},
		}
		metrics, err := RunSuite(context.Background(), suite, RunOptions{
			OutputDir: outputDir,
			Runner: ComponentRunnerFunc(func(context.Context, string, Input, string) (ComponentResult, error) {
				return ComponentResult{}, nil
			}),
		})
		if metrics.Failed != 1 || !errors.Is(err, scorerErr) {
			t.Fatalf("metrics=%+v err=%v", metrics, err)
		}
		report, readErr := os.ReadFile(filepath.Join(outputDir, "scorer-error.json"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.Contains(string(report), scorerErr.Error()) {
			t.Fatalf("report did not preserve scorer error:\n%s", report)
		}
	})
}

func TestRunSuiteEmptySelectionNeedsNoRunner(t *testing.T) {
	metrics, err := RunSuite(context.Background(), Suite{
		Name:  "filtered",
		Cases: []Case{{Name: "slow", Tags: []string{"slow"}}},
	}, RunOptions{Tags: []string{"fast"}})
	if err != nil || metrics.TotalCases != 0 || metrics.PassRate != 0 {
		t.Fatalf("metrics=%+v err=%v", metrics, err)
	}
}

type scorerFunc func(context.Context, Input, Result) ScoreDetail

func (f scorerFunc) Name() string {
	return "test"
}

func (f scorerFunc) Score(ctx context.Context, input Input, result Result) ScoreDetail {
	return f(ctx, input, result)
}
