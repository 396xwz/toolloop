package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/eval"
	"github.com/396xwz/toolloop/internal/eval/suites"
	"github.com/396xwz/toolloop/internal/tools"
)

const defaultEvalOutput = "eval-results"

type evaluationModeOptions struct {
	Suite          string
	OutputDir      string
	Tasks          []string
	IndexPath      string
	REPL           bool
	AgentName      string
	TopologyPath   string
	TopologyReport string
	DirectAction   bool
	OutputFile     string
}

func validateEvaluationMode(opts evaluationModeOptions) error {
	if opts.Suite == "" {
		if opts.OutputDir != "" {
			return fmt.Errorf("-eval-output requires -eval")
		}
		return nil
	}

	isTaskEvaluation := opts.Suite == suites.TaskSuite
	if isTaskEvaluation {
		if len(opts.Tasks) != 1 || strings.TrimSpace(opts.Tasks[0]) == "" {
			return fmt.Errorf("-eval task requires exactly one non-empty -task")
		}
	}

	var incompatible []string
	if len(opts.Tasks) > 0 && !isTaskEvaluation {
		incompatible = append(incompatible, "-task")
	}
	if opts.IndexPath != "" {
		incompatible = append(incompatible, "-index")
	}

	if opts.REPL {
		incompatible = append(incompatible, "-repl")
	}
	if opts.AgentName != "" {
		incompatible = append(incompatible, "-agent")
	}
	if opts.TopologyPath != "" {
		incompatible = append(incompatible, "-topology")
	}
	if opts.TopologyReport != "" {
		incompatible = append(incompatible, "-topology-report")
	}
	if opts.DirectAction {
		incompatible = append(incompatible, "direct tool flags")
	}
	if opts.OutputFile != "" {
		incompatible = append(incompatible, "-output")
	}
	if len(incompatible) > 0 {
		return fmt.Errorf("-eval cannot be combined with %s", strings.Join(incompatible, ", "))
	}
	return nil
}

type normalTaskExecution struct {
	Output         string
	Task           *engine.Task
	ExtraToolCalls []eval.ToolCallRecord
	Tokens         eval.TokenUsage
	Err            error
}

type taskEvaluationExecutor func(context.Context, string) normalTaskExecution

type taskEvaluationRunner struct {
	execute taskEvaluationExecutor
}

func (r taskEvaluationRunner) Run(
	ctx context.Context,
	component string,
	input eval.Input,
	_ string,
) (eval.ComponentResult, error) {
	if component != "task" {
		return eval.ComponentResult{}, fmt.Errorf("task runner does not support component %q", component)
	}
	if r.execute == nil {
		return eval.ComponentResult{}, fmt.Errorf("task executor is not configured")
	}
	execution := r.execute(ctx, input.Task)
	result := eval.ComponentResult{Output: execution.Output, Tokens: execution.Tokens}
	if execution.Task != nil {
		for _, step := range execution.Task.Steps {
			if step.ToolCall == nil || step.ToolCall.Name == "" {
				continue
			}
			args := make(map[string]any, len(step.ToolCall.Args))
			for key, value := range step.ToolCall.Args {
				args[key] = value
			}
			duration := time.Duration(0)
			if !step.StartedAt.IsZero() && !step.FinishedAt.Before(step.StartedAt) {
				duration = step.FinishedAt.Sub(step.StartedAt)
			}
			result.ToolCalls = append(result.ToolCalls, eval.ToolCallRecord{
				Tool:     step.ToolCall.Name,
				Args:     args,
				Duration: duration,
				Err:      step.Err,
			})
		}
	}
	result.ToolCalls = append(result.ToolCalls, execution.ExtraToolCalls...)
	return result, execution.Err
}

