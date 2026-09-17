package main

import (
	"context"
	"strings"
	"testing"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/tools"
)

// fakeChatModel implements engine.ChatModel with a scripted step sequence so
// tests can drive the exact tool calls a task loop should see. The same
// instance may serve nested runAgentTask loops: script entries are consumed
// in call order.
type fakeChatModel struct {
	prompt    string
	script    []*engine.Step // one entry per PlanNextStep; nil ends the loop
	final     string
	planned   int
	systemLog []string
}

func (f *fakeChatModel) PlanNextStep(ctx context.Context, task *engine.Task) (*engine.Step, error) {
	f.systemLog = append(f.systemLog, f.prompt)
	if f.planned >= len(f.script) || f.script[f.planned] == nil {
		return &engine.Step{Plan: "done"}, nil
	}
	step := f.script[f.planned]
	f.planned++
	return step, nil
}

func (f *fakeChatModel) GenerateFinalAnswer(ctx context.Context, task *engine.Task) (string, error) {
	return f.final, nil
}

func (f *fakeChatModel) ModelName() string         { return "fake" }
func (f *fakeChatModel) SetModel(string)           {}
func (f *fakeChatModel) SystemPromptValue() string { return f.prompt }
func (f *fakeChatModel) SetSystemPrompt(s string)  { f.prompt = s }

func newAgentToolForTest(t *testing.T, model engine.ChatModel) (*agentTool, *agentManager, tools.ToolRegistry) {
	t.Helper()
	mgr := newAgentManager("main system prompt")
	registry := tools.NewToolRegistry()
	tool := agentTool{mgr: mgr, model: model, registry: registry, mem: nil, rag: nil}
	registry.Register("agent", tool)
	return &tool, mgr, registry
}

func TestAgentToolDelegatesToNamedAgent(t *testing.T) {
	model := &fakeChatModel{
		prompt: "main system prompt",
		script: []*engine.Step{
			{Plan: "delegate", ToolCall: &engine.ToolCall{Name: "agent", Args: map[string]string{"name": "tester", "task": "run the tests"}}},
			nil, // inner (tester) loop: no tools needed
			nil, // outer loop resumes: no tools needed
		},
		final: "tests pass",
	}
	_, mgr, registry := newAgentToolForTest(t, model)
	tester, err := mgr.create("tester", "You are the tester.")
	if err != nil {
		t.Fatalf("create tester: %v", err)
	}

	// Drive the same entry point the REPL uses: the main agent's task loop
	// consumes the delegation step, the tester's loop consumes the first nil,
	// and main resumes on the second.
	answer, err := runAgentTask(context.Background(), mgr, model, registry, nil, nil, mgr.current(), "make the tests green")
	if err != nil {
		t.Fatalf("runAgentTask: %v", err)
	}
	if answer != "tests pass" {
		t.Fatalf("answer = %q, want %q", answer, "tests pass")
	}
	if tester.Runs != 1 {
		t.Fatalf("tester.Runs = %d, want 1", tester.Runs)
	}
	notes := tester.notesString()
	if !strings.Contains(notes, "run the tests") || !strings.Contains(notes, "tests pass") {
		t.Fatalf("tester notes missing task or result: %q", notes)
	}
	if got := model.SystemPromptValue(); got != "main system prompt" {
		t.Fatalf("system prompt not restored: %q", got)
	}
	wantLog := []string{"main system prompt", "You are the tester.", "main system prompt"}
	if len(model.systemLog) != len(wantLog) {
		t.Fatalf("PlanNextStep calls = %d, want %d (log: %v)", len(model.systemLog), len(wantLog), model.systemLog)
	}
	for i, want := range wantLog {
		if model.systemLog[i] != want {
			t.Fatalf("call %d ran with prompt %q, want %q", i, model.systemLog[i], want)
		}
	}
}

func TestAgentToolRequiresNameAndTask(t *testing.T) {
	model := &fakeChatModel{prompt: "main"}
	tool, _, _ := newAgentToolForTest(t, model)

	if _, err := tool.Execute(context.Background(), map[string]string{"name": "", "task": "x"}); err == nil {
		t.Fatal("empty name: expected error, got nil")
	}
	if _, err := tool.Execute(context.Background(), map[string]string{"name": "main", "task": "  "}); err == nil {
		t.Fatal("empty task: expected error, got nil")
	}
}

