package topology

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/memory"
	"github.com/396xwz/toolloop/internal/tools"
)

// maxBlackboardAnswerChars bounds how much of each node's final answer is
// carried into downstream node descriptions.
const maxBlackboardAnswerChars = 4000

// NodeReport is the per-visit record of one executed node.
type NodeReport struct {
	ID         string
	Verdict    string // "ok" | "fail"
	Reason     string
	Steps      int
	StartedAt  time.Time
	FinishedAt time.Time
}

// Report is the outcome of a full graph walk. It is the data source for the
// CLI printout and the Phase 5 export.
type Report struct {
	Status string // "ok" | "fail"
	Reason string
	Nodes  []NodeReport // one entry per visit, in execution order (cycle ids repeat)
	// ReachedTerminal is true when the walk's final transition went to the
	// implicit terminal, so the exports can mark the last node's edge into
	// it as taken. Early termination (max_visits, ctx cancel) leaves it
	// false even if the last verdict would have matched a terminal edge.
	ReachedTerminal bool
}

// StepHook is an optional per-node callback, invoked after the node's task
// completes (success or failure), with the node id and the full step trace.
// The CLI uses it to route -debug step detail through its debug logger.
type StepHook func(nodeID string, steps []*engine.Step)

// ConfirmFunc gates a Confirm node: called with the node before it runs;
// (false, nil) denies the walk.
type ConfirmFunc func(node Node) (bool, error)

// Sentinel errors returned by Run before the offending node runs.
var (
	ErrAgentToolForbidden = errors.New("agent tool is forbidden in graph node allowlists")
	ErrConfirmRequired    = errors.New("confirm-gated node reached in a non-interactive walk")
	ErrConfirmDenied      = errors.New("confirmation denied")
)

// Run walks g starting from g.Entry, executing each node through
// engine.Engine.RunTask and routing on verdicts until a terminal node, a
// node without a matching outgoing edge, or the max_visits cap is reached.
//
// model is shared by all nodes; the walk is strictly serial so the
// per-node SetSystemPrompt is safe. roles maps role name -> system prompt
// for every node role; the caller must have run ValidateRoles first.
// registry is the global tool registry; each node gets a filtered copy of
// its allowlist plus the verdict tool (the graph protocol).
//
// confirm gates Confirm nodes: before a Confirm node runs, confirm is called
// with the node; (false, nil) denies the walk with ErrConfirmDenied. A nil
// confirm means the walk is non-interactive: confirm-gated nodes fail with
// ErrConfirmRequired before they run.
//
// A node's run error (model/transport failure) is a node fail, not a Run
// error. Run returns an error only for structural problems (unknown
// role/tool, unknown node) or context cancellation, in which case the
// report accumulated so far is returned as well.
//
// hooks, if any, are invoked after each node's task completes with the
// node id and its step trace, for callers that log step-level detail.
func Run(ctx context.Context, g *Graph, entryTask string, model engine.ChatModel, registry tools.ToolRegistry, mem memory.Memory, rag memory.RAG, roles map[string]string, confirm ConfirmFunc, hooks ...StepHook) (*Report, error) {
	prevPrompt := model.SystemPromptValue()
	defer model.SetSystemPrompt(prevPrompt)

	eng := &engine.Engine{Model: model}
	var (
		blackboard strings.Builder
		visits     = map[string]int{}
		reports    []NodeReport
	)
	report := &Report{}

	current := g.Entry
	for {
		if err := ctx.Err(); err != nil {
			report.Status = tools.VerdictFail
			report.Reason = err.Error()
			report.Nodes = reports
			return report, err
		}
		if current == "" || current == Terminal {
			report.Status = tools.VerdictOK
			report.Reason = "walk completed"
			report.Nodes = reports
			return report, nil
		}

		node, ok := findNode(g, current)
		if !ok {
			return nil, fmt.Errorf("graph %q: walk reached unknown node %q", g.Name, current)
		}

		// Protocol rule: the agent tool is never offered to graph nodes, even
		// when present in the parent registry; delegation is an engine-level
		// capability, not a node-level one.
		for _, name := range node.Tools {
			if name == "agent" {
				report.Status = tools.VerdictFail
				report.Reason = ErrAgentToolForbidden.Error()
				report.Nodes = reports
				return report, ErrAgentToolForbidden
			}
		}

		// max_visits counts total visits per node id; the cap ends the
		// walk as a normal terminal, not an error.
		if g.MaxVisits > 0 && visits[node.ID] >= g.MaxVisits {
			report.Status = tools.VerdictFail
			report.Reason = fmt.Sprintf("max_visits (%d) exceeded for node %q", g.MaxVisits, node.ID)
			report.Nodes = reports
			return report, nil
		}
		visits[node.ID]++

		prompt, ok := roles[node.Role]
		if !ok {
			return nil, fmt.Errorf("graph %q: node %q has unknown role %q", g.Name, node.ID, node.Role)
		}

		// A Confirm node must be explicitly approved before it runs. A walk
		// without a ConfirmFunc is non-interactive: confirm-gated nodes
		// cannot be reached, so the walk fails before the node runs.
		if node.Confirm {
			if confirm == nil {
				report.Status = tools.VerdictFail
				report.Reason = ErrConfirmRequired.Error()
				report.Nodes = reports
				return report, ErrConfirmRequired
			}
			granted, cerr := confirm(node)
			if cerr != nil {
				report.Status = tools.VerdictFail
				report.Reason = cerr.Error()
				report.Nodes = reports
				return report, cerr
			}
			if !granted {
				report.Status = tools.VerdictFail
				report.Reason = ErrConfirmDenied.Error()
				report.Nodes = reports
				return report, ErrConfirmDenied
			}
		}

		nodeTools, err := nodeRegistry(registry, node)
		if err != nil {
			return nil, err
		}

		desc := entryTask
		if blackboard.Len() > 0 {
			desc = fmt.Sprintf("%s\n\nContext from previous nodes:\n%s", entryTask, blackboard.String())
		}

		task := &engine.Task{
			ID:             fmt.Sprintf("node-%s-%d", node.ID, visits[node.ID]),
			Description:    desc,
			CreatedAt:      time.Now(),
			Status:         engine.TaskPending,
			Steps:          []*engine.Step{},
			Memory:         mem,
			RAG:            rag,
			Tools:          nodeTools,
			Label:          node.ID,
			MaxSteps:       node.MaxSteps,
			RequireVerdict: true,
		}

		model.SetSystemPrompt(prompt)
		started := time.Now()
		runErr := eng.RunTask(ctx, task)
		finished := time.Now()
		for _, hook := range hooks {
			hook(node.ID, task.Steps)
		}

		verdict := task.Verdict
		if runErr != nil {
			verdict = &tools.Verdict{Status: tools.VerdictFail, Reason: runErr.Error(), Implicit: true}
		} else if verdict == nil || verdict.Status == "" {
			verdict = &tools.Verdict{Status: tools.VerdictFail, Reason: "no verdict recorded", Implicit: true}
		}

		reports = append(reports, NodeReport{
			ID:         node.ID,
			Verdict:    verdict.Status,
			Reason:     verdict.Reason,
			Steps:      len(task.Steps),
			StartedAt:  started,
			FinishedAt: finished,
		})

		answer := finalAnswer(task)
		header := fmt.Sprintf("From node '%s' (verdict: %s", node.ID, verdict.Status)
		if verdict.Reason != "" {
			header += " - " + verdict.Reason
		}
		header += "):\n"
		blackboard.WriteString(header + truncate(answer, maxBlackboardAnswerChars) + "\n\n")

		next, ok := selectEdge(g, node.ID, verdict.Status)
		if !ok || next == Terminal {
			report.Status = verdict.Status
			report.Reason = verdict.Reason
			report.ReachedTerminal = ok && next == Terminal
			report.Nodes = reports
			return report, nil
		}
		current = next
	}
}