func runTaskEvaluation(
	ctx context.Context,
	prompt string,
	outputDir string,
	printf evaluationPrintf,
	execute taskEvaluationExecutor,
) error {
	if outputDir == "" {
		outputDir = defaultEvalOutput
	}
	suite, err := suites.Lookup(suites.TaskSuite, suites.Options{Task: prompt})
	if err != nil {
		return err
	}
	var execution normalTaskExecution
	executed := false
	capturingExecute := execute
	if execute != nil {
		capturingExecute = func(ctx context.Context, prompt string) normalTaskExecution {
			execution = execute(ctx, prompt)
			executed = true
			return execution
		}
	}
	metrics, runErr := eval.RunSuite(ctx, suite, eval.RunOptions{
		Parallel:  1,
		OutputDir: outputDir,
		Runner:    taskEvaluationRunner{execute: capturingExecute},
	})
	if executed && execution.Err == nil && execution.Output != "" {
		_, _ = printf("Task completed.\nFinal Answer:\n%s", execution.Output)
		if !strings.HasSuffix(execution.Output, "\n") {
			_, _ = printf("\n")
		}
	}
	status := "completed"
	if metrics.Failed > 0 || runErr != nil {
		status = "failed"
	}
	_, _ = printf(
		"Task evaluation %s: latency %s, tokens %d prompt/%d output/%d total, %d tool calls, %d tool errors; reports: %s\n",
		status,
		metrics.AvgLatency.Round(time.Millisecond),
		metrics.PromptTokens,
		metrics.OutputTokens,
		metrics.TotalTokens,
		metrics.ToolCalls,
		metrics.ToolErrors,
		outputDir,
	)
	if runErr != nil {
		return fmt.Errorf("task evaluation failed: %w", runErr)
	}
	if metrics.Failed > 0 {
		return fmt.Errorf("task evaluation failed")
	}
	return nil
}

func dispatchEvaluationMode(opts evaluationModeOptions, runEval, runNormal func() error) error {
	if err := validateEvaluationMode(opts); err != nil {
		return err
	}
	if opts.Suite != "" {
		return runEval()
	}
	return runNormal()
}

type modelEvaluationRunner struct {
	model engine.ChatModel
}

func (r modelEvaluationRunner) run(ctx context.Context, input eval.Input) (eval.Result, error) {
	task := &engine.Task{
		ID:          fmt.Sprintf("eval-%d", time.Now().UnixNano()),
		Description: input.Task,
		CreatedAt:   time.Now(),
		Status:      engine.TaskPending,
		Steps:       []*engine.Step{},
		Label:       "eval",
	}
	start := time.Now()
	step, err := r.model.PlanNextStep(ctx, task)
	result := eval.Result{
		Latency: time.Since(start),
		Err:     err,
	}
	if step != nil {
		result.Output = step.Plan
		if step.ToolCall != nil {
			args := make(map[string]any, len(step.ToolCall.Args))
			for key, value := range step.ToolCall.Args {
				args[key] = value
			}
			result.ToolCalls = []eval.ToolCallRecord{{
				Tool: step.ToolCall.Name,
				Args: args,
			}}
		}
	}
	if err == nil && step == nil {
		err = fmt.Errorf("model planning returned no step")
		result.Err = err
	}
	return result, err
}

func (r modelEvaluationRunner) Run(ctx context.Context, _ string, input eval.Input, _ string) (eval.ComponentResult, error) {
	result, err := r.run(ctx, input)
	return eval.ComponentResult{Output: result.Output, ToolCalls: result.ToolCalls}, err
}

func runEvaluation(ctx context.Context, model engine.ChatModel, suiteName, outputDir string) error {
	return runEvaluationWithLookup(ctx, model, suiteName, outputDir, fmt.Printf, suites.Lookup)
}

type evaluationPrintf func(format string, args ...any) (int, error)
type evaluationSuiteLookup func(string, suites.Options) (eval.Suite, error)

type filesystemTool interface {
	Name() string
	Execute(context.Context, map[string]string) (string, error)
}

type filesystemEvaluationRunner struct {
	tool filesystemTool
}

func (r filesystemEvaluationRunner) Run(
	ctx context.Context,
	component string,
	input eval.Input,
	dir string,
) (eval.ComponentResult, error) {
	if component != "tool:fs" {
		return eval.ComponentResult{}, fmt.Errorf("filesystem runner does not support component %q", component)
	}
	var request struct {
		Tool string            `json:"tool"`
		Args map[string]string `json:"args"`
	}
	if err := json.Unmarshal([]byte(input.Task), &request); err != nil {
		return eval.ComponentResult{}, fmt.Errorf("decode filesystem request: %w", err)
	}
	if request.Tool != "fs" {
		return eval.ComponentResult{}, fmt.Errorf("filesystem request uses tool %q", request.Tool)
	}
	if request.Args == nil {
		request.Args = make(map[string]string)
	}

	recordArgs := make(map[string]any, len(request.Args))
	for key, value := range request.Args {
		recordArgs[key] = value
	}
	path, err := confinedEvaluationPath(dir, request.Args["path"])
	if err != nil {
		return eval.ComponentResult{}, err
	}
	request.Args["path"] = path

	start := time.Now()
	output, toolErr := r.tool.Execute(ctx, request.Args)
	call := eval.ToolCallRecord{
		Tool:     r.tool.Name(),
		Args:     recordArgs,
		Duration: time.Since(start),
		Err:      toolErr,
	}
	result := eval.ComponentResult{
		Output:    output,
		ToolCalls: []eval.ToolCallRecord{call},
	}
	if toolErr != nil {
		result.Output = "error: " + toolErr.Error()
	}
	if operation := request.Args["op"]; operation == "write" || operation == "edit" {
		if content, readErr := os.ReadFile(path); readErr == nil {
			result.Artifacts = map[string]string{"content": string(content)}
		}
	}
	return result, nil
}

