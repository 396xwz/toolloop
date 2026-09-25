package topology

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/tools"
)

// scriptModel is a scripted engine.ChatModel: PlanNextStep pops steps from
// the script in order; an exhausted script yields a plain-text step which
// terminates the task without a verdict. fail, when set, is returned once
// by PlanNextStep to simulate a model/transport failure.
type scriptModel struct {
	script []*engine.Step
	i      int
	fail   error
	prompt string
	tasks  []*engine.Task
}

func (m *scriptModel) PlanNextStep(ctx context.Context, task *engine.Task) (*engine.Step, error) {
	if m.fail != nil {
		err := m.fail
		m.fail = nil
		return nil, err
	}
	m.tasks = append(m.tasks, task)
	if m.i < len(m.script) {
		m.i++
		return m.script[m.i-1], nil
	}
	return &engine.Step{Plan: "done"}, nil
}

func (m *scriptModel) GenerateFinalAnswer(ctx context.Context, task *engine.Task) (string, error) {
	return "", nil
}

func (m *scriptModel) ModelName() string { return "script" }

func (m *scriptModel) SetModel(name string) {}

func (m *scriptModel) SystemPromptValue() string { return m.prompt }

func (m *scriptModel) SetSystemPrompt(prompt string) { m.prompt = prompt }

func verdictStep(status, reason string) *engine.Step {
	return &engine.Step{
		Plan: "closing",
		ToolCall: &engine.ToolCall{
			Name: "verdict",
			Args: map[string]string{"status": status, "reason": reason},
		},
	}
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

// TestRunLinearOK walks a -> b(ok) -> done(always); both nodes verdict ok.
func TestRunLinearOK(t *testing.T) {
	g := &Graph{
		Name:  "linear",
		Entry: "a",
		Nodes: []Node{
			{ID: "a", Role: "r1", Tools: []string{}},
			{ID: "b", Role: "r2", Tools: []string{}},
		},
		Edges: []Edge{
			{From: "a", To: "b", On: "ok"},
			{From: "b", To: Terminal, On: "always"},
		},
	}
	model := &scriptModel{script: []*engine.Step{
		verdictStep("ok", "a done"),
		verdictStep("ok", "b done"),
	}}
	reg := testRegistry()
	reg.Register("verdict", tools.VerdictTool{})
	roles := map[string]string{"r1": "prompt-1", "r2": "prompt-2"}

	report, err := Run(context.Background(), g, "task text", model, reg, nil, nil, roles)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q, want ok (reason %q)", report.Status, report.Reason)
	}
	if report.Reason != "b done" {
		t.Fatalf("reason = %q, want %q", report.Reason, "b done")
	}
	if len(report.Nodes) != 2 {
		t.Fatalf("len(Nodes) = %d, want 2", len(report.Nodes))
	}
	if report.Nodes[0].ID != "a" || report.Nodes[1].ID != "b" {
		t.Fatalf("node ids = %q, %q; want a, b", report.Nodes[0].ID, report.Nodes[1].ID)
	}
	if report.Nodes[0].Verdict != "ok" || report.Nodes[1].Verdict != "ok" {
		t.Fatalf("verdicts = %q, %q; want ok, ok", report.Nodes[0].Verdict, report.Nodes[1].Verdict)
	}
	if report.Nodes[0].Steps != 1 || report.Nodes[1].Steps != 1 {
		t.Fatalf("steps = %d, %d; want 1, 1", report.Nodes[0].Steps, report.Nodes[1].Steps)
	}
	// Blackboard: node b must see node a's verdict in its task description.
	if len(model.tasks) != 2 {
		t.Fatalf("model saw %d tasks, want 2", len(model.tasks))
	}
	bDesc := model.tasks[1].Description
	if !strings.Contains(bDesc, "Context from previous nodes") {
		t.Fatalf("node b description missing blackboard:\n%s", bDesc)
	}
	if !strings.Contains(bDesc, "From node 'a' (verdict: ok - a done)") {
		t.Fatalf("node b description missing node a line:\n%s", bDesc)
	}
	if model.prompt != "" {
		t.Fatalf("system prompt not restored after Run: %q", model.prompt)
	}
}

