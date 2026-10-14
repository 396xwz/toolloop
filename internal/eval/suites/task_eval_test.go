package suites

import (
	"context"
	"strings"
	"testing"

	"github.com/396xwz/toolloop/internal/eval"
)

func TestTaskRubricAcceptsCorrectProse(t *testing.T) {
	suite := NewTaskSuite("Explain why the sky is blue in prose.", nil)
	rubric := suite.Cases[0].Rubric
	if len(rubric.Criteria) != 2 {
		t.Fatalf("criteria = %+v, want only correctness and grounding", rubric.Criteria)
	}
	if rubric.Criteria[0].Weight != 0.625 || rubric.Criteria[1].Weight != 0.375 {
		t.Fatalf("weights do not preserve the original 5:3 ratio: %+v", rubric.Criteria)
	}
	for _, criterion := range rubric.Criteria {
		if criterion.Check == "json_valid" || criterion.Name == "valid_json" {
			t.Fatal("generic prose task has an unconditional JSON check")
		}
	}
	const output = "Air molecules scatter shorter blue wavelengths more strongly than red wavelengths."
	calls := 0
	results, total := eval.EvaluateRubric(rubric, suite.Cases[0].Input.Task, output, "", "",
		func(prompt string) (string, error) {
			calls++
			if !strings.Contains(prompt, output) || !strings.Contains(prompt, suite.Cases[0].Input.Task) {
				t.Errorf("judge prompt omitted task or prose: %q", prompt)
			}
			return `{"score":2,"rationale":"Correct and supported prose answer."}`, nil
		}, "test-judge")
	if calls != 2 || len(results) != 2 || total != 1 || !rubric.Passed(total) {
		t.Fatalf("correct prose was penalized: calls=%d results=%+v total=%v", calls, results, total)
	}
}

func TestLookupTaskSuiteWithoutJudgeHasNoScorers(t *testing.T) {
	suite, err := Lookup(TaskSuite, Options{Task: "inspect input.txt"})
	if err != nil {
		t.Fatalf("Lookup(%s): %v", TaskSuite, err)
	}
	if suite.Name != TaskSuite || suite.Component != "task" {
		t.Errorf("suite = name %q component %q, want %q/task", suite.Name, suite.Component, TaskSuite)
	}
	if len(suite.Cases) != 1 {
		t.Fatalf("len(cases) = %d, want 1", len(suite.Cases))
	}
	c := suite.Cases[0]
	if c.Name != "task" {
		t.Errorf("case name = %q, want task", c.Name)
	}
	if c.Input.Task != "inspect input.txt" {
		t.Errorf("input task = %q, want inspect input.txt", c.Input.Task)
	}
	if len(c.Scorers) != 0 {
		t.Errorf("len(scorers) without judge = %d, want 0", len(c.Scorers))
	}
	if c.Rubric == nil {
		t.Fatal("rubric is nil, want non-nil")
	}
	if c.Rubric.Threshold != 0.8 {
		t.Errorf("rubric threshold = %v, want 0.8", c.Rubric.Threshold)
	}
}

func TestLookupTaskSuiteWithJudgeCarriesLLMJudgeScorer(t *testing.T) {
	const marker = `{"score": 7, "rationale": "marker"}`
	judge := &eval.LLMJudgeScorer{
		Criterion:  "task correctness",
		MinScore:   5,
		JudgeModel: "test-model",
		Judge: func(ctx context.Context, model, prompt string) (string, error) {
			if model != "test-model" || prompt == "" {
				t.Errorf("judge called with model %q prompt %q", model, prompt)
			}
			return marker, nil
		},
	}

	suite, err := Lookup(TaskSuite, Options{Task: "inspect input.txt", TaskJudge: judge})
	if err != nil {
		t.Fatalf("Lookup(%s): %v", TaskSuite, err)
	}
	c := suite.Cases[0]
	if len(c.Scorers) != 1 {
		t.Fatalf("len(scorers) = %d, want 1", len(c.Scorers))
	}
	s, ok := c.Scorers[0].(eval.LLMJudgeScorer)
	if !ok {
		t.Fatalf("scorer type = %T, want eval.LLMJudgeScorer", c.Scorers[0])
	}
	if s.Criterion != "task correctness" || s.JudgeModel != "test-model" || s.MinScore != 5 {
		t.Errorf("scorer fields = %+v, want criterion task correctness model test-model min 5", s)
	}
	if s.Judge == nil {
		t.Fatal("carried Judge closure is nil")
	}
	out, err := s.Judge(context.Background(), "test-model", "grade this")
	if err != nil || out != marker {
		t.Errorf("Judge() = %q, %v; want marker output, no error", out, err)
	}
}

func TestLookupTaskSuiteWithNilJudgeClosureHasNoScorers(t *testing.T) {
	judge := &eval.LLMJudgeScorer{Criterion: "task correctness", MinScore: 5, JudgeModel: "m"} // no Judge set
	suite, err := Lookup(TaskSuite, Options{Task: "x", TaskJudge: judge})
	if err != nil {
		t.Fatalf("Lookup(%s): %v", TaskSuite, err)
	}
	if len(suite.Cases[0].Scorers) != 0 {
		t.Errorf("len(scorers) with nil Judge closure = %d, want 0", len(suite.Cases[0].Scorers))
	}
}
