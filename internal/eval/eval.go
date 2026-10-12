package eval

import (
	"context"
	"time"
)

// Case is a single test case with an input and one or more scorers.
type Case struct {
	Name        string
	Description string
	Input       Input
	Scorers     []Scorer // all must pass for the case to be green
	Tags        []string // e.g. "tool:fs", "agent:builder", "slow"
}

// Input is what gets fed to the component under test.
type Input struct {
	Task    string            // task string / prompt
	Context map[string]string // upstream context (for graph nodes)
	Files   map[string]string // filename → content, written to tmp dir before run
	EnvVars map[string]string // injected into the component's environment
}

// Result is everything captured from one Case execution.
type Result struct {
	CaseName     string
	Passed       bool
	ScoreDetails []ScoreDetail
	Output       string            // raw final answer
	Artifacts    map[string]string // named outputs ("diff", "plan", etc.)
	ToolCalls    []ToolCallRecord  // every tool call made during the run
	Latency      time.Duration
	PromptTokens int
	OutputTokens int
	TotalTokens  int
	Cost         float64 // USD estimate
	Err          error
}

// ToolCallRecord captures one tool invocation for analysis.
type ToolCallRecord struct {
	Tool     string
	Args     map[string]any
	Duration time.Duration
	Repeated bool // true if repeat-detection would have fired
	Err      error
}

// ScoreDetail is the verdict from one Scorer.
type ScoreDetail struct {
	ScorerName string
	Passed     bool
	Score      float64 // 0.0–1.0 for fuzzy scorers
	Reason     string
	Err        error `json:"-"`
}

// Scorer evaluates a Result and returns a ScoreDetail.
type Scorer interface {
	Name() string
	Score(ctx context.Context, input Input, result Result) ScoreDetail
}

// Suite groups Cases that test the same component.
type Suite struct {
	Name      string
	Component string // "tool:fs" | "agent:builder" | "graph:review" | "model:ollama"
	Cases     []Case
	Setup     func(dir string) error // called once before all cases (create fixture files, etc.)
	Teardown  func(dir string) error // called once after all cases
}