// TestRunBranchOnFail: node a verdicts fail; the fail edge goes to b, the ok
// edge to c. The walk must visit b, not c.
func TestRunBranchOnFail(t *testing.T) {
	g := &Graph{
		Name:  "branch",
		Entry: "a",
		Nodes: []Node{
			{ID: "a", Role: "r1", Tools: []string{}},
			{ID: "b", Role: "r2", Tools: []string{}},
			{ID: "c", Role: "r3", Tools: []string{}},
		},
		Edges: []Edge{
			{From: "a", To: "b", On: "fail"},
			{From: "a", To: "c", On: "ok"},
			{From: "b", To: Terminal, On: "always"},
		},
	}
	model := &scriptModel{script: []*engine.Step{
		verdictStep("fail", "a broke"),
		verdictStep("ok", "b recovered"),
	}}
	reg := testRegistry()
	reg.Register("verdict", tools.VerdictTool{})
	roles := map[string]string{"r1": "p1", "r2": "p2", "r3": "p3"}

	report, err := Run(context.Background(), g, "task", model, reg, nil, nil, roles)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q, want ok (reason %q)", report.Status, report.Reason)
	}
	if len(report.Nodes) != 2 {
		t.Fatalf("len(Nodes) = %d, want 2", len(report.Nodes))
	}
	if report.Nodes[0].ID != "a" || report.Nodes[0].Verdict != "fail" {
		t.Fatalf("first node = %q/%q, want a/fail", report.Nodes[0].ID, report.Nodes[0].Verdict)
	}
	if report.Nodes[1].ID != "b" {
		t.Fatalf("second node = %q, want b (fail edge)", report.Nodes[1].ID)
	}
}

// TestRunMaxVisits: a -> b(ok) -> b -> a(always) cycle with MaxVisits = 2.
// The walk must stop when node a is about to be visited a third time.
func TestRunMaxVisits(t *testing.T) {
	g := &Graph{
		Name:      "cycle",
		Entry:     "a",
		MaxVisits: 2,
		Nodes: []Node{
			{ID: "a", Role: "r1", Tools: []string{}},
			{ID: "b", Role: "r2", Tools: []string{}},
		},
		Edges: []Edge{
			{From: "a", To: "b", On: "ok"},
			{From: "b", To: "a", On: "always"},
		},
	}
	model := &scriptModel{script: []*engine.Step{
		verdictStep("ok", "a1"),
		verdictStep("ok", "b1"),
		verdictStep("ok", "a2"),
		verdictStep("ok", "b2"),
	}}
	reg := testRegistry()
	reg.Register("verdict", tools.VerdictTool{})
	roles := map[string]string{"r1": "p1", "r2": "p2"}

	report, err := Run(context.Background(), g, "task", model, reg, nil, nil, roles)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail (reason %q)", report.Status, report.Reason)
	}
	want := `max_visits (2) exceeded for node "a"`
	if !strings.Contains(report.Reason, want) {
		t.Fatalf("reason = %q, want it to contain %q", report.Reason, want)
	}
	if len(report.Nodes) != 4 {
		t.Fatalf("len(Nodes) = %d, want 4 (a, b, a, b)", len(report.Nodes))
	}
	if report.Nodes[0].ID != "a" || report.Nodes[1].ID != "b" ||
		report.Nodes[2].ID != "a" || report.Nodes[3].ID != "b" {
		ids := make([]string, len(report.Nodes))
		for i, n := range report.Nodes {
			ids[i] = n.ID
		}
		t.Fatalf("node ids = %v, want [a b a b]", ids)
	}
}

