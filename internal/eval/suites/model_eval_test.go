package suites

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/396xwz/toolloop/internal/eval"
)

func TestNewModelStabilitySuiteIsServiceIndependent(t *testing.T) {
	suite := NewModelStabilitySuite(ModelSuiteOptions{Component: "model:test", StabilityRuns: 3})
	if suite.Name != ModelBackendSuite || suite.Component != "model:test" {
		t.Fatalf("unexpected suite: %+v", suite)
	}
	if len(suite.Cases) != 2 {
		t.Fatalf("got %d cases without a baseline, want 2", len(suite.Cases))
	}

	withBaseline := NewModelStabilitySuite(ModelSuiteOptions{BaselineComponent: "model:baseline"})
	if len(withBaseline.Cases) != 3 {
		t.Fatalf("got %d cases with a baseline, want 3", len(withBaseline.Cases))
	}
}

func TestStabilityScorerUsesFreshRuns(t *testing.T) {
	outputs := []string{"pass", "fail", "pass"}
	calls := 0
	scorer := StabilityScorer{
		MinPassRate: 0.75,
		Scorer:      eval.ExactScorer{Want: "pass"},
		Runs:        4,
		Component:   "model:test",
		Runner: func(_ context.Context, component string, _ eval.Input) (eval.Result, error) {
			if component != "model:test" {
				t.Fatalf("component=%q", component)
			}
			output := outputs[calls]
			calls++
			return eval.Result{Output: output}, nil
		},
	}

	detail := scorer.Score(context.Background(), eval.Input{Task: "task"}, eval.Result{Output: "pass"})
	if !detail.Passed || detail.Score != 0.75 || calls != 3 {
		t.Fatalf("detail=%+v calls=%d", detail, calls)
	}
}

func TestStabilityScorerHandlesRunFailures(t *testing.T) {
	runErr := errors.New("unavailable")
	scorer := StabilityScorer{
		MinPassRate: 1,
		Scorer:      eval.ExactScorer{Want: "pass"},
		Runs:        2,
		Runner: func(context.Context, string, eval.Input) (eval.Result, error) {
			return eval.Result{}, runErr
		},
	}
	detail := scorer.Score(context.Background(), eval.Input{}, eval.Result{Output: "pass"})
	if detail.Passed || detail.Score != 0.5 || !errors.Is(detail.Err, runErr) ||
		!strings.Contains(detail.Reason, "stability run 2") {
		t.Fatalf("unexpected detail: %+v", detail)
	}
}

func TestModelSuiteStructurallyScoresToolCalls(t *testing.T) {
	suite := NewModelStabilitySuite(ModelSuiteOptions{StabilityRuns: 1})
	listResult := eval.Result{ToolCalls: []eval.ToolCallRecord{{
		Tool: "fs",
		Args: map[string]any{"op": "list", "path": "."},
	}}}
	if detail := suite.Cases[0].Scorers[0].Score(
		context.Background(),
		suite.Cases[0].Input,
		listResult,
	); !detail.Passed {
		t.Fatalf("expected structural list call to pass: %+v", detail)
	}
	listResult.Output = `{"tool":"fs","args":{"op":"list","path":"."}}`
	listResult.ToolCalls = nil
	if detail := suite.Cases[0].Scorers[0].Score(
		context.Background(),
		suite.Cases[0].Input,
		listResult,
	); detail.Passed {
		t.Fatalf("raw JSON without a parsed tool call unexpectedly passed: %+v", detail)
	}
}

func TestCostDeltaScorerRunsBaseline(t *testing.T) {
	scorer := CostDeltaScorer{
		BaseBackend:   "model:baseline",
		MaxMultiplier: 2,
		Runner: func(_ context.Context, component string, input eval.Input) (eval.Result, error) {
			if component != "model:baseline" || input.Task != "task" {
				t.Fatalf("component=%q input=%+v", component, input)
			}
			return eval.Result{Cost: 0.5}, nil
		},
	}

	if detail := scorer.Score(
		context.Background(),
		eval.Input{Task: "task"},
		eval.Result{Cost: 0.75},
	); !detail.Passed {
		t.Fatalf("expected pass: %+v", detail)
	}
	if detail := scorer.Score(
		context.Background(),
		eval.Input{Task: "task"},
		eval.Result{Cost: 1.01},
	); detail.Passed {
		t.Fatalf("expected failure: %+v", detail)
	}
}

func TestRegistryListAndLookup(t *testing.T) {
	names := List()
	if len(names) != 3 || names[0] != FilesystemToolSuite || names[1] != ModelBackendSuite || names[2] != TaskSuite {
		t.Fatalf("unexpected names: %v", names)
	}

	suite, err := Lookup(ModelBackendSuite, Options{
		Model: ModelSuiteOptions{Component: "model:test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if suite.Name != ModelBackendSuite || suite.Component != "model:test" {
		t.Fatalf("unexpected suite: %+v", suite)
	}
	taskSuite, err := Lookup(TaskSuite, Options{Task: "run this"})
	if err != nil {
		t.Fatal(err)
	}
	if taskSuite.Component != "task" || len(taskSuite.Cases) != 1 ||
		taskSuite.Cases[0].Input.Task != "run this" || len(taskSuite.Cases[0].Scorers) != 0 {
		t.Fatalf("unexpected task suite: %+v", taskSuite)
	}

	_, err = Lookup("missing", Options{})
	if err == nil || !strings.Contains(err.Error(), `unknown evaluation suite "missing"`) ||
		!strings.Contains(err.Error(), "available: fs-tool, model-backend, task") {
		t.Fatalf("unexpected unknown-suite error: %v", err)
	}
}