func TestAgentToolUnknownAgent(t *testing.T) {
	model := &fakeChatModel{prompt: "main"}
	tool, _, _ := newAgentToolForTest(t, model)

	_, err := tool.Execute(context.Background(), map[string]string{"name": "ghost", "task": "do it"})
	if err == nil {
		t.Fatal("unknown agent: expected error, got nil")
	}
	if !strings.Contains(err.Error(), `unknown agent "ghost"`) {
		t.Fatalf("error = %q, want it to name the unknown agent", err)
	}
}

func TestAgentToolEnforcesDepthLimit(t *testing.T) {
	model := &fakeChatModel{prompt: "main"}
	tool, mgr, _ := newAgentToolForTest(t, model)
	top := mgr.current()
	for i := 0; i < maxAgentDepth; i++ {
		mgr.pushRunning(top)
	}
	defer func() {
		for len(mgr.running) > 0 {
			mgr.popRunning()
		}
	}()

	_, err := tool.Execute(context.Background(), map[string]string{"name": "main", "task": "do it"})
	if err == nil {
		t.Fatal("depth limit: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("error = %q, want depth-limit message", err)
	}
}

// resultTool is a registry tool that always returns a fixed output.
type resultTool struct {
	name string
	out  string
}

func (r resultTool) Name() string { return r.name }

func (r resultTool) Execute(ctx context.Context, args map[string]string) (string, error) {
	return r.out, nil
}

// A successful fs write must not terminate a task that can still delegate
// work: when the agent tool is available, the write is one step of a larger
// workflow (the orchestrator writes plan.md, then hands off to sub-agents).
func TestFsWriteContinuesWhenAgentToolAvailable(t *testing.T) {
	model := &fakeChatModel{
		script: []*engine.Step{
			{Plan: "write plan", ToolCall: &engine.ToolCall{Name: "fs", Args: map[string]string{"op": "write", "path": "/tmp/plan.md", "content": "plan"}}},
			{Plan: "delegate", ToolCall: &engine.ToolCall{Name: "agent", Args: map[string]string{"name": "tester", "task": "run the tests"}}},
			nil,
		},
		final: "done",
	}
	registry := tools.NewToolRegistry()
	registry.Register("fs", resultTool{name: "fs", out: "written"})
	registry.Register("agent", resultTool{name: "agent", out: "tests pass"})

	task := &engine.Task{
		ID:      "t-fs",
		Status:  engine.TaskPending,
		Steps:   []*engine.Step{},
		Tools:   registry,
	}
	if err := (&engine.Engine{Model: model}).RunTask(context.Background(), task); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if task.Status != engine.TaskCompleted {
		t.Fatalf("status = %s, want completed", task.Status)
	}
	if len(task.Steps) != 3 {
		t.Fatalf("steps = %d, want 3 (fs write must not end a delegating workflow)", len(task.Steps))
	}
	if got := task.Steps[1].ToolCall.Output; got != "tests pass" {
		t.Fatalf("agent step output = %q, want %q", got, "tests pass")
	}
}

// Without the agent tool (batch mode), a successful fs write still
// completes the task immediately so the model is not asked again.
func TestFsWriteCompletesWithoutAgentTool(t *testing.T) {
	model := &fakeChatModel{
		script: []*engine.Step{
			{Plan: "write plan", ToolCall: &engine.ToolCall{Name: "fs", Args: map[string]string{"op": "write", "path": "/tmp/plan.md", "content": "plan"}}},
			{Plan: "list", ToolCall: &engine.ToolCall{Name: "shell", Args: map[string]string{"command": "ls"}}},
		},
		final: "done",
	}
	registry := tools.NewToolRegistry()
	registry.Register("fs", resultTool{name: "fs", out: "written"})
	registry.Register("shell", resultTool{name: "shell", out: "ok"})

	task := &engine.Task{
		ID:      "t-batch",
		Status:  engine.TaskPending,
		Steps:   []*engine.Step{},
		Tools:   registry,
	}
	if err := (&engine.Engine{Model: model}).RunTask(context.Background(), task); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if task.Status != engine.TaskCompleted {
		t.Fatalf("status = %s, want completed", task.Status)
	}
	if len(task.Steps) != 1 {
		t.Fatalf("steps = %d, want 1 (fs write still completes non-delegating tasks)", len(task.Steps))
	}
}