// TestRunMissingVerdict: node a's script runs out, so the engine completes
// the task with no verdict. The runner records an implicit fail, and the
// always edge still routes to b. The walk ends with b's verdict.
func TestRunMissingVerdict(t *testing.T) {
	g := &Graph{
		Name:  "missing-verdict",
		Entry: "a",
		Nodes: []Node{
			{ID: "a", Role: "r1", Tools: []string{}},
			{ID: "b", Role: "r2", Tools: []string{}},
		},
		Edges: []Edge{
			{From: "a", To: "b", On: "always"},
			{From: "b", To: Terminal, On: "always"},
		},
	}
	model := &scriptModel{script: []*engine.Step{
		{Plan: "no tools left"},
		verdictStep("ok", "b fine"),
	}}
	reg := testRegistry()
	reg.Register("verdict", tools.VerdictTool{})
	roles := map[string]string{"r1": "p1", "r2": "p2"}

	report, err := Run(context.Background(), g, "task", model, reg, nil, nil, roles)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Nodes) != 2 {
		t.Fatalf("len(Nodes) = %d, want 2 (implicit fail must not abort the walk)", len(report.Nodes))
	}
	if report.Nodes[0].ID != "a" || report.Nodes[0].Verdict != "fail" {
		t.Fatalf("first node = %q/%q, want a/fail", report.Nodes[0].ID, report.Nodes[0].Verdict)
	}
	if report.Nodes[0].Reason != "no verdict recorded" {
		t.Fatalf("first node reason = %q, want %q", report.Nodes[0].Reason, "no verdict recorded")
	}

	if report.Status != "ok" || report.Reason != "b fine" {
		t.Fatalf("status = %q reason = %q; want ok / b fine", report.Status, report.Reason)
	}
	// Node b's description carries a's implicit-fail line.
	if !strings.Contains(model.tasks[1].Description, "From node 'a' (verdict: fail - no verdict recorded)") {
		t.Fatalf("node b description missing node a line:\n%s", model.tasks[1].Description)
	}
}

// TestRunErrorRoutesFailEdge: the model fails once on node a; the runner
// must turn that into a fail verdict and route along the fail edge to b.
func TestRunErrorRoutesFailEdge(t *testing.T) {
	g := &Graph{
		Name:  "error-route",
		Entry: "a",
		Nodes: []Node{
			{ID: "a", Role: "r1", Tools: []string{}},
			{ID: "b", Role: "r2", Tools: []string{}},
			{ID: "c", Role: "r3", Tools: []string{}},
		},
		Edges: []Edge{{From: "a", To: "b", On: "fail"}, {From: "a", To: "c", On: "ok"}},
	}
	model := &scriptModel{
		fail:   errors.New("model exploded"),
		script: []*engine.Step{verdictStep("ok", "b done")},
	}
	reg := testRegistry()
	reg.Register("verdict", tools.VerdictTool{})
	roles := map[string]string{"r1": "p1", "r2": "p2", "r3": "p3"}

	report, err := Run(context.Background(), g, "task", model, reg, nil, nil, roles)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Nodes) != 2 {
		t.Fatalf("len(Nodes) = %d, want 2", len(report.Nodes))
	}
	if report.Nodes[0].ID != "a" || report.Nodes[0].Verdict != "fail" {
		t.Fatalf("first node = %q/%q, want a/fail", report.Nodes[0].ID, report.Nodes[0].Verdict)
	}
	if !strings.Contains(report.Nodes[0].Reason, "model exploded") {
		t.Fatalf("first node reason = %q, want it to contain %q", report.Nodes[0].Reason, "model exploded")
	}
	if report.Nodes[1].ID != "b" {
		t.Fatalf("second node = %q, want b (fail edge)", report.Nodes[1].ID)
	}
	if report.Status != "ok" || report.Reason != "b done" {
		t.Fatalf("status = %q reason = %q; want ok / b done", report.Status, report.Reason)
	}
}
