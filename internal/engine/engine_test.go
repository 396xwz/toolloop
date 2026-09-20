package engine

import (
        "context"
        "strings"
        "testing"

        "github.com/396xwz/toolloop/internal/tools"
)

type scriptModel struct {
        script []*Step
        i      int
}

func (m *scriptModel) PlanNextStep(ctx context.Context, task *Task) (*Step, error) {
        if m.i >= len(m.script) || m.script[m.i] == nil {
                return &Step{Plan: "done"}, nil
        }
        step := m.script[m.i]
        m.i++
        return step, nil
}

type stubTool struct{ name string }

func (s stubTool) Name() string { return s.name }

func (s stubTool) Execute(ctx context.Context, args map[string]string) (string, error) {
        return "ok:" + s.name, nil
}

func testRegistry(names ...string) tools.ToolRegistry {
        reg := tools.NewToolRegistry()
        for _, n := range names {
                reg.Register(n, stubTool{name: n})
        }
        return reg
}

func TestNonConsecutiveRereadIsAllowed(t *testing.T) {
        readPlan := &ToolCall{Name: "fs", Args: map[string]string{"op": "read", "path": "plan.md"}}
        model := &scriptModel{script: []*Step{
                {Plan: "read", ToolCall: readPlan},
                {Plan: "delegate", ToolCall: &ToolCall{Name: "agent", Args: map[string]string{"name": "builder", "task": "write files"}}},
                {Plan: "reread", ToolCall: &ToolCall{Name: "fs", Args: map[string]string{"op": "read", "path": "plan.md"}}},
                nil,
        }}
        reg := testRegistry("fs", "agent")
        task := &Task{ID: "t-reread", Status: TaskPending, Steps: []*Step{}, Tools: reg}
        if err := (&Engine{Model: model}).RunTask(context.Background(), task); err != nil {
                t.Fatalf("RunTask: %v", err)
        }
        if task.Status != TaskCompleted {
                t.Fatalf("status = %s, want completed", task.Status)
        }
        if len(task.Steps) != 4 {
                t.Fatalf("steps = %d, want 4 (read, agent, reread, done); last plan=%q", len(task.Steps), task.Steps[len(task.Steps)-1].Plan)
        }
        if task.Steps[2].ToolCall == nil || task.Steps[2].ToolCall.Name != "fs" {
                t.Fatalf("third step should execute the re-read, got %#v plan=%q", task.Steps[2].ToolCall, task.Steps[2].Plan)
        }
        if !strings.HasPrefix(task.Steps[2].Result, "ok:") {
                t.Fatalf("re-read result = %q, want executed tool output", task.Steps[2].Result)
        }
}

func TestConsecutiveRepeatIsBlockedAndTaskContinues(t *testing.T) {
        list := &ToolCall{Name: "fs", Args: map[string]string{"op": "list", "path": "."}}
        model := &scriptModel{script: []*Step{
                {Plan: "list", ToolCall: list},
                {Plan: "list again", ToolCall: &ToolCall{Name: "fs", Args: map[string]string{"op": "list", "path": "."}}},
                {Plan: "write", ToolCall: &ToolCall{Name: "fs", Args: map[string]string{"op": "write", "path": "x.py", "content": "x"}}},
                nil,
        }}
        reg := testRegistry("fs", "agent")
        task := &Task{ID: "t-repeat", Status: TaskPending, Steps: []*Step{}, Tools: reg}
        if err := (&Engine{Model: model}).RunTask(context.Background(), task); err != nil {
                t.Fatalf("RunTask: %v", err)
        }
        if task.Status != TaskCompleted {
                t.Fatalf("status = %s, want completed", task.Status)
        }
        if len(task.Steps) != 4 {
                t.Fatalf("steps = %d, want 4 (list, blocked, write, done)", len(task.Steps))
        }
        if !strings.HasPrefix(task.Steps[1].Plan, "Blocked:") {
                t.Fatalf("second step plan = %q, want Blocked:", task.Steps[1].Plan)
        }
        if task.Steps[1].ToolCall != nil {
                t.Fatalf("blocked step should not keep a ToolCall, got %#v", task.Steps[1].ToolCall)
        }
        if task.Steps[2].ToolCall == nil || task.Steps[2].ToolCall.Args["op"] != "write" {
                t.Fatalf("third step should write, got %#v", task.Steps[2].ToolCall)
        }
}
