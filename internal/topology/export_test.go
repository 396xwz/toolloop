package topology

import (
	"strings"
	"testing"
	"time"
)

// visit builds one NodeReport with the given duration.
func visit(id, verdict string, d time.Duration) NodeReport {
	start := time.Now()
	return NodeReport{
		ID:         id,
		Verdict:    verdict,
		StartedAt:  start,
		FinishedAt: start.Add(d),
	}
}

// exportGraph is the fixture used by the export tests:
//
//	a -ok-> b, b -fail-> a, b -ok-> d, b -always-> done; node c is isolated.
//
// Walked: a(ok 500ms) -> b(fail 500ms) -> a(ok 1s) -> b(ok 200ms) ->
// d(ok 250ms). So a: ok x2 1.5s, b: ok x2 700ms, d: ok x1 250ms, c idle.
func exportGraph() *Graph {
	return &Graph{
		Name:  "fixture",
		Entry: "a",
		Nodes: []Node{
			{ID: "a", Role: "dev"},
			{ID: "b", Role: "qa"},
			{ID: "c", Role: "dev"},
			{ID: "d", Role: "qa"},
		},
		Edges: []Edge{
			{From: "a", To: "b", On: "ok"},
			{From: "b", To: "a", On: "fail"},
			{From: "b", To: "d", On: "ok"},
			{From: "b", To: Terminal, On: "always"},
		},
	}
}

func exportReport() *Report {
	return &Report{
		Status: "ok",
		Nodes: []NodeReport{
			visit("a", "ok", 500*time.Millisecond),
			visit("b", "fail", 500*time.Millisecond),
			visit("a", "ok", time.Second),
			visit("b", "ok", 200*time.Millisecond),
			visit("d", "ok", 250*time.Millisecond),
		},
	}
}

func TestDOT(t *testing.T) {
	out := DOT(exportGraph(), exportReport())
	if !strings.HasPrefix(out, "digraph topology {") {
		t.Fatalf("missing digraph header:\n%s", out)
	}
	for _, want := range []string{
		"rankdir=LR",
		// a: ok x2, 1.5s total, green
		"\nok x2", "\n1.5s", fillOK,
		// b: last verdict ok x2, 700ms, green
		"\nok x2", "\n700ms",
		// d: single 250ms visit
		"\nok x1", "250ms",
		// c: never visited -> bare label, no verdict, idle fill
		`c [label="c", fillcolor="#ffffff"];`,
		// terminal doublecircle only because an edge targets done
		`done [label="done", shape=doublecircle, style=filled, fillcolor="#eeeeee"];`,
		// traversed edges are thick
		"a -> b [label=\"ok\", penwidth=2.0];",
		"b -> a [label=\"fail\", penwidth=2.0];",
		"b -> d [label=\"ok\", penwidth=2.0];",
		// not traversed
		"b -> done [label=\"always\"];",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DOT missing %q:\n%s", want, out)
		}
	}
}

func TestDOTNilReport(t *testing.T) {
	out := DOT(exportGraph(), nil)
	for _, want := range []string{
		`a [label="a", fillcolor="#ffffff"];`,
		"b [label=\"b\", fillcolor=\"#ffffff\"]",
		`done [label="done", shape=doublecircle`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DOT(nil report) missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "penwidth") {
		t.Errorf("nil report must not mark edges taken:\n%s", out)
	}
}

func TestDOTEscapes(t *testing.T) {
	g := &Graph{
		Name:  "esc",
		Entry: `my "node"`,
		Nodes: []Node{{ID: `my "node"`, Role: "dev"}, {ID: `a\b`, Role: "qa"}},
		Edges: []Edge{{From: `my "node"`, To: `a\b`, On: "ok"}},
	}
	rep := &Report{Status: "ok", Nodes: []NodeReport{visit(`my "node"`, "ok", time.Second)}}
	out := DOT(g, rep)
	if !strings.Contains(out, `my \"node\"`) {
		t.Errorf("quotes not escaped:\n%s", out)
	}
	if !strings.Contains(out, `a\\b`) {
		t.Errorf("backslash not escaped:\n%s", out)
	}
}

func TestMermaid(t *testing.T) {
	out := Mermaid(exportGraph(), exportReport())
	if !strings.HasPrefix(out, "flowchart LR") {
		t.Fatalf("missing flowchart header:\n%s", out)
	}
	for _, want := range []string{
		// a: green, visited twice
		`a["a<br/>ok x2<br/>1.5s"]`, "class a ok",
		// b: green (last verdict ok)
		`b["b<br/>ok x2<br/>700ms"]`, "class b ok",
		// d: single 250ms visit
		`d["d<br/>ok x1<br/>250ms"]`, "class d ok",
		// c: never visited
		`c["c"]`, "class c idle",
		// terminal
		`done(("done"))`,
		// traversed vs not
		"a ==>|ok| b",
		"b ==>|fail| a",
		"b ==>|ok| d",
		"b -->|always| done",
		"classDef ok fill:#c8e6c9,stroke:#2e7d32",
		"classDef fail fill:#ffcdd2,stroke:#c62828",
		"classDef idle fill:#ffffff,stroke:#9e9e9e",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Mermaid missing %q:\n%s", want, out)
		}
	}
}

func TestMermaidIDSanitizing(t *testing.T) {
	g := &Graph{
		Name:  "san",
		Entry: "my-node 2",
		Nodes: []Node{
			{ID: "my-node 2", Role: "dev"},
			{ID: "2nd", Role: "qa"},
		},
		Edges: []Edge{{From: "my-node 2", To: "2nd", On: "ok"}},
	}
	out := Mermaid(g, nil)
	for _, want := range []string{
		`my_node_2["my-node 2"]`,
		`n_2nd["2nd"]`,
		"my_node_2 -->|ok| n_2nd",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Mermaid missing %q:\n%s", want, out)
		}
	}
}

// TestDOTTerminalEdge checks that the edge into the implicit terminal is
// marked taken (bold/==> in both exports) when the walk reached it, and
// stays plain when it did not.
func TestDOTTerminalEdge(t *testing.T) {
	g := &Graph{
		Name:  "term",
		Entry: "a",
		Nodes: []Node{{ID: "a", Role: "dev"}},
		Edges: []Edge{{From: "a", To: Terminal, On: "ok"}, {From: "a", To: Terminal, On: "fail"}},
	}
	r := &Report{
		Status:          "ok",
		ReachedTerminal: true,
		Nodes:           []NodeReport{visit("a", "ok", 500*time.Millisecond)},
	}
	out := DOT(g, r)
	for _, want := range []string{
		`done [label="done", shape=doublecircle`,
		`a -> done [label="ok", penwidth=2.0]`,
		`a -> done [label="fail"];`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DOT missing %q:\n%s", want, out)
		}
	}
	// Without ReachedTerminal the same edge stays thin.
	r.ReachedTerminal = false
	out = DOT(g, r)
	if !strings.Contains(out, `a -> done [label="ok"];`) {
		t.Errorf("terminal edge should stay thin without ReachedTerminal:\n%s", out)
	}
	// Mermaid mirror: taken terminal edge uses ==>.
	r.ReachedTerminal = true
	mm := Mermaid(g, r)
	if !strings.Contains(mm, "a ==>|ok| done") {
		t.Errorf("Mermaid missing taken terminal edge:\n%s", mm)
	}
	if !strings.Contains(mm, "a -->|fail| done") {
		t.Errorf("Mermaid should keep the untaken parallel edge plain:\n%s", mm)
	}
}