func confinedEvaluationPath(root, requested string) (string, error) {
	if requested == "" {
		requested = "."
	}
	clean := filepath.Clean(requested)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("filesystem path %q escapes the evaluation fixture directory", requested)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve evaluation fixture directory: %w", err)
	}
	path := filepath.Join(root, clean)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("filesystem path %q escapes the evaluation fixture directory", requested)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve evaluation fixture directory symlinks: %w", err)
	}
	existing := path
	for {
		if _, statErr := os.Lstat(existing); statErr == nil {
			break
		} else if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect filesystem path %q: %w", requested, statErr)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("filesystem path %q has no existing parent", requested)
		}
		existing = parent
	}
	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolve filesystem path %q symlinks: %w", requested, err)
	}
	resolvedRelative, err := filepath.Rel(resolvedRoot, resolvedExisting)
	if err != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("filesystem path %q escapes the evaluation fixture directory", requested)
	}
	return path, nil
}

func runEvaluationWithLookup(
	ctx context.Context,
	model engine.ChatModel,
	suiteName string,
	outputDir string,
	printf evaluationPrintf,
	lookup evaluationSuiteLookup,
) error {
	return runEvaluationWithRunners(
		ctx,
		model,
		filesystemEvaluationRunner{tool: tools.FileSystemTool{}},
		suiteName,
		outputDir,
		printf,
		lookup,
	)
}

func runEvaluationWithRunners(
	ctx context.Context,
	model engine.ChatModel,
	filesystemRunner eval.ComponentRunner,
	suiteName string,
	outputDir string,
	printf evaluationPrintf,
	lookup evaluationSuiteLookup,
) error {
	var opts suites.Options
	var modelRunner modelEvaluationRunner
	if model != nil {
		modelRunner = modelEvaluationRunner{model: model}
		opts.Model = suites.ModelSuiteOptions{
			Component: "model:" + model.ModelName(),
			Runner: func(ctx context.Context, _ string, input eval.Input) (eval.Result, error) {
				return modelRunner.run(ctx, input)
			},
		}
	}
	suite, err := lookup(suiteName, opts)
	if err != nil {
		return err
	}
	var runner eval.ComponentRunner
	switch suite.Component {
	case "tool:fs":
		runner = filesystemRunner
	default:
		if model == nil {
			return fmt.Errorf("evaluation suite %q requires a model backend", suiteName)
		}
		runner = modelRunner
	}
	if runner == nil {
		return fmt.Errorf("evaluation suite %q has no configured runner", suiteName)
	}
	if outputDir == "" {
		outputDir = defaultEvalOutput
	}

	metrics, runErr := eval.RunSuite(ctx, suite, eval.RunOptions{
		Parallel:  1,
		OutputDir: outputDir,
		Runner:    runner,
	})
	_, _ = printf(
		"Evaluation %s: %d/%d passed (%.1f%%), avg latency %s; reports: %s\n",
		metrics.SuiteName,
		metrics.Passed,
		metrics.TotalCases,
		metrics.PassRate,
		metrics.AvgLatency.Round(time.Millisecond),
		outputDir,
	)
	if runErr != nil {
		return fmt.Errorf("evaluation %q failed: %w", suiteName, runErr)
	}
	if metrics.Failed > 0 {
		return fmt.Errorf("evaluation %q failed: %d of %d cases failed", suiteName, metrics.Failed, metrics.TotalCases)
	}
	return nil
}

func evaluationPrinter(w io.Writer) evaluationPrintf {
	return func(format string, args ...any) (int, error) {
		return fmt.Fprintf(w, format, args...)
	}
}
