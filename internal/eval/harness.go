package eval

import (
	"context"
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
		}
		if !detail.Passed {
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
