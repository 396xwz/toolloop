package suites

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/396xwz/toolloop/internal/eval"
)

const ModelBackendSuite = "model-backend"

// ResultRunner executes one model evaluation. Callers adapt their configured
// model implementation to this boundary, keeping suites independent of model
// services and CLI packages.
type ResultRunner func(ctx context.Context, component string, input eval.Input) (eval.Result, error)

type ModelSuiteOptions struct {
	Component         string
	StabilityRuns     int
	Runner            ResultRunner
	BaselineComponent string
	MaxCostMultiplier float64
}

// NewModelStabilitySuite constructs the model suite around caller-provided
// model execution. No model server is contacted during construction.
func NewModelStabilitySuite(opts ModelSuiteOptions) eval.Suite {
	component := opts.Component
	if component == "" {
		component = "model"
	}
	runs := opts.StabilityRuns
	if runs <= 0 {
		runs = 10
	}
	maxCostMultiplier := opts.MaxCostMultiplier
	if maxCostMultiplier <= 0 {
		maxCostMultiplier = 2
	}

	cases := []eval.Case{
		{
			Name: "model-emits-valid-tool-call-json",
			Input: eval.Input{
				Task: "List the files in the current directory.",
			},
			Scorers: []eval.Scorer{
				eval.ToolCallScorer{Tool: "fs", Args: map[string]string{"op": "list", "path": "."}},
				eval.LatencyScorer{Max: 10 * time.Second},
			},
		},
		{
			Name: "model-tool-call-stability",
			Input: eval.Input{
				Task: "Read the file plan.md.",
			},
			Scorers: []eval.Scorer{
				StabilityScorer{
					MinPassRate: 0.90,
					Scorer: eval.ToolCallScorer{
						Tool: "fs",
						Args: map[string]string{"op": "read", "path": "plan.md"},
					},
					Runs:      runs,
					Component: component,
					Runner:    opts.Runner,
				},
			},
		},
	}

	if opts.BaselineComponent != "" {
		cases = append(cases, eval.Case{
			Name: "model-cost-delta",
			Input: eval.Input{
				Task: "Summarize what this project does in one sentence.",
			},
			Scorers: []eval.Scorer{
				CostDeltaScorer{
					BaseBackend:   opts.BaselineComponent,
					MaxMultiplier: maxCostMultiplier,
					Runner:        opts.Runner,
				},
			},
		})
	}

	return eval.Suite{
		Name:      ModelBackendSuite,
		Component: component,
		Cases:     cases,
	}
}

// StabilityScorer scores the initial result and enough fresh executions to
// reach Runs, then compares the inner scorer's observed pass rate.
type StabilityScorer struct {
	MinPassRate float64
	Scorer      eval.Scorer
	Runs        int
	Component   string
	Runner      ResultRunner
}

func (s StabilityScorer) Name() string {
	return fmt.Sprintf("stability(%.0f%%)", s.MinPassRate*100)
}

func (s StabilityScorer) Score(ctx context.Context, input eval.Input, result eval.Result) eval.ScoreDetail {
	if s.Scorer == nil {
		return failedScore(s.Name(), "inner scorer is not configured")
	}
	runs := s.Runs
	if runs <= 0 {
		runs = 10
	}

	passes := 0
	var runErrors []error
	if result.Err == nil {
		detail := s.Scorer.Score(ctx, input, result)
		if detail.Err != nil {
			runErrors = append(runErrors, fmt.Errorf("stability run 1 scorer: %w", detail.Err))
		} else if detail.Passed {
			passes++
		}
	}
	for run := 1; run < runs; run++ {
		if s.Runner == nil {
			return failedScore(s.Name(), "result runner is not configured")
		}
		repeated, err := s.Runner(ctx, s.Component, input)
		if err != nil {
			runErrors = append(runErrors, fmt.Errorf("stability run %d: %w", run+1, err))
			continue
		}
		if repeated.Err != nil {
			runErrors = append(runErrors, fmt.Errorf("stability run %d: %w", run+1, repeated.Err))
			continue
		}
		detail := s.Scorer.Score(ctx, input, repeated)
		if detail.Err != nil {
			runErrors = append(runErrors, fmt.Errorf("stability run %d scorer: %w", run+1, detail.Err))
		} else if detail.Passed {
			passes++
		}
	}

	rate := float64(passes) / float64(runs)
	pass := rate >= s.MinPassRate
	detail := eval.ScoreDetail{
		ScorerName: s.Name(),
		Passed:     pass,
		Score:      rate,
		Reason:     fmt.Sprintf("%d of %d runs passed (%.0f%%)", passes, runs, rate*100),
	}
	if len(runErrors) > 0 {
		detail.Passed = false
		detail.Err = errors.Join(runErrors...)
		detail.Reason = detail.Err.Error()
	}
	return detail
}

// CostDeltaScorer runs a baseline component and checks the current result's
// reported cost against it.
type CostDeltaScorer struct {
	BaseBackend   string
	MaxMultiplier float64
	Runner        ResultRunner
}

func (s CostDeltaScorer) Name() string {
	return "cost_delta:" + s.BaseBackend
}

func (s CostDeltaScorer) Score(ctx context.Context, input eval.Input, result eval.Result) eval.ScoreDetail {
	if s.Runner == nil {
		return failedScore(s.Name(), "result runner is not configured")
	}
	if s.BaseBackend == "" {
		return failedScore(s.Name(), "baseline component is not configured")
	}
	if s.MaxMultiplier <= 0 {
		return failedScore(s.Name(), "maximum multiplier must be greater than zero")
	}
	if result.Cost < 0 {
		return failedScore(s.Name(), "current result reported a negative cost")
	}

	baseline, err := s.Runner(ctx, s.BaseBackend, input)
	if err != nil {
		return failedScore(s.Name(), "baseline run failed: "+err.Error())
	}
	if baseline.Cost < 0 {
		return failedScore(s.Name(), "baseline reported a negative cost")
	}

	limit := baseline.Cost * s.MaxMultiplier
	pass := result.Cost <= limit
	score := 1.0
	if !pass {
		score = 0
	}
	return eval.ScoreDetail{
		ScorerName: s.Name(),
		Passed:     pass,
		Score:      score,
		Reason: fmt.Sprintf(
			"current cost %.6f, baseline %.6f, limit %.6f",
			result.Cost,
			baseline.Cost,
			limit,
		),
	}
}

func failedScore(name, reason string) eval.ScoreDetail {
	return eval.ScoreDetail{ScorerName: name, Reason: reason}
}
