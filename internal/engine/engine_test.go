package engine

import (
	"context"
	"fmt"
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

func TestVerdictOKClosesTask(t *testing.T) {
	reg := testRegistry("fs")
	reg.Register("verdict", tools.VerdictTool{})
	model := &scriptModel{script: []*Step{
		{Plan: "close", ToolCall: &ToolCall{Name: "verdict", Args: map[string]string{"status": "ok", "reason": "done"}}},
	}}
	task := &Task{ID: "t-verdict-ok", Status: TaskPending, Steps: []*Step{}, Tools: reg}
	if err := (&Engine{Model: model}).RunTask(context.Background(), task); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if task.Status != TaskCompleted {
		t.Fatalf("status = %q, want completed", task.Status)
	}
	if len(task.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(task.Steps))
	}
	if task.Verdict == nil {
		t.Fatal("task.Verdict is nil")
	}
	if task.Verdict.Status != tools.VerdictOK {
		t.Errorf("verdict status = %q, want ok", task.Verdict.Status)
	}
	if task.Verdict.Reason != "done" {
		t.Errorf("verdict reason = %q, want done", task.Verdict.Reason)
	}
	if task.Verdict.Implicit {
		t.Error("verdict is implicit, want explicit")
	}
	if task.Steps[0].Verdict == nil {
		t.Error("step verdict is nil")
	}
}

func TestInvalidVerdictStatusIsNotTerminal(t *testing.T) {
	reg := testRegistry("fs")
	reg.Register("verdict", tools.VerdictTool{})
	model := &scriptModel{script: []*Step{
		{Plan: "bad", ToolCall: &ToolCall{Name: "verdict", Args: map[string]string{"status": "weird"}}},
		{Plan: "good", ToolCall: &ToolCall{Name: "verdict", Args: map[string]string{"status": "ok", "reason": "fixed"}}},
	}}
	task := &Task{ID: "t-verdict-invalid", Status: TaskPending, Steps: []*Step{}, Tools: reg}
	if err := (&Engine{Model: model}).RunTask(context.Background(), task); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if task.Steps[0].Err == nil {
		t.Error("step 0 error is nil, want parse error")
	}
	if task.Steps[0].Verdict != nil {
		t.Error("step 0 verdict set, want nil")
	}
	if len(task.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(task.Steps))
	}
	if task.Status != TaskCompleted {
		t.Fatalf("status = %q, want completed", task.Status)
	}
	if task.Verdict == nil || task.Verdict.Status != tools.VerdictOK {
		t.Fatalf("verdict = %+v, want ok", task.Verdict)
	}
}

func TestExhaustionYieldsImplicitFail(t *testing.T) {
	script := make([]*Step, 0, MaxSteps)
	for i := range MaxSteps {
		script = append(script, &Step{
			Plan:     fmt.Sprintf("read f%d", i),
			ToolCall: &ToolCall{Name: "fs", Args: map[string]string{"op": "read", "path": fmt.Sprintf("f%d.txt", i)}},
		})
	}
	reg := testRegistry("fs")
	model := &scriptModel{script: script}
	task := &Task{ID: "t-exhaust", Status: TaskPending, Steps: []*Step{}, Tools: reg}
	if err := (&Engine{Model: model}).RunTask(context.Background(), task); err == nil {
		t.Fatal("expected exhaustion error")
	}
	if task.Status != TaskFailed {
		t.Fatalf("status = %q, want failed", task.Status)
	}
	if task.Verdict == nil {
		t.Fatal("task.Verdict is nil")
	}
	if task.Verdict.Status != tools.VerdictFail {
		t.Errorf("verdict status = %q, want fail", task.Verdict.Status)
	}
	if !task.Verdict.Implicit {
		t.Error("verdict is not implicit")
	}
	if task.Verdict.Reason != "reached max steps" {
		t.Errorf("verdict reason = %q, want 'reached max steps'", task.Verdict.Reason)
	}
	if len(task.Steps) != MaxSteps {
		t.Errorf("steps = %d, want %d", len(task.Steps), MaxSteps)
	}
}

func TestVerdictWinsOverFSWrite(t *testing.T) {
	reg := testRegistry("fs")
	reg.Register("verdict", tools.VerdictTool{})
	model := &scriptModel{script: []*Step{
		{Plan: "close", ToolCall: &ToolCall{Name: "verdict", Args: map[string]string{"status": "ok"}}},
		{Plan: "write", ToolCall: &ToolCall{Name: "fs", Args: map[string]string{"op": "write", "path": "x.py", "content": "x"}}},
	}}
	task := &Task{ID: "t-verdict-fs", Status: TaskPending, Steps: []*Step{}, Tools: reg}
	if err := (&Engine{Model: model}).RunTask(context.Background(), task); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if len(task.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(task.Steps))
	}
	if task.Status != TaskCompleted {
		t.Fatalf("status = %q, want completed", task.Status)
	}
	if task.Verdict == nil || task.Verdict.Status != tools.VerdictOK {
		t.Fatalf("verdict = %+v, want ok", task.Verdict)
	}
}

func TestVerdictAfterBlockedRepeat(t *testing.T) {
	reg := testRegistry("fs")
	reg.Register("verdict", tools.VerdictTool{})
	model := &scriptModel{script: []*Step{
		{Plan: "list", ToolCall: &ToolCall{Name: "fs", Args: map[string]string{"op": "list", "path": "."}}},
		{Plan: "list again", ToolCall: &ToolCall{Name: "fs", Args: map[string]string{"op": "list", "path": "."}}},
		{Plan: "give up", ToolCall: &ToolCall{Name: "verdict", Args: map[string]string{"status": "fail", "reason": "stuck"}}},
	}}
	task := &Task{ID: "t-verdict-blocked", Status: TaskPending, Steps: []*Step{}, Tools: reg}
	if err := (&Engine{Model: model}).RunTask(context.Background(), task); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if task.Status != TaskCompleted {
		t.Fatalf("status = %q, want completed", task.Status)
	}
	if len(task.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(task.Steps))
	}
	if !strings.HasPrefix(task.Steps[1].Plan, "Blocked:") {
		t.Errorf("step 1 plan = %q, want Blocked: prefix", task.Steps[1].Plan)
	}
	if task.Steps[2].Verdict == nil {
		t.Error("step 2 verdict is nil")
	}
	if task.Verdict == nil || task.Verdict.Status != tools.VerdictFail {
		t.Fatalf("verdict = %+v, want fail", task.Verdict)
	}
}
