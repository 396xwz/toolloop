package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/396xwz/toolloop/internal/memory"
	"github.com/396xwz/toolloop/internal/tools"
)

type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
)

const MaxSteps = 8

type Task struct {
	ID          string
	Description string
	CreatedAt   time.Time
	Status      TaskStatus
	Steps       []*Step
	Memory      memory.Memory
	RAG         memory.RAG
	Tools       tools.ToolRegistry
}

type Step struct {
	Index      int
	Plan       string
	ToolCall   *ToolCall
	Result     string
	StartedAt  time.Time
	FinishedAt time.Time
	Err        error
}

type ToolCall struct {
	Name   string
	Args   map[string]string
	Output string
}

type Model interface {
	PlanNextStep(ctx context.Context, task *Task) (*Step, error)
}

// ChatModel adds final-answer generation and runtime prompt/model selection.
type ChatModel interface {
	Model
	GenerateFinalAnswer(context.Context, *Task) (string, error)
	ModelName() string
	SetModel(string)
	SystemPromptValue() string
	SetSystemPrompt(string)
}

type Engine struct {
	Model Model
}

func (e *Engine) RunTask(ctx context.Context, task *Task) error {
	task.Status = TaskRunning

	for i := 0; i < MaxSteps; i++ {
		step, err := e.Model.PlanNextStep(ctx, task)
		if err != nil {
			task.Status = TaskFailed
			return err
		}

		step.Index = len(task.Steps)
		step.StartedAt = time.Now()

		if step.ToolCall != nil && step.ToolCall.Name != "" && step.ToolCall.Name != "none" {
			// Detect repeated identical tool calls
			if isRepeatToolCall(task.Steps, step.ToolCall) {
				step.Result = "Stopped: same tool + args was already used."
				step.ToolCall = nil // force stop
				fmt.Println("  → Detected repeated tool call, stopping")
			} else {
				tool, ok := task.Tools.Get(step.ToolCall.Name)
				if !ok {
					step.Err = fmt.Errorf("unknown tool: %s", step.ToolCall.Name)
					step.Result = step.Err.Error()
				} else {
					out, err := tool.Execute(ctx, step.ToolCall.Args)
					step.ToolCall.Output = out
					step.Result = out
					step.Err = err
					fmt.Printf("  → Tool %s executed (args: %v)\n", step.ToolCall.Name, step.ToolCall.Args)

					// If this was an fs write that succeeded, consider the task complete to avoid asking the model again.
					if err == nil && step.ToolCall.Name == "fs" {
						if op, ok := step.ToolCall.Args["op"]; ok && op == "write" {
							step.FinishedAt = time.Now()
							task.Steps = append(task.Steps, step)
							task.Status = TaskCompleted
							fmt.Printf("  → fs write detected; marking task completed\n")
							return nil
						}
					}
				}
			}
		} else {
			step.Result = "No more tools needed"
		}

		step.FinishedAt = time.Now()
		task.Steps = append(task.Steps, step)

		// A "Blocked:" plan means the model tried a disallowed/invalid tool
		// call (e.g. wrong fs op, path not mentioned in task). Give the model
		// another chance to pick a different action instead of ending the
		// task early with no tool ever having run.
		if strings.HasPrefix(step.Plan, "Blocked:") {
			continue
		}

		// Stop if no tool was requested
		if step.ToolCall == nil || step.ToolCall.Name == "" || step.ToolCall.Name == "none" {
			task.Status = TaskCompleted
			return nil
		}
	}

	task.Status = TaskFailed
	return fmt.Errorf("reached max steps (%d)", MaxSteps)
}

// isRepeatToolCall returns true if the same tool+args was already used
func isRepeatToolCall(steps []*Step, current *ToolCall) bool {
	if current == nil {
		return false
	}
	for _, s := range steps {
		if s.ToolCall == nil {
			continue
		}
		if s.ToolCall.Name != current.Name {
			continue
		}
		// Compare args
		if len(s.ToolCall.Args) != len(current.Args) {
			continue
		}
		match := true
		for k, v := range current.Args {
			if s.ToolCall.Args[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
