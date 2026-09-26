// Package topology defines the graph format, parser, and structural
// validation for toolloop topology files.
//
// A topology file is a YAML document describing a named directed graph of
// nodes (execution roles with scoped tools) connected by edges that fire on a
// step verdict (ok, fail, always). The graph may contain cycles, in which
// case max_visits must bound them; the implicit terminal "done" is a valid
// edge endpoint that is never a declared node.
//
// Load performs only structural validation. Role and tool names are
// runtime-dependent, so they are validated by ValidateRoles and ValidateTools
// once the CLI/runner knows what is loaded.
//
// Safety: a node's tool scope limits which nodes can obtain which tools —
// never the agent tool — but it does not limit what a tool can do; a
// shell-scoped node is an unattended shell. A confirm: true node is gated
// interactively (Enter approves, any other input denies) and fails the walk
// in non-interactive runs.
package topology

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Terminal is the implicit terminal endpoint: a valid edge To value that is
// never a declared node. Edges out of Terminal are invalid.
const Terminal = "done"

// Node is the per-node execution config. MaxSteps 0 = unspecified; the
// default is applied by the runner at run time, not here.
type Node struct {
	ID       string   `yaml:"id"`
	Role     string   `yaml:"role"`
	Tools    []string `yaml:"tools"`
	MaxSteps int      `yaml:"max_steps"`
	Confirm  bool     `yaml:"confirm"`
}

// Edge is one transition between two nodes. From must be a declared node;
// To must be a declared node or Terminal. On must be ok, fail or always.
type Edge struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	On   string `yaml:"on"`
}

// Graph is a parsed topology file. MaxVisits is the cap for revisits to
// nodes on cycles; absent/0 means unbounded.
type Graph struct {
	Name      string `yaml:"name"`
	Entry     string `yaml:"entry"`
	MaxVisits int    `yaml:"max_visits"`
	Nodes     []Node `yaml:"nodes"`
	Edges     []Edge `yaml:"edges"`
}

// Sentinel errors returned by Load and the runtime-dependent validators.
var (
	ErrEmptyNodeID     = errors.New("topology: node id is empty")
	ErrDuplicateNodeID = errors.New("topology: duplicate node id")
	ErrTerminalNodeID  = errors.New("topology: node id is the implicit terminal")
	ErrNoEntry         = errors.New("topology: entry not set or not a node id")
	ErrUnknownEndpoint = errors.New("topology: edge endpoint is not a node id or terminal")
	ErrBadOnValue      = errors.New("topology: edge on value must be ok, fail or always")
	ErrUnreachableNode = errors.New("topology: node unreachable from entry")
	ErrUnboundedCycle  = errors.New("topology: cycle exists without bounded max_visits")
	ErrUnknownRole     = errors.New("topology: node role not in known roles")
	ErrUnknownTool     = errors.New("topology: node tool not in known tools")
)

// Load reads and parses the topology file at path, then runs structural
// validation. First failure wins; every returned error wraps the matching
// sentinel so callers can test with errors.Is.
func Load(path string) (*Graph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("topology: read %s: %w", path, err)
	}
	var g Graph
	if err := yaml.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("topology: parse %s: %w", path, err)
	}
	if err := g.validate(); err != nil {
		return nil, err
	}
	return &g, nil
}

// validate runs the structural validation steps in fixed order; the first
// failing step's error is returned.
func (g *Graph) validate() error {
	// a. every node has a non-empty ID.
	for i, n := range g.Nodes {
		if n.ID == "" {
			return fmt.Errorf("topology: node %d: %w", i, ErrEmptyNodeID)
		}
	}

	// b. node IDs are unique and none is the implicit terminal.
	declared := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.ID == Terminal {
			return fmt.Errorf("topology: node id %q: %w", n.ID, ErrTerminalNodeID)
		}
		if declared[n.ID] {
			return fmt.Errorf("topology: node id %q: %w", n.ID, ErrDuplicateNodeID)
		}
		declared[n.ID] = true
	}

	// c. entry is set and is a declared node.
	if g.Entry == "" {
		return fmt.Errorf("topology: %w", ErrNoEntry)
	}
	if !declared[g.Entry] {
		return fmt.Errorf("topology: entry %q: %w", g.Entry, ErrNoEntry)
	}

	// d. edge endpoints and on values.
	for i, e := range g.Edges {
		if !declared[e.From] {
			return fmt.Errorf("topology: edge %d: from %q: %w", i, e.From, ErrUnknownEndpoint)
		}
		if e.To != Terminal && !declared[e.To] {
			return fmt.Errorf("topology: edge %d: to %q: %w", i, e.To, ErrUnknownEndpoint)
		}
		switch e.On {
		case "ok", "fail", "always":
		default:
			return fmt.Errorf("topology: edge %d: on %q: %w", i, e.On, ErrBadOnValue)
		}
	}

	// e. every declared node is reachable from entry. Edges are followed
	// regardless of on; the terminal is a sink.
	next := make(map[string][]string, len(g.Edges))
	for _, e := range g.Edges {
		next[e.From] = append(next[e.From], e.To)
	}
	reached := make(map[string]bool, len(g.Nodes))
	stack := []string{g.Entry}
	reached[g.Entry] = true
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, nxt := range next[cur] {
			if nxt == Terminal || reached[nxt] {
				continue
			}
			reached[nxt] = true
			stack = append(stack, nxt)
		}
	}
	for _, n := range g.Nodes {
		if !reached[n.ID] {
			return fmt.Errorf("topology: node %q: %w", n.ID, ErrUnreachableNode)
		}
	}

	// f. any directed cycle requires a bounded max_visits. Three-color DFS
	// over declared nodes only; the terminal is excluded and on is ignored.
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS path
		black = 2 // finished
	)
	color := make(map[string]int, len(g.Nodes))
	var cyclic bool
	var dfs func(string) bool
	dfs = func(id string) bool {
		color[id] = gray
		for _, nxt := range next[id] {
			if nxt == Terminal {
				continue
			}
			switch color[nxt] {
			case white:
				if dfs(nxt) {
					return true
				}
			case gray:
				return true
			}
		}
		color[id] = black
		return false
	}
	for _, n := range g.Nodes {
		if color[n.ID] == white && dfs(n.ID) {
			cyclic = true
			break
		}
	}
	if cyclic && g.MaxVisits < 1 {
		return fmt.Errorf("topology: %w", ErrUnboundedCycle)
	}

	return nil
}

// ValidateRoles checks that every node has a non-empty Role present in
// knownRoles. The CLI/runner calls it once it knows which roles are loaded;
// Load does not.
func (g *Graph) ValidateRoles(knownRoles []string) error {
	known := make(map[string]bool, len(knownRoles))
	for _, r := range knownRoles {
		known[r] = true
	}
	for _, n := range g.Nodes {
		if n.Role == "" || !known[n.Role] {
			return fmt.Errorf("topology: node %q: role %q: %w", n.ID, n.Role, ErrUnknownRole)
		}
	}
	return nil
}

// ValidateTools checks that every tool name referenced by a node is present
// in knownTools. The CLI/runner calls it once it knows the global registry's
// names; Load does not.
func (g *Graph) ValidateTools(knownTools []string) error {
	known := make(map[string]bool, len(knownTools))
	for _, t := range knownTools {
		known[t] = true
	}
	for _, n := range g.Nodes {
		for _, t := range n.Tools {
			if !known[t] {
				return fmt.Errorf("topology: node %q: tool %q: %w", n.ID, t, ErrUnknownTool)
			}
		}
	}
	return nil
}
