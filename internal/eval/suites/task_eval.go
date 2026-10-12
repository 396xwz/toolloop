package suites

import "github.com/396xwz/toolloop/internal/eval"

const TaskSuite = "task"

// NewTaskSuite evaluates only whether one normal task execution completes.
// It intentionally has no answer scorer or LLM judge.
func NewTaskSuite(prompt string) eval.Suite {
	return eval.Suite{
		Name:      TaskSuite,
		Component: "task",
		Cases: []eval.Case{{
			Name:  "task",
			Input: eval.Input{Task: prompt},
		}},
	}
}
