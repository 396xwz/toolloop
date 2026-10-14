package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RunOptions struct {
	Tags      []string // filter cases by tag; empty = run all
	Parallel  int      // max concurrent cases; default 1
	OutputDir string   // where to write JSON + Markdown reports
	Verbose   bool
	Runner    ComponentRunner
}

type TokenUsage struct {
	Prompt int
	Output int
	Total  int
}

type ComponentResult struct {
	Output    string
	Artifacts map[string]string
	ToolCalls []ToolCallRecord
	Tokens    TokenUsage
	Cost      float64
}

type ComponentRunner interface {
	Run(ctx context.Context, component string, input Input, dir string) (ComponentResult, error)
}

type ComponentRunnerFunc func(context.Context, string, Input, string) (ComponentResult, error)

func (f ComponentRunnerFunc) Run(ctx context.Context, component string, input Input, dir string) (ComponentResult, error) {
	return f(ctx, component, input, dir)
}

func RunSuite(ctx context.Context, suite Suite, opts RunOptions) (metrics Metrics, returnErr error) {
	dir, err := os.MkdirTemp("", "toolloop-eval-*")
	if err != nil {
		return Metrics{}, fmt.Errorf("create workspace: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove workspace: %w", err))
		}
	}()

	if suite.Setup != nil {
		if err := suite.Setup(dir); err != nil {
			return Metrics{}, fmt.Errorf("setup: %w", err)
		}
	}
	if suite.Teardown != nil {
		defer func() {
			if err := suite.Teardown(dir); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("teardown: %w", err))
			}
		}()
	}

	selected := make([]Case, 0, len(suite.Cases))
	for _, testCase := range suite.Cases {
		if matchesTags(testCase.Tags, opts.Tags) {
			selected = append(selected, testCase)
		}
	}
	if len(selected) > 0 && opts.Runner == nil {
		return Metrics{}, errors.New("component runner is not configured")
	}

	results := make([]Result, len(selected))
	parallel := opts.Parallel
	if parallel <= 0 {
		parallel = 1
	}
	if parallel > len(selected) {
		parallel = len(selected)
	}
	if parallel > 0 {
		jobs := make(chan int)
		var wg sync.WaitGroup
		for range parallel {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for index := range jobs {
					results[index] = runCase(ctx, dir, suite.Component, selected[index], opts.Runner)
				}
			}()
		}
		for index := range selected {
			jobs <- index
		}
		close(jobs)
		wg.Wait()
	}

	var executionErrors []error
	for _, result := range results {
		if result.Err != nil {
			executionErrors = append(executionErrors, fmt.Errorf("%s: %w", result.CaseName, result.Err))
		}
		if opts.Verbose {
			for _, detail := range result.ScoreDetails {
				status := "✓"
				if !detail.Passed {
					status = "✗"
				}
				fmt.Printf("  %s [%s] %s — %s\n", status, result.CaseName, detail.ScorerName, detail.Reason)
			}
		}
	}

	metrics = Aggregate(suite.Name, suite.Component, results)
	if opts.OutputDir != "" {
		if err := WriteReport(opts.OutputDir, suite, results, metrics); err != nil {
			return metrics, errors.Join(errors.Join(executionErrors...), fmt.Errorf("write report: %w", err))
		}
	}
	return metrics, errors.Join(executionErrors...)
}

func runCase(ctx context.Context, root, component string, testCase Case, runner ComponentRunner) Result {
	result := Result{CaseName: testCase.Name}
	caseDir, err := caseDirectory(root, testCase.Name)
	if err != nil {
		result.Err = err
		return result
	}
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		result.Err = fmt.Errorf("create case directory: %w", err)
		return result
	}
	if err := writeFixtures(caseDir, testCase.Input.Files); err != nil {
		result.Err = fmt.Errorf("write fixtures: %w", err)
		return result
	}

	start := time.Now()
	execution, err := runner.Run(ctx, component, testCase.Input, caseDir)
	result.Latency = time.Since(start)
	result.Output = execution.Output
	result.Artifacts = execution.Artifacts
	result.ToolCalls = execution.ToolCalls
	result.PromptTokens = execution.Tokens.Prompt
	result.OutputTokens = execution.Tokens.Output
	result.TotalTokens = execution.Tokens.Total
	result.Cost = execution.Cost
	result.Err = err

	allPassed := err == nil
	for _, scorer := range testCase.Scorers {
		detail := scorer.Score(ctx, testCase.Input, result)
		result.ScoreDetails = append(result.ScoreDetails, detail)
		if detail.Err != nil {
			result.Err = errors.Join(result.Err, detail.Err)
			allPassed = false
		}
		if !detail.Passed {
			allPassed = false
		}
	}
	if testCase.Rubric != nil {
		judge, judgeModel := rubricJudgeFromScorers(ctx, testCase.Scorers)
		result.CriterionResults, result.RubricScore = EvaluateRubric(
			testCase.Rubric,
			testCase.Input.Task,
			result.Output,
			rubricToolCallsText(result.ToolCalls),
			rubricArtifactsText(result.Artifacts),
			judge,
			judgeModel,
		)
		result.RubricPassed = testCase.Rubric.Passed(result.RubricScore)
		for _, criterion := range result.CriterionResults {
			if criterion.Err != nil {
				result.Err = errors.Join(result.Err, fmt.Errorf("criterion %s: %w", criterion.Name, criterion.Err))
				result.RubricPassed = false
			}
		}
		if !result.RubricPassed {
			allPassed = false
		}
	}
	result.Passed = allPassed
	return result
}

func matchesTags(caseTags, filters []string) bool {
	for _, filter := range filters {
		found := false
		for _, tag := range caseTags {
			if tag == filter {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func caseDirectory(root, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid case name %q", name)
	}
	return filepath.Join(root, name), nil
}

func writeFixtures(root string, files map[string]string) error {
	for name, content := range files {
		clean := filepath.Clean(name)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid fixture path %q", name)
		}
		path := filepath.Join(root, clean)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// rubricJudgeFromScorers adapts the case's LLM-judge plumbing to a rubric JudgeFn,
// reusing the first LLMJudgeScorer with a configured Judge closure so rubric llm
// criteria are graded by the same model/prompt mechanism as scorers. Returns
// (nil, "") when no scorer configures a judge: llm criteria then score 0 with
// rationale "no_judge_configured" while deterministic checks still run.
func rubricJudgeFromScorers(ctx context.Context, scorers []Scorer) (JudgeFn, string) {
	for _, s := range scorers {
		j, ok := s.(LLMJudgeScorer)
		if !ok || j.Judge == nil {
			continue
		}
		model := j.JudgeModel
		return func(prompt string) (string, error) { return j.Judge(ctx, model, prompt) }, model
	}
	return nil, ""
}

// rubricToolCallsText renders ToolCallRecords as deterministic text for the judge
// prompt: one compact JSON object per line, in call order; "" when there are no calls.
func rubricToolCallsText(calls []ToolCallRecord) string {
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	for i, call := range calls {
		line, err := json.Marshal(call)
		if err != nil {
			line = []byte(fmt.Sprintf(`{"tool":%q,"error":%q}`, call.Tool, err.Error()))
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.Write(line)
	}
	return b.String()
}

// rubricArtifactsText renders Artifacts as a single JSON line; encoding/json sorts
// map keys, so the output is deterministic for a given artifact set. "" when empty.
func rubricArtifactsText(artifacts map[string]string) string {
	if len(artifacts) == 0 {
		return ""
	}
	data, err := json.Marshal(artifacts)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(data)
}
