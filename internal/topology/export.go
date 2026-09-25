package topology

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Node execution colors shared by both exports.
const (
	fillOK       = "#c8e6c9"
	fillFail     = "#ffcdd2"
	fillIdle     = "#ffffff"
	fillTerminal = "#eeeeee"
)

// nodeStat aggregates everything the exports need per node id: how many
// visits, the last verdict seen, and the total wall time across visits.
type nodeStat struct {
	visits   int
	verdict  string
	duration time.Duration
}

// nodeStats aggregates report visits per node id. A nil report yields a nil
// map (every node renders idle).
func nodeStats(report *Report) map[string]*nodeStat {
	if report == nil {
		return nil
	}
	m := make(map[string]*nodeStat, len(report.Nodes))
	for _, n := range report.Nodes {
		st, ok := m[n.ID]
		if !ok {
			st = &nodeStat{}
			m[n.ID] = st
		}
		st.visits++
		st.verdict = n.Verdict
		if !n.StartedAt.IsZero() {
			st.duration += n.FinishedAt.Sub(n.StartedAt)
		}
	}
	return m
}

// takenEdges records the node transitions actually walked, keyed
// "from\x00on\x00to" so that parallel edges between the same pair are only
// marked for the label actually taken. The walk is serial, so each
// consecutive pair in the report is one traversed edge; the taken label is
// recomputed from the source visit's verdict, mirroring the runner's
// selectEdge. When the walk reached the implicit terminal
// (report.ReachedTerminal) the last node's edge into it is traversed too —
// the terminal itself is never recorded as a visit.
func takenEdges(g *Graph, report *Report) map[string]bool {
	m := map[string]bool{}
	if report == nil {
		return m
	}
	for i := 1; i < len(report.Nodes); i++ {
		from := report.Nodes[i-1]
		to := report.Nodes[i].ID
		m[from.ID+"\x00"+edgeOn(g, from.ID, to, from.Verdict)+"\x00"+to] = true
	}
	if report.ReachedTerminal && len(report.Nodes) > 0 {
		last := report.Nodes[len(report.Nodes)-1]
		m[last.ID+"\x00"+edgeOn(g, last.ID, Terminal, last.Verdict)+"\x00"+Terminal] = true
	}
	return m
}

// edgeOn returns the On label of the edge the walk takes from "from" to
// "to" with the given verdict status, mirroring runner selectEdge: the
// exact status match in edge order first, then "always". Empty when no
// edge matches, which no render lookup can hit.
func edgeOn(g *Graph, from, to, status string) string {
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.On == status {
			return e.On
		}
	}
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.On == "always" {
			return e.On
		}
	}
	return ""
}

// dotQuote quotes s as a DOT string literal, escaping backslashes and
// double quotes.
func dotQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// dotReserved holds DOT keywords and the attribute names this file emits; a
// bare node id must not collide with them.
var dotReserved = map[string]bool{
	"digraph": true, "graph": true, "subgraph": true, "node": true,
	"edge": true, "strict": true, "rankdir": true, "shape": true,
	"style": true, "fillcolor": true, "label": true, "penwidth": true,
}

// dotID renders s as a DOT node id: bare when s is a plain identifier that
// is not reserved, quoted-escaped otherwise.
func dotID(s string) string {
	if isBareDotID(s) {
		return s
	}
	return dotQuote(s)
}

