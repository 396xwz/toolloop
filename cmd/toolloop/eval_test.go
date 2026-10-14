package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/eval"
	"github.com/396xwz/toolloop/internal/eval/suites"
	"github.com/396xwz/toolloop/internal/tools"
)

type evalTestModel struct {
	output     string
	toolCall   *engine.ToolCall
	plan       func(int) (*engine.Step, error)
	calls      int
	finalCalls int
	planTask   string
	finalTask  string
}

func (m *evalTestModel) PlanNextStep(_ context.Context, task *engine.Task) (*engine.Step, error) {
	m.calls++
	m.planTask = task.Description
	if m.plan != nil {
		return m.plan(m.calls)
	}
	return &engine.Step{Plan: m.output, ToolCall: m.toolCall}, nil
}

func (m *evalTestModel) GenerateFinalAnswer(_ context.Context, task *engine.Task) (string, error) {
	m.finalCalls++
	m.finalTask = task.Description
	return m.output, nil
}

func (m *evalTestModel) ModelName() string         { return "test-model" }
func (m *evalTestModel) SetModel(string)           {}
func (m *evalTestModel) SystemPromptValue() string { return "" }
func (m *evalTestModel) SetSystemPrompt(string)    {}

func TestValidateEvaluationMode(t *testing.T) {
	t.Run("defaults allow normal mode", func(t *testing.T) {
		if err := validateEvaluationMode(evaluationModeOptions{}); err != nil {
			t.Fatalf("validateEvaluationMode() error = %v", err)
		}
	})

	t.Run("eval does not require task", func(t *testing.T) {
		if err := validateEvaluationMode(evaluationModeOptions{Suite: "suite"}); err != nil {
			t.Fatalf("validateEvaluationMode() error = %v", err)
		}
	})

	t.Run("task eval requires one non-empty task", func(t *testing.T) {
		for _, tasks := range [][]string{nil, {""}, {"   "}, {"one", "two"}} {
			err := validateEvaluationMode(evaluationModeOptions{Suite: suites.TaskSuite, Tasks: tasks})
			if err == nil || err.Error() != "-eval task requires exactly one non-empty -task" {
				t.Fatalf("tasks=%q error=%v", tasks, err)
			}
		}
		if err := validateEvaluationMode(evaluationModeOptions{
			Suite: suites.TaskSuite,
			Tasks: []string{"do the work"},
		}); err != nil {
			t.Fatalf("valid task evaluation rejected: %v", err)
		}
	})

	t.Run("output requires eval", func(t *testing.T) {
		err := validateEvaluationMode(evaluationModeOptions{OutputDir: "reports"})
		if err == nil || err.Error() != "-eval-output requires -eval" {
			t.Fatalf("validateEvaluationMode() error = %v", err)
		}
	})

	tests := []struct {
		name string
		opts evaluationModeOptions
		want string
	}{
		{"task", evaluationModeOptions{Tasks: []string{"work"}}, "-task"},
		{"index", evaluationModeOptions{IndexPath: "docs"}, "-index"},
		{"repl", evaluationModeOptions{REPL: true}, "-repl"},
		{"agent", evaluationModeOptions{AgentName: "builder"}, "-agent"},
		{"topology", evaluationModeOptions{TopologyPath: "graph.yaml"}, "-topology"},
		{"topology report", evaluationModeOptions{TopologyReport: "graph.dot"}, "-topology-report"},
		{"direct action", evaluationModeOptions{DirectAction: true}, "direct tool flags"},
		{"output file", evaluationModeOptions{OutputFile: "answer.txt"}, "-output"},
	}
	for _, tt := range tests {
		t.Run("eval rejects "+tt.name, func(t *testing.T) {
			tt.opts.Suite = "suite"
			err := validateEvaluationMode(tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateEvaluationMode() error = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestTaskEvaluationRunsOnceAndReportsExecutionMetrics(t *testing.T) {
	outputDir := newEvalTestDir(t)
	calls := 0
	var gotPrompt string
	toolErr := errors.New("permission denied")
	started := time.Unix(100, 0)
	answer := "first line\nsecond line"
	execute := func(_ context.Context, prompt string) normalTaskExecution {
		calls++
		gotPrompt = prompt
		return normalTaskExecution{
			Output: answer,
			Task: &engine.Task{
				Status: engine.TaskCompleted,
				Steps: []*engine.Step{{
					ToolCall: &engine.ToolCall{
						Name: "fs",
						Args: map[string]string{"op": "read", "path": "input.txt"},
					},
					StartedAt:  started,
					FinishedAt: started.Add(25 * time.Millisecond),
					Err:        toolErr,
				}},
			},
		}
	}

	var stdout bytes.Buffer
	err := runTaskEvaluation(
		context.Background(),
		"inspect input.txt",
		nil,
		outputDir,
		evaluationPrinter(&stdout),
		execute,
	)
	if err == nil || !strings.Contains(err.Error(), "task evaluation failed") {
		t.Fatalf("runTaskEvaluation() error = %v, want task evaluation failed", err)
	}
	if calls != 1 || gotPrompt != "inspect input.txt" {
		t.Fatalf("calls=%d prompt=%q; want one propagated execution", calls, gotPrompt)
	}
	answerOutput := "Task completed.\nFinal Answer:\n" + answer + "\n"
	summaryIndex := strings.Index(stdout.String(), "Task evaluation failed:")
	if !strings.HasPrefix(stdout.String(), answerOutput) || summaryIndex < len(answerOutput) {
		t.Fatalf("answer was not printed before summary:\n%q", stdout.String())
	}
	if strings.Count(stdout.String(), answer) != 1 {
		t.Fatalf("answer printed more than once:\n%q", stdout.String())
	}
	for _, want := range []string{"Task evaluation failed:", "1 tool calls", "1 tool errors"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
	jsonReport, err := os.ReadFile(filepath.Join(outputDir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"suite": "task"`,
		`"passed": false`,
		`"tool_calls": 1`,
		`"tool_errors": 1`,
		`"output": "first line\nsecond line"`,
		`"duration_ms": 25`,
		`"error": "permission denied"`,
	} {
		if !strings.Contains(string(jsonReport), want) {
			t.Fatalf("JSON report does not contain %q:\n%s", want, jsonReport)
		}
	}
	markdown, err := os.ReadFile(filepath.Join(outputDir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(markdown), "Tool calls: 1 (1 errors)") ||
		!strings.Contains(string(markdown), "| task | fail |") ||
		!strings.Contains(string(markdown), "    first line\n    second line") {
		t.Fatalf("unexpected Markdown report:\n%s", markdown)
	}
}

func TestTaskEvaluationDoesNotPrintEmptyAnswer(t *testing.T) {
	var stdout bytes.Buffer
	err := runTaskEvaluation(
		context.Background(),
		"empty answer",
		nil,
		newEvalTestDir(t),
		evaluationPrinter(&stdout),
		func(context.Context, string) normalTaskExecution {
			return normalTaskExecution{}
		},
	)
	if err == nil || !strings.Contains(err.Error(), "task evaluation failed") {
		t.Fatalf("runTaskEvaluation() error = %v, want task evaluation failed", err)
	}
	if strings.Contains(stdout.String(), "Final Answer:") ||
		!strings.HasPrefix(stdout.String(), "Task evaluation failed:") {
		t.Fatalf("unexpected output for empty answer: %q", stdout.String())
	}
}

func TestTaskEvaluationExecutionErrorFailsAndStillReports(t *testing.T) {
	outputDir := newEvalTestDir(t)
	runErr := errors.New("model unavailable")
	answer := "partial answer"
	calls := 0
	var stdout bytes.Buffer
	err := runTaskEvaluation(
		context.Background(),
		"fail once",
		nil,
		outputDir,
		evaluationPrinter(&stdout),
		func(context.Context, string) normalTaskExecution {
			calls++
			return normalTaskExecution{Output: answer, Err: runErr}
		},
	)
	if !errors.Is(err, runErr) || calls != 1 {
		t.Fatalf("error=%v calls=%d; want wrapped error and one execution", err, calls)
	}
	if !strings.Contains(stdout.String(), "Task evaluation failed:") ||
		strings.Contains(stdout.String(), answer) ||
		strings.Contains(stdout.String(), "Final Answer:") {
		t.Fatalf("unexpected summary: %q", stdout.String())
	}
	report, readErr := os.ReadFile(filepath.Join(outputDir, "task.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(report), `"passed": false`) ||
		!strings.Contains(string(report), `"output": "partial answer"`) ||
		!strings.Contains(string(report), `"error": "model unavailable"`) {
		t.Fatalf("execution failure missing from report:\n%s", report)
	}
	markdown, readErr := os.ReadFile(filepath.Join(outputDir, "task.md"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(markdown), "    partial answer") {
		t.Fatalf("execution output missing from Markdown report:\n%s", markdown)
	}
}

func TestTaskEvaluationUsesNormalTaskRuntimeOnce(t *testing.T) {
	model := &evalTestModel{output: "done"}
	registry := tools.NewToolRegistry()
	err := runTaskEvaluation(
		context.Background(),
		"normal runtime prompt",
		nil,
		newEvalTestDir(t),
		evaluationPrinter(&bytes.Buffer{}),
		func(ctx context.Context, prompt string) normalTaskExecution {
			return executeNormalTask(ctx, model, registry, prompt, "eval-test", "main", nil, nil)
		},
	)
	if err == nil || !strings.Contains(err.Error(), "task evaluation failed") {
		t.Fatalf("runTaskEvaluation() error = %v, want task evaluation failed", err)
	}
	if model.calls != 1 || model.finalCalls != 1 {
		t.Fatalf("plan calls=%d final calls=%d; want one normal task execution", model.calls, model.finalCalls)
	}
	if model.planTask != "normal runtime prompt" || model.finalTask != "normal runtime prompt" {
		t.Fatalf("prompt propagation: plan=%q final=%q", model.planTask, model.finalTask)
	}
}

func TestTaskEvaluationAggregatesMockedLlamaUsage(t *testing.T) {
	outputDir := newEvalTestDir(t)
	completions := []string{
		`{"content":"No more tools needed","stop":true,"tokens_evaluated":100,"tokens_predicted":10}`,
		`{"content":"final answer","stop":true,"timings":{"prompt_n":80,"predicted_n":20}}`,
	}
	completionIndex := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apply-template":
			_, _ = w.Write([]byte(`{"prompt":"rendered prompt"}`))
		case "/completion":
			if completionIndex >= len(completions) {
				t.Fatalf("unexpected completion request %d", completionIndex+1)
			}
			_, _ = w.Write([]byte(completions[completionIndex]))
			completionIndex++
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	model := &LlamaCppModel{Client: NewLlamaClient(server.URL), Model: "test-model"}
	model.Client.HTTP = server.Client()
	var stdout bytes.Buffer
	err := runTaskEvaluation(
		context.Background(),
		"answer without tools",
		nil,
		outputDir,
		evaluationPrinter(&stdout),
		func(ctx context.Context, prompt string) normalTaskExecution {
			return executeNormalTask(ctx, model, tools.NewToolRegistry(), prompt, "usage-test", "main", nil, nil)
		},
	)
	if err == nil || !strings.Contains(err.Error(), "task evaluation failed") {
		t.Fatalf("runTaskEvaluation() error = %v, want task evaluation failed", err)
	}
	if completionIndex != 2 {
		t.Fatalf("completion requests = %d, want 2", completionIndex)
	}
	if want := "tokens 180 prompt/30 output/210 total"; !strings.Contains(stdout.String(), want) {
		t.Fatalf("summary %q does not contain %q", stdout.String(), want)
	}
	jsonReport, err := os.ReadFile(filepath.Join(outputDir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"prompt_tokens": 180`,
		`"output_tokens": 30`,
		`"total_tokens": 210`,
	} {
		if !strings.Contains(string(jsonReport), want) {
			t.Fatalf("JSON report does not contain %q:\n%s", want, jsonReport)
		}
	}
	markdown, err := os.ReadFile(filepath.Join(outputDir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Tokens (prompt / output / total): 180 / 30 / 210",
		"| task | fail |",
		"180 / 30 / 210",
	} {
		if !strings.Contains(string(markdown), want) {
			t.Fatalf("Markdown report does not contain %q:\n%s", want, markdown)
		}
	}
}

func TestTaskEvaluationUnknownUsageRemainsZero(t *testing.T) {
	outputDir := newEvalTestDir(t)
	var stdout bytes.Buffer
	err := runTaskEvaluation(
		context.Background(),
		"backend without usage",
		nil,
		outputDir,
		evaluationPrinter(&stdout),
		func(context.Context, string) normalTaskExecution {
			return normalTaskExecution{Output: "done"}
		},
	)
	if err == nil || !strings.Contains(err.Error(), "task evaluation failed") {
		t.Fatalf("runTaskEvaluation() error = %v, want task evaluation failed", err)
	}
	if want := "tokens 0 prompt/0 output/0 total"; !strings.Contains(stdout.String(), want) {
		t.Fatalf("summary %q does not contain %q", stdout.String(), want)
	}
}

func TestRunEvaluationPassesFlagsAndWritesReports(t *testing.T) {
	outputDir := newEvalTestDir(t)
	var gotName string
	var gotOptions suites.Options
	lookup := func(name string, opts suites.Options) (eval.Suite, error) {
		gotName = name
		gotOptions = opts
		return eval.Suite{
			Name:      "passing-suite",
			Component: opts.Model.Component,
			Cases: []eval.Case{{
				Name:    "passes",
				Input:   eval.Input{Task: "return ok"},
				Scorers: []eval.Scorer{eval.ExactScorer{Want: "ok"}},
			}},
		}, nil
	}

	var stdout bytes.Buffer
	err := runEvaluationWithLookup(
		context.Background(),
		&evalTestModel{output: "ok"},
		"requested-suite",
		outputDir,
		evaluationPrinter(&stdout),
		lookup,
	)
	if err != nil {
		t.Fatalf("runEvaluationWithLookup() error = %v", err)
	}
	if gotName != "requested-suite" {
		t.Fatalf("lookup suite name = %q, want requested-suite", gotName)
	}
	if gotOptions.Model.Component != "model:test-model" || gotOptions.Model.Runner == nil {
		t.Fatalf("lookup options = %+v, want configured test model runner", gotOptions.Model)
	}
	for _, name := range []string{"passing-suite.json", "passing-suite.md"} {
		if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
			t.Fatalf("report %s was not written: %v", name, err)
		}
	}
	summary := stdout.String()
	for _, want := range []string{
		"Evaluation passing-suite: 1/1 passed (100.0%)",
		"reports: " + outputDir,
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q does not contain %q", summary, want)
		}
	}
}

func TestModelEvaluationRunnerUsesPlanningToolCallPath(t *testing.T) {
	model := &evalTestModel{
		output: "call fs",
		toolCall: &engine.ToolCall{
			Name: "fs",
			Args: map[string]string{"op": "list", "path": "."},
		},
	}
	result, err := (modelEvaluationRunner{model: model}).run(
		context.Background(),
		eval.Input{Task: "List the files in the current directory."},
	)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || model.finalCalls != 0 {
		t.Fatalf("planning calls=%d final-answer calls=%d; want 1, 0", model.calls, model.finalCalls)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Tool != "fs" ||
		result.ToolCalls[0].Args["op"] != "list" || result.ToolCalls[0].Args["path"] != "." {
		t.Fatalf("unexpected structural tool call: %+v", result.ToolCalls)
	}
}

func TestRunEvaluationDefaultsOutputDirectory(t *testing.T) {
	root := newEvalTestDir(t)
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	lookup := func(_ string, opts suites.Options) (eval.Suite, error) {
		return passingEvalSuite(opts.Model.Component), nil
	}
	err = runEvaluationWithLookup(
		context.Background(),
		&evalTestModel{output: "ok"},
		"suite",
		"",
		evaluationPrinter(&bytes.Buffer{}),
		lookup,
	)
	if err != nil {
		t.Fatalf("runEvaluationWithLookup() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(defaultEvalOutput, "passing-suite.json")); err != nil {
		t.Fatalf("default report was not written: %v", err)
	}
}

func TestRunEvaluationUnknownSuiteDoesNotRunModel(t *testing.T) {
	model := &evalTestModel{output: "should not run"}
	var stdout bytes.Buffer
	err := runEvaluationWithLookup(
		context.Background(),
		model,
		"missing",
		newEvalTestDir(t),
		evaluationPrinter(&stdout),
		suites.Lookup,
	)
	if err == nil || !strings.Contains(err.Error(), `unknown evaluation suite "missing"`) {
		t.Fatalf("runEvaluationWithLookup() error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected summary for unknown suite: %q", stdout.String())
	}
	if model.calls != 0 {
		t.Fatalf("model was called %d times for an unknown suite", model.calls)
	}
}

func TestFilesystemEvaluationRunsWithoutModelAndWritesReports(t *testing.T) {
	outputDir := newEvalTestDir(t)
	var stdout bytes.Buffer
	err := runEvaluationWithRunners(
		context.Background(),
		nil,
		filesystemEvaluationRunner{tool: tools.FileSystemTool{}},
		suites.FilesystemToolSuite,
		outputDir,
		evaluationPrinter(&stdout),
		suites.Lookup,
	)
	if err != nil {
		t.Fatalf("filesystem evaluation failed without a model: %v", err)
	}
	if !strings.Contains(stdout.String(), "Evaluation fs-tool: 5/5 passed (100.0%)") {
		t.Fatalf("unexpected summary: %q", stdout.String())
	}
	for _, name := range []string{"fs-tool.json", "fs-tool.md"} {
		data, err := os.ReadFile(filepath.Join(outputDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(data), "fs-read-missing-file-errors") {
			t.Fatalf("%s does not include the error case:\n%s", name, data)
		}
	}
	jsonReport, err := os.ReadFile(filepath.Join(outputDir, "fs-tool.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"case_name": "fs-read-existing-file"`,
		`"case_name": "fs-write-file"`,
		`"case_name": "fs-edit-unique-substring"`,
		`"error": "open`,
		`"error": "old_snippet must appear exactly once`,
	} {
		if !strings.Contains(string(jsonReport), want) {
			t.Fatalf("JSON report does not contain %q:\n%s", want, jsonReport)
		}
	}
}

func TestFilesystemEvaluationRunnerConfinesPaths(t *testing.T) {
	root := newEvalTestDir(t)
	runner := filesystemEvaluationRunner{tool: tools.FileSystemTool{}}
	for _, path := range []string{"../escape.txt", filepath.Join(root, "absolute.txt")} {
		_, err := runner.Run(
			context.Background(),
			"tool:fs",
			eval.Input{Task: `{"tool":"fs","args":{"op":"write","path":` + strconv.Quote(path) + `,"content":"bad"}}`},
			root,
		)
		if err == nil || !strings.Contains(err.Error(), "escapes the evaluation fixture directory") {
			t.Fatalf("path %q error = %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("escape path was created or stat failed unexpectedly: %v", err)
	}
}

func TestRunEvaluationSummaryAndFailureSemantics(t *testing.T) {
	tests := []struct {
		name       string
		suite      eval.Suite
		model      *evalTestModel
		wantErr    string
		wantOutput string
	}{
		{
			name:       "successful suite",
			suite:      passingEvalSuite("model:test-model"),
			model:      &evalTestModel{output: "ok"},
			wantOutput: "1/1 passed (100.0%)",
		},
		{
			name: "failed case returns error after summary",
			suite: eval.Suite{
				Name:      "failing-suite",
				Component: "model:test-model",
				Cases: []eval.Case{{
					Name:    "fails",
					Scorers: []eval.Scorer{eval.ExactScorer{Want: "wanted"}},
				}},
			},
			model:      &evalTestModel{output: "actual"},
			wantErr:    `evaluation "suite" failed: 1 of 1 cases failed`,
			wantOutput: "0/1 passed (0.0%)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			lookup := func(_ string, _ suites.Options) (eval.Suite, error) {
				return tt.suite, nil
			}
			err := runEvaluationWithLookup(
				context.Background(),
				tt.model,
				"suite",
				newEvalTestDir(t),
				evaluationPrinter(&stdout),
				lookup,
			)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("runEvaluationWithLookup() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Fatalf("runEvaluationWithLookup() error = %v, want %q", err, tt.wantErr)
			}
			if !strings.Contains(stdout.String(), tt.wantOutput) {
				t.Fatalf("summary %q does not contain %q", stdout.String(), tt.wantOutput)
			}
		})
	}
}

func TestRunEvaluationReportErrorFails(t *testing.T) {
	root := newEvalTestDir(t)
	outputPath := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(outputPath, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}

	lookup := func(_ string, opts suites.Options) (eval.Suite, error) {
		return passingEvalSuite(opts.Model.Component), nil
	}
	err := runEvaluationWithLookup(
		context.Background(),
		&evalTestModel{output: "ok"},
		"suite",
		outputPath,
		evaluationPrinter(&bytes.Buffer{}),
		lookup,
	)
	if err == nil || !strings.Contains(err.Error(), "write report") {
		t.Fatalf("runEvaluationWithLookup() error = %v, want report error", err)
	}
}

func TestRunEvaluationPreservesStabilityRunErrors(t *testing.T) {
	runErr := errors.New("backend unavailable")
	model := &evalTestModel{
		output: "pass",
		plan: func(call int) (*engine.Step, error) {
			if call == 1 {
				return &engine.Step{Plan: "pass"}, nil
			}
			return nil, runErr
		},
	}
	outputDir := newEvalTestDir(t)
	lookup := func(_ string, opts suites.Options) (eval.Suite, error) {
		return eval.Suite{
			Name:      "stability-errors",
			Component: opts.Model.Component,
			Cases: []eval.Case{{
				Name: "repeated-run",
				Scorers: []eval.Scorer{suites.StabilityScorer{
					MinPassRate: 1,
					Scorer:      eval.ExactScorer{Want: "pass"},
					Runs:        2,
					Component:   opts.Model.Component,
					Runner:      opts.Model.Runner,
				}},
			}},
		}, nil
	}
	err := runEvaluationWithLookup(
		context.Background(),
		model,
		"suite",
		outputDir,
		evaluationPrinter(&bytes.Buffer{}),
		lookup,
	)
	if !errors.Is(err, runErr) || !strings.Contains(err.Error(), "stability run 2") {
		t.Fatalf("runEvaluationWithLookup() error = %v", err)
	}
	report, readErr := os.ReadFile(filepath.Join(outputDir, "stability-errors.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(report), "stability run 2") ||
		!strings.Contains(string(report), "backend unavailable") {
		t.Fatalf("report did not preserve repeated-run error:\n%s", report)
	}
}

func TestEvaluationModeIsolation(t *testing.T) {
	normalModeCalls := 0
	evalModeCalls := 0
	runEval := func() error {
		evalModeCalls++
		return nil
	}
	runNormal := func() error {
		normalModeCalls++
		return nil
	}

	if err := dispatchEvaluationMode(evaluationModeOptions{Suite: "suite"}, runEval, runNormal); err != nil {
		t.Fatal(err)
	}
	if evalModeCalls != 1 || normalModeCalls != 0 {
		t.Fatalf("eval calls = %d, normal calls = %d; want 1, 0", evalModeCalls, normalModeCalls)
	}

	err := dispatchEvaluationMode(
		evaluationModeOptions{Suite: "suite", Tasks: []string{"must not run"}},
		runEval,
		runNormal,
	)
	if err == nil {
		t.Fatal("expected incompatible normal mode to be rejected")
	}
	if evalModeCalls != 1 || normalModeCalls != 0 {
		t.Fatalf("rejected dispatch changed calls: eval = %d, normal = %d", evalModeCalls, normalModeCalls)
	}
}

func passingEvalSuite(component string) eval.Suite {
	return eval.Suite{
		Name:      "passing-suite",
		Component: component,
		Cases: []eval.Case{{
			Name:    "passes",
			Scorers: []eval.Scorer{eval.ExactScorer{Want: "ok"}},
		}},
	}
}

func newEvalTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".eval-cli-test-*")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove test directory: %v", err)
		}
	})
	t.Setenv("TMPDIR", dir)
	return dir
}

func TestTaskEvaluationJudgeScoresLLMCriteria(t *testing.T) {
	const judgeJSON = `{"score": 9, "rationale": "correct tool used"}`
	judge, _ := newTaskJudgeTestModel(t, "ollama", judgeJSON, http.StatusOK, "")

	outputDir := newEvalTestDir(t)
	var stdout bytes.Buffer
	executor := func(_ context.Context, _ string) normalTaskExecution {
		return normalTaskExecution{Output: "The requested task is complete."}
	}
	if err := runTaskEvaluation(
		context.Background(),
		"judge the task",
		judge,
		outputDir,
		evaluationPrinter(&stdout),
		executor,
	); err != nil {
		t.Fatalf("runTaskEvaluation: %v", err)
	}

	report, err := os.ReadFile(filepath.Join(outputDir, "task.json"))
	if err != nil {
		t.Fatalf("read task report: %v", err)
	}
	text := string(report)
	if strings.Contains(text, "no_judge_configured") {
		t.Errorf("judge-configured run still reports no_judge_configured; report:\n%s", text)
	}
	if !strings.Contains(text, `"rationale": "correct tool used"`) {
		t.Errorf("llm criteria were not scored by the judge model; report:\n%s", text)
	}
}