// findNode looks up a node by id. Node ids are unique after Load/Validate.
func findNode(g *Graph, id string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// nodeRegistry builds the per-node tool registry: the node's allowlist
// copied from the global registry, plus the verdict tool which is always
// injected so a node can always close itself out.
func nodeRegistry(registry tools.ToolRegistry, node Node) (tools.ToolRegistry, error) {
	filtered := tools.NewToolRegistry()
	for _, name := range node.Tools {
		tool, ok := registry.Get(name)
		if !ok {
			return tools.ToolRegistry{}, fmt.Errorf("node %q: unknown tool %q", node.ID, name)
		}
		filtered.Register(name, tool)
	}
	filtered.Register("verdict", tools.VerdictTool{})
	return filtered, nil
}

// selectEdge picks the outgoing edge for a node: an exact verdict status
// match wins; then "always"; otherwise the walk terminates.
func selectEdge(g *Graph, from, status string) (string, bool) {
	for _, e := range g.Edges {
		if e.From == from && e.On == status {
			return e.To, true
		}
	}
	for _, e := range g.Edges {
		if e.From == from && e.On == "always" {
			return e.To, true
		}
	}
	return "", false
}

// finalAnswer returns the node's last free-text plan (a step with no tool
// call). Verdict steps carry their reason separately, so a task closed by
// a verdict call may have no final answer; in that case "" is returned.
func finalAnswer(task *engine.Task) string {
	for i := len(task.Steps) - 1; i >= 0; i-- {
		s := task.Steps[i]
		if s.ToolCall == nil && s.Err == nil && !strings.HasPrefix(s.Plan, "Blocked:") && strings.TrimSpace(s.Plan) != "" {
			return s.Plan
		}
	}
	return ""
}

// truncate shortens s to at most max characters, marking the cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
