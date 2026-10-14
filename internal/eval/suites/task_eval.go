package suites

import "github.com/396xwz/toolloop/internal/eval"

const TaskSuite = "task"

// taskEvalRubricYAML is the canonical rubric from RUBRICS.md, kept next to the
// case it scores. Parsed once at suite construction; llm criteria degrade to a
// zero score with "no_judge_configured" when NewTaskSuite receives no judge.
const taskEvalRubricYAML = `name: task
threshold: 0.8
criteria:
  - name: correctness
    description: "Output correctly and completely answers the task, following its requested format and constraints"
    weight: 0.625
    type: llm
    scale: 2
  - name: no_hallucinated_fields
    description: "Output makes no unsupported claims or invented facts; any tool calls use only supported schema fields"
    weight: 0.375
    type: llm
    scale: 2`

// NewTaskSuite evaluates one normal task execution scored against the
// embedded rubric. Pass a non-nil LLMJudgeScorer (with its Judge closure set)
// to grade the llm criteria; without it they degrade to zero scores with
// rationale "no_judge_configured".
func NewTaskSuite(prompt string, judge *eval.LLMJudgeScorer) eval.Suite {
	rubric, err := eval.ParseRubric([]byte(taskEvalRubricYAML))
	if err != nil {
		panic("suites: invalid task rubric YAML: " + err.Error())
	}
	caseObj := eval.Case{
		Name:   "task",
		Input:  eval.Input{Task: prompt},
		Rubric: rubric,
	}
	if judge != nil && judge.Judge != nil {
		scorer := *judge
		caseObj.Scorers = append(caseObj.Scorers, scorer)
	}
	return eval.Suite{
		Name:      TaskSuite,
		Component: "task",
		Cases:     []eval.Case{caseObj},
	}
}
