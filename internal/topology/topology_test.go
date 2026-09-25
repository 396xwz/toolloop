package topology

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeGraph(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "graph.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

// The example graph from research.md §7, verbatim.
const exampleYAML = `name: fix-issue-42
entry: plan
max_visits: 2

nodes:
  - id: plan
    role: planner
    max_steps: 8
    tools: [fs, shell]
  - id: build
    role: builder
    confirm: true
    max_steps: 32
    tools: [fs, shell, python]
  - id: review
    role: reviewer
    max_steps: 16
    tools: [fs, shell]

edges:
  - {from: plan, to: build, on: ok}
  - {from: build, to: review, on: ok}
  - {from: build, to: plan, on: fail}
  - {from: review, to: build, on: fail}
  - {from: review, to: done, on: ok}
`

func TestLoadValidGraph(t *testing.T) {
	g, err := Load(writeGraph(t, exampleYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if g.Name != "fix-issue-42" {
		t.Errorf("Name = %q, want %q", g.Name, "fix-issue-42")
	}
	if g.Entry != "plan" {
		t.Errorf("Entry = %q, want %q", g.Entry, "plan")
	}
	if g.MaxVisits != 2 {
		t.Errorf("MaxVisits = %d, want 2", g.MaxVisits)
	}
	if len(g.Nodes) != 3 {
		t.Fatalf("len(Nodes) = %d, want 3", len(g.Nodes))
	}
	if len(g.Edges) != 5 {
		t.Fatalf("len(Edges) = %d, want 5", len(g.Edges))
	}
	if !g.Nodes[1].Confirm {
		t.Errorf("Nodes[1].Confirm = false, want true")
	}
	if g.Nodes[1].MaxSteps != 32 {
		t.Errorf("Nodes[1].MaxSteps = %d, want 32", g.Nodes[1].MaxSteps)
	}
	wantTools := []string{"fs", "shell", "python"}
	if len(g.Nodes[1].Tools) != len(wantTools) {
		t.Fatalf("Nodes[1].Tools = %v, want %v", g.Nodes[1].Tools, wantTools)
	}
	for i := range wantTools {
		if g.Nodes[1].Tools[i] != wantTools[i] {
			t.Errorf("Nodes[1].Tools = %v, want %v", g.Nodes[1].Tools, wantTools)
			break
		}
	}
	if g.Nodes[0].MaxSteps != 8 {
		t.Errorf("Nodes[0].MaxSteps = %d, want 8", g.Nodes[0].MaxSteps)
	}
	if g.Edges[4].To != "done" {
		t.Errorf("Edges[4].To = %q, want %q", g.Edges[4].To, "done")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/graph.yaml"); err == nil {
		t.Fatal("Load: want error, got nil")
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	if _, err := Load(writeGraph(t, "nodes: [unclosed")); err == nil {
		t.Fatal("Load: want error, got nil")
	}
}

func TestLoadValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr error
	}{
		{
			name: "missing entry",
			yaml: `name: g
nodes:
  - id: a
    role: planner
`,
			wantErr: ErrNoEntry,
		},
		{
			name: "entry not a node id",
			yaml: `entry: ghost
nodes:
  - id: a
    role: planner
`,
			wantErr: ErrNoEntry,
		},
		{
			name: "edge to undeclared node",
			yaml: `entry: a
nodes:
  - id: a
    role: planner
edges:
  - {from: a, to: b, on: ok}
`,
			wantErr: ErrUnknownEndpoint,
		},
		{
			name: "edge from undeclared node",
			yaml: `entry: a
nodes:
  - id: a
    role: planner
edges:
  - {from: ghost, to: a, on: ok}
`,
			wantErr: ErrUnknownEndpoint,
		},
		{
			name: "edge from terminal",
			yaml: `entry: a
nodes:
  - id: a
    role: planner
edges:
  - {from: done, to: a, on: ok}
`,
			wantErr: ErrUnknownEndpoint,
		},
		{
			name: "unreachable node",
			yaml: `entry: a
nodes:
  - id: a
    role: planner
  - id: b
    role: builder
`,
			wantErr: ErrUnreachableNode,
		},
		{
			name: "duplicate node id",
			yaml: `entry: a
nodes:
  - id: a
    role: planner
  - id: a
    role: builder
`,
			wantErr: ErrDuplicateNodeID,
		},
		{
			name: "empty node id",
			yaml: `entry: a
nodes:
  - id: a
    role: planner
  - role: builder
`,
			wantErr: ErrEmptyNodeID,
		},
		{
			name: "bad on value",
			yaml: `entry: a
nodes:
  - id: a
    role: planner
edges:
  - {from: a, to: a, on: maybe}
`,
			wantErr: ErrBadOnValue,
		},
		{
			name: "unbounded cycle",
			yaml: `entry: a
nodes:
  - id: a
    role: planner
  - id: b
    role: builder
edges:
  - {from: a, to: b, on: ok}
  - {from: b, to: a, on: fail}
`,
			wantErr: ErrUnboundedCycle,
		},
		{
			name: "declared terminal node id",
			yaml: `entry: plan
nodes:
  - id: plan
    role: planner
  - id: done
    role: builder
`,
			wantErr: ErrTerminalNodeID,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeGraph(t, tc.yaml)); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Load: want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadCycleWithCapAccepted(t *testing.T) {
	yaml := `entry: a
max_visits: 2
nodes:
  - id: a
    role: planner
  - id: b
    role: builder
edges:
  - {from: a, to: b, on: ok}
  - {from: b, to: a, on: fail}
`
	if _, err := Load(writeGraph(t, yaml)); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestValidateRoles(t *testing.T) {
	g := &Graph{
		Entry: "a",
		Nodes: []Node{{ID: "a", Role: "builder"}},
	}
	if err := g.ValidateRoles([]string{"planner"}); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("ValidateRoles: want %v, got %v", ErrUnknownRole, err)
	}
	empty := &Graph{Nodes: []Node{{ID: "a"}}}
	if err := empty.ValidateRoles([]string{"planner"}); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("ValidateRoles: empty role, want %v, got %v", ErrUnknownRole, err)
	}
	if err := g.ValidateRoles([]string{"builder"}); err != nil {
		t.Fatalf("ValidateRoles: %v", err)
	}
}

func TestValidateTools(t *testing.T) {
	g := &Graph{
		Entry: "a",
		Nodes: []Node{{ID: "a", Role: "builder", Tools: []string{"shell"}}},
	}
	if err := g.ValidateTools([]string{"fs"}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("ValidateTools: want %v, got %v", ErrUnknownTool, err)
	}
	if err := g.ValidateTools([]string{"fs", "shell"}); err != nil {
		t.Fatalf("ValidateTools: %v", err)
	}
}
