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

const MaxSteps = 32

type Task struct {
	ID          string
	Description string
	CreatedAt   time.Time
	Status      TaskStatus
	Steps       []*Step
	Memory      memory.Memory
	RAG         memory.RAG
	Tools       tools.ToolRegistry
	Verdict     *tools.Verdict
	Label       string
	MaxSteps    int
	// RequireVerdict suppresses the fs-write-completes-task shortcut: the
	// task closes only on a verdict call or step exhaustion. The topology
	// runner sets it, since graph nodes report via verdict.
	RequireVerdict bool
}

type Step struct {
	Index      int
	Plan       string
	ToolCall   *ToolCall
	Verdict    *tools.Verdict
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
	maxSteps := MaxSteps
	if task.MaxSteps > 0 {
		maxSteps = task.MaxSteps
	}

	label := task.Label
	if label == "" {
		label = "task"
	}

	for i := 0; i < maxSteps; i++ {
		fmt.Printf("  → %s thinking… (step %d)\n", label, len(task.Steps)+1)
		step, err := e.Model.PlanNextStep(ctx, task)
		if err != nil {
			task.Status = TaskFailed
			return err
		}

		step.Index = len(task.Steps)
		step.StartedAt = time.Now()

		if step.ToolCall != nil && step.ToolCall.Name != "" && step.ToolCall.Name != "none" {
			// Detect consecutive repeated identical tool calls
			if isConsecutiveRepeat(task.Steps, step.ToolCall) {
				// Block this step and let the model continue: the "Blocked:"
				// plan is fed back to the model so it can pick a different
				// action instead of ending the task early.
				step.Plan = "Blocked: repeated the previous tool call (same tool + args); pick a different action"
				step.Result = "Blocked: same tool + args as the previous step."
				step.ToolCall = nil
				fmt.Println("  → Detected consecutive repeated tool call, blocked")
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

					// A successful fs write normally completes the task so the
					// model is not asked again. Exception: when the agent tool
					// is available, the write is usually one step of a larger
					// delegated workflow (e.g. the orchestrator writing plan.md
					// before handing off to sub-agents).
					if err == nil && step.ToolCall.Name == "fs" && !task.RequireVerdict && !hasAgentTool(task) {
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

		// A successful verdict call closes the task: record the verdict on the
		// step and the task, then stop. A rejected verdict (invalid status) is
		// not terminal so the model can retry.
		if step.ToolCall != nil && step.ToolCall.Name == "verdict" && step.Err == nil {
			v, _ := tools.ParseVerdict(step.ToolCall.Args)
			step.Verdict = &v
			task.Verdict = &v
			task.Status = TaskCompleted
			fmt.Printf(" -> verdict recorded (%s); marking task completed\n", v.Status)
			return nil
		}

		// A "Blocked:" plan means the model tried a disallowed/invalid tool
		// call (e.g. wrong fs op, path not mentioned in task), or the engine
		// blocked a consecutive repeat. Give the model another chance to pick
		// a different action instead of ending the task early with no tool
		// ever having run.
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
	task.Verdict = &tools.Verdict{Status: tools.VerdictFail, Reason: "reached max steps", Implicit: true}
	return fmt.Errorf("reached max steps (%d)", maxSteps)
}

// hasAgentTool reports whether the task's registry offers the agent
// tool, i.e. the model can delegate work to named sub-agents.
func hasAgentTool(task *Task) bool {
	_, ok := task.Tools.Get("agent")
	return ok
}

// isConsecutiveRepeat reports whether current is identical to the most
// recent executed tool call, i.e. the model asked for the same tool + args
// twice in a row. Calls separated by a different tool call (e.g. re-reading
// a file after delegating work) are not repeats and remain allowed.
func isConsecutiveRepeat(steps []*Step, current *ToolCall) bool {
	if current == nil {
		return false
	}
	for i := len(steps) - 1; i >= 0; i-- {
		prev := steps[i].ToolCall
		if prev == nil {
			continue
		}
		if prev.Name != current.Name || len(prev.Args) != len(current.Args) {
			return false
		}
		for k, v := range current.Args {
			if prev.Args[k] != v {
				return false
			}
		}
		return true
	}
	return false
}