func isBareDotID(s string) bool {
	if s == "" || dotReserved[s] {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// mermaidID maps a raw node id to a safe mermaid node id: only [A-Za-z0-9_],
// no leading digit, unique within this export.
func mermaidID(raw string, seen map[string]bool) string {
	var sb strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	id := sb.String()
	if id == "" {
		id = "n"
	}
	if id[0] >= '0' && id[0] <= '9' {
		id = "n_" + id
	}
	base := id
	for k := 2; seen[id]; k++ {
		id = fmt.Sprintf("%s_%d", base, k)
	}
	seen[id] = true
	return id
}

// formatDuration renders sub-second durations in ms and everything else to
// one decimal second.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// DOT renders the declared graph as a Graphviz digraph annotated with the
// walk from report (nil report: every node idle). Node colors: ok green,
// fail red, never-visited white, terminal "done" a grey doublecircle.
// Labels carry id, last verdict x visit-count, and summed duration. Traversed
// edges are thick (penwidth).
func DOT(g *Graph, report *Report) string {
	stats := nodeStats(report)
	taken := takenEdges(g, report)
	hasTerminal := false
	for _, e := range g.Edges {
		if e.To == Terminal {
			hasTerminal = true
			break
		}
	}
	var b strings.Builder
	b.WriteString("digraph topology {\n\trankdir=LR;\n")
	b.WriteString("\tnode [shape=box, style=filled, fillcolor=" + dotQuote(fillIdle) + "];\n")
	for _, n := range g.Nodes {
		st := stats[n.ID]
		fill := fillIdle
		label := n.ID
		if st != nil {
			switch st.verdict {
			case "ok":
				fill = fillOK
			case "fail":
				fill = fillFail
			}
			label += "\n" + st.verdict + " x" + strconv.Itoa(st.visits)
			label += "\n" + formatDuration(st.duration)
		}
		fmt.Fprintf(&b, "\t%s [label=%s, fillcolor=%s];\n", dotID(n.ID), dotQuote(label), dotQuote(fill))
	}
	if hasTerminal {
		fmt.Fprintf(&b, "\t%s [label=%s, shape=doublecircle, style=filled, fillcolor=%s];\n",
			dotID(Terminal), dotQuote(Terminal), dotQuote(fillTerminal))
	}
	for _, e := range g.Edges {
		style := ""
		if taken[e.From+"\x00"+e.On+"\x00"+e.To] {
			style = ", penwidth=2.0"
		}
		fmt.Fprintf(&b, "\t%s -> %s [label=%s%s];\n", dotID(e.From), dotID(e.To), dotQuote(e.On), style)
	}
	b.WriteString("}\n")
	return b.String()
}

// Mermaid renders the declared graph as a mermaid flowchart annotated with
// the walk from report. Colors via classDef (ok/fail/idle); traversed edges
// use ==> instead of -->.
func Mermaid(g *Graph, report *Report) string {
	stats := nodeStats(report)
	taken := takenEdges(g, report)
	seen := map[string]bool{}
	ids := make(map[string]string, len(g.Nodes))
	declare := func(raw string) string {
		if id, ok := ids[raw]; ok {
			return id
		}
		id := mermaidID(raw, seen)
		ids[raw] = id
		return id
	}
	hasTerminal := false
	for _, e := range g.Edges {
		if e.To == Terminal {
			hasTerminal = true
			break
		}
	}
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	for _, n := range g.Nodes {
		id := declare(n.ID)
		st := stats[n.ID]
		label := n.ID
		class := "idle"
		if st != nil {
			class = "idle"
			if st.verdict == "ok" {
				class = "ok"
			} else if st.verdict == "fail" {
				class = "fail"
			}
			label += "<br/>" + st.verdict + " x" + strconv.Itoa(st.visits)
			label += "<br/>" + formatDuration(st.duration)
		}
		fmt.Fprintf(&b, "    %s[%q]\n", id, label)
		fmt.Fprintf(&b, "    class %s %s\n", id, class)
	}
	if hasTerminal {
		id := declare(Terminal)
		fmt.Fprintf(&b, "    %s((\"%s\"))\n", id, Terminal)
	}
	for _, e := range g.Edges {
		arrow := "-->"
		if taken[e.From+"\x00"+e.On+"\x00"+e.To] {
			arrow = "==>"
		}
		fmt.Fprintf(&b, "    %s %s|%s| %s\n", declare(e.From), arrow, e.On, declare(e.To))
	}
	b.WriteString("    classDef ok fill:#c8e6c9,stroke:#2e7d32\n")
	b.WriteString("    classDef fail fill:#ffcdd2,stroke:#c62828\n")
	b.WriteString("    classDef idle fill:#ffffff,stroke:#9e9e9e\n")
	return b.String()
}
