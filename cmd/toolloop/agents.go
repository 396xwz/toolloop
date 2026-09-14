package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/396xwz/toolloop/internal/agent"
)

// ─── REPL AGENTS ────────────────────────────────────────────────────
//
// An "agent" here is a named conversation with its own system prompt and
// its own session notes. Switching agents starts a fresh conversation
// context without restarting the process, which lets you run an
// orchestrator/builder style workflow inside one REPL session.

const defaultAgentName = "main"

// replAgent is a single named conversation.
type replAgent struct {
	Name         string
	SystemPrompt string
	Notes        strings.Builder
	CreatedAt    time.Time
	Runs         int
}

// builtinAgentPrompts are ready-made roles so `/agent create builder`
// works with no prompt text typed in. These mirror the role definitions
// in AGENTS.md at the repository root (role, responsibilities and
// output_format for each subagent).
var builtinAgentPrompts = map[string]string{
	"orchestrator": `You are the orchestrator.
Role: oversee the entire project, delegate tasks, and do NOT write code yourself.

Responsibilities:
- Maintain plan.md (read it first with fs op=read; update it with fs op=write).
- Break the request into concrete, ordered steps.
- Spawn/delegate to the planner, researcher, builder, reviewer and tester agents.
- Merge the results those agents report back into a single coherent outcome.

Output format:
- A JSON array of task objects, each like
  {"id":"1","agent":"builder","task":"...","acceptance":"..."}
- Followed by the delegation command to run next, e.g.
  /agent run builder <task text>

Rules:
- Never implement a step yourself and never call fs op=write on source files.
- Always state which step is delegated next and its acceptance criteria.`,

	"planner": `You are the planner.
Role: convert goals into actionable steps.

Responsibilities:
- Read plan.md first (fs op=read, path=plan.md); if it does not exist, start a new plan.
- Produce a step-by-step implementation plan that a builder can follow without guessing.

Output format:
- An updated plan.md written with fs op=write (complete file body in "content").
- A numbered task list, each entry with: step number, goal, files to touch,
  and acceptance criteria.

Rules:
- Plan only. Do not implement steps or write source files.
- Keep steps small enough that one builder run can complete each of them.`,

	"researcher": `You are the researcher.
Role: read the codebase and extract relevant information.

Responsibilities:
- Summarize the files that matter to the current goal (fs op=read / op=read_many / op=tree).
- Describe the dependency graph: which packages, files and functions call which.
- Use web_search, browser or scrape only when external facts are needed;
  prefer scrape for dynamic pages where a plain fetch returns app-shell HTML.

Output format:
- A research.md file written with fs op=write, containing per-file summaries
  and a dependency graph section.

Rules:
- Cite the file path or URL each fact came from.
- Never invent files, symbols or data that were not actually retrieved.`,

	"builder": `You are the builder.
Role: write code for one specific step.

Responsibilities:
- Implement ONLY the assigned step.
- Modify ONLY the files you were given.

Output format:
- A unified diff (git-style) showing the change, then apply it by writing the
  complete resulting file with fs op=write (full file body in "content").

Rules:
- Do not redesign the plan, refactor unrelated code, or expand scope.
- Prefer complete, runnable files over fragments.
- Implement precisely what was asked, then stop.`,

	"reviewer": `You are the reviewer.
Role: review builder output.

Responsibilities:
- Check correctness: logic errors, wrong conditions, missing error handling,
  broken edge cases, security issues.
- Check style: naming, structure, and consistency with the surrounding code.
- Suggest concrete fixes.

Output format:
- A review report listing findings as: file:line - severity - problem - fix.
- Then either "APPROVED" with the unchanged diff, or a corrected unified diff.

Rules:
- Read files with the fs tool; do not write or modify files yourself.
- If there are no real issues, say so plainly instead of inventing nits.`,

	"tester": `You are the tester.
Role: run tests or simulate them to validate behavior.

Responsibilities:
- Run the project's existing tests with the shell tool
  (e.g. shell cmd="go test ./..." or the narrowest matching selector).
- If tests cannot be run, simulate them: walk the code path by hand and state
  the expected versus actual behavior for each case.

Output format:
- A test report with: command run, pass/fail counts, failing test names,
  the relevant error output, and a one-line verdict.

Rules:
- Only run tests that already exist; do not add new test frameworks.
- Do not modify source files to make tests pass; report failures instead.`,
}

// agentsMDPath records where AGENTS.md was loaded from (empty if not found
// or not loaded), for /agent help and debug output.
var agentsMDPath string

// agentsMDRoles is set to true for role names that came from AGENTS.md, so
// /agent help can distinguish loaded roles from the Go-coded fallback set.
var agentsMDRoles = map[string]bool{}

// loadAgentsMarkdown searches the current directory and its parents for an
// AGENTS.md file, parses its "## role" sections into system prompts, and
// merges them into builtinAgentPrompts (AGENTS.md entries win on conflict).
// This lets `/agent create <role>` auto-load roles/responsibilities/
// output_format straight from AGENTS.md instead of the hand-copied Go
// literals, and picks up any roles added to AGENTS.md later without a
// rebuild. It is safe to call once at startup; errors are non-fatal.
func loadAgentsMarkdown() {
	path := findAgentsMarkdown()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	roles := parseAgentsMarkdown(string(data))
	if len(roles) == 0 {
		return
	}
	for name, prompt := range roles {
		builtinAgentPrompts[name] = prompt
		agentsMDRoles[name] = true
	}
	agentsMDPath = path
}

// findAgentsMarkdown looks for AGENTS.md in the current working directory,
// then walks up parent directories when the CLI is started from a
// subdirectory of a repo whose AGENTS.md lives at the repo root.
func findAgentsMarkdown() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "AGENTS.md")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// parseAgentsMarkdown turns AGENTS.md's "## role" sections into system
// prompts. Expected shape per section:
//
//	## rolename
//	role: <one-line description>
//	responsibilities:
//	  - item
//	  - item
//	output_format:
//	  - item
//	  - item
//
// Unrecognized lines are ignored; sections without a "role:" line are still
// captured using whatever fields were present.
func parseAgentsMarkdown(md string) map[string]string {
	type section struct {
		name             string
		role             string
		responsibilities []string
		outputFormat     []string
	}
	var sections []*section
	var cur *section
	var listTarget *[]string

	scanner := bufio.NewScanner(strings.NewReader(md))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "## "):
			cur = &section{name: normalizeAgentName(strings.TrimPrefix(trimmed, "## "))}
			sections = append(sections, cur)
			listTarget = nil
		case cur == nil:
			continue
		case strings.HasPrefix(trimmed, "role:"):
			cur.role = strings.TrimSpace(strings.TrimPrefix(trimmed, "role:"))
			listTarget = nil
		case strings.HasPrefix(trimmed, "responsibilities:"):
			listTarget = &cur.responsibilities
		case strings.HasPrefix(trimmed, "output_format:"):
			listTarget = &cur.outputFormat
		case strings.HasPrefix(trimmed, "- ") && listTarget != nil:
			*listTarget = append(*listTarget, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		}
	}

	roles := map[string]string{}
	for _, s := range sections {
		if s.name == "" {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "You are the %s.\n", s.name)
		if s.role != "" {
			fmt.Fprintf(&b, "Role: %s\n", s.role)
		}
		if len(s.responsibilities) > 0 {
			b.WriteString("\nResponsibilities:\n")
			for _, r := range s.responsibilities {
				fmt.Fprintf(&b, "- %s\n", r)
			}
		}
		if len(s.outputFormat) > 0 {
			b.WriteString("\nOutput format:\n")
			for _, o := range s.outputFormat {
				fmt.Fprintf(&b, "- %s\n", o)
			}
		}
		roles[s.name] = strings.TrimSpace(b.String())
	}
	return roles
}

// agentManager owns the set of named conversations and the active one.
type agentManager struct {
	agents map[string]*replAgent
	order  []string
	active string
}

func newAgentManager(defaultPrompt string) *agentManager {
	m := &agentManager{agents: map[string]*replAgent{}}
	def := &replAgent{
		Name:         defaultAgentName,
		SystemPrompt: defaultPrompt,
		CreatedAt:    time.Now(),
	}
	m.agents[defaultAgentName] = def
	m.order = append(m.order, defaultAgentName)
	m.active = defaultAgentName

	for _, name := range builtinRoleNames() {
		if prompt, ok := builtinAgentPrompts[name]; ok && name != defaultAgentName {
			m.agents[name] = &replAgent{
				Name:         name,
				SystemPrompt: prompt,
				CreatedAt:    time.Now(),
			}
			m.order = append(m.order, name)
		}
	}
	return m
}

func normalizeAgentName(name string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(name), `"'`))
}

func (m *agentManager) get(name string) (*replAgent, bool) {
	norm := normalizeAgentName(name)
	if a, ok := m.agents[norm]; ok {
		return a, true
	}
	if preset, ok := builtinAgentPrompts[norm]; ok {
		a := &replAgent{Name: norm, SystemPrompt: preset, CreatedAt: time.Now()}
		m.agents[norm] = a
		m.order = append(m.order, norm)
		return a, true
	}
	return nil, false
}

func (m *agentManager) current() *replAgent {
	if a, ok := m.agents[m.active]; ok {
		return a
	}
	return m.agents[defaultAgentName]
}

func (m *agentManager) create(name, prompt string) (*replAgent, error) {
	name = normalizeAgentName(name)
	if name == "" {
		return nil, fmt.Errorf("agent name is required")
	}
	if strings.ContainsAny(name, " \t/") {
		return nil, fmt.Errorf("agent name must be a single word without '/': %q", name)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("agent %q needs a system prompt", name)
	}
	if a, exists := m.agents[name]; exists {
		a.SystemPrompt = prompt
		a.Notes.Reset()
		a.Runs = 0
		return a, nil
	}
	a := &replAgent{Name: name, SystemPrompt: prompt, CreatedAt: time.Now()}
	m.agents[name] = a
	m.order = append(m.order, name)
	return a, nil
}

func (m *agentManager) remove(name string) error {
	name = normalizeAgentName(name)
	if name == defaultAgentName {
		return fmt.Errorf("cannot delete the %q agent", defaultAgentName)
	}
	if _, ok := m.agents[name]; !ok {
		return fmt.Errorf("unknown agent: %s", name)
	}
	delete(m.agents, name)
	for i, n := range m.order {
		if n == name {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	if m.active == name {
		m.active = defaultAgentName
	}
	return nil
}

func (m *agentManager) use(name string) error {
	norm := normalizeAgentName(name)
	if _, ok := m.get(norm); !ok {
		return fmt.Errorf("unknown agent: %s (create it with /agent create %s)", name, name)
	}
	m.active = norm
	return nil
}

func (m *agentManager) list() []*replAgent {
	out := make([]*replAgent, 0, len(m.order))
	for _, n := range m.order {
		if a, ok := m.agents[n]; ok {
			out = append(out, a)
		}
	}
	return out
}

// appendNotes records a Q/A pair in the agent's own conversation history
// and trims it so a long session cannot grow without bound.
func (a *replAgent) appendNotes(question, answer string) {
	a.Notes.WriteString(fmt.Sprintf("- Q: %s\n  A: %s\n", truncate(question, 200), truncate(answer, 400)))
	if a.Notes.Len() > 8000 {
		s := a.Notes.String()
		a.Notes.Reset()
		a.Notes.WriteString(s[len(s)/2:])
	}
}

// runAgentTask runs one task using the given agent's system prompt and
// conversation notes, leaving the model's own prompt untouched afterwards.
func runAgentTask(
	ctx context.Context,
	model ChatModel,
	registry agent.ToolRegistry,
	mem agent.Memory,
	rag agent.RAG,
	ag *replAgent,
	input string,
) (string, error) {
	prevPrompt := model.SystemPromptValue()
	model.SetSystemPrompt(ag.SystemPrompt)
	defer func() { model.SetSystemPrompt(prevPrompt) }()

	desc := input
	if ag.Notes.Len() > 0 {
		desc = fmt.Sprintf("%s\n\nSession notes:\n%s", input, ag.Notes.String())
	}

	task := &agent.Task{
		ID:          fmt.Sprintf("repl-%s-%d", ag.Name, time.Now().UnixNano()),
		Description: desc,
		CreatedAt:   time.Now(),
		Status:      agent.TaskPending,
		Steps:       []*agent.Step{},
		Memory:      mem,
		RAG:         rag,
		Tools:       registry,
	}

	engine := &agent.Engine{Model: model}
	if err := engine.RunTask(ctx, task); err != nil {
		return "", err
	}
	answer, err := model.GenerateFinalAnswer(ctx, task)
	if err != nil {
		return "", err
	}

	ag.Runs++
	ag.appendNotes(input, answer)
	if mem != nil {
		_ = mem.Save(ctx, task.ID, "task_result", answer)
	}
	return answer, nil
}

// readMultilinePrompt collects prompt lines until a line containing only
// "." (or EOF). Used when /agent create is given no inline prompt.
func readMultilinePrompt(in *bufio.Scanner) string {
	fmt.Println("Enter the system prompt. Finish with a single '.' on its own line:")
	var b strings.Builder
	for {
		fmt.Print("  | ")
		if !in.Scan() {
			break
		}
		line := in.Text()
		if strings.TrimSpace(line) == "." {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func agentUsage() {
	fmt.Println(`usage:
  /agent                          List agents (a * marks the active one)
  /agent list                     Same as above
  /agent create <name>            New conversation; uses a built-in role prompt
                                  or asks you to type one
  /agent create <name> <prompt>   New conversation with an inline system prompt
  /agent create <name> -f <file>  New conversation with a prompt read from a file
  /agent use <name>               Switch the active conversation
  /agent run <name> <task>        Run one task as <name> without switching
  /agent show [name]              Show an agent's system prompt
  /agent reset [name]             Clear an agent's conversation history
  /agent delete <name>            Remove an agent

Built-in role prompts: ` + strings.Join(builtinRoleNames(), ", ") + agentsMDSourceNote())
}

// builtinRoleNames returns the built-in role names in a stable order,
// preferring the AGENTS.md canonical order when it was loaded.
func builtinRoleNames() []string {
	preferred := []string{"orchestrator", "planner", "researcher", "builder", "reviewer", "tester"}
	seen := map[string]bool{}
	var names []string
	for _, n := range preferred {
		if _, ok := builtinAgentPrompts[n]; ok {
			names = append(names, n)
			seen[n] = true
		}
	}
	for n := range builtinAgentPrompts {
		if !seen[n] {
			names = append(names, n)
			seen[n] = true
		}
	}
	return names
}

// agentsMDSourceNote reports whether roles came from AGENTS.md on disk or
// the built-in Go fallback, for /agent help.
func agentsMDSourceNote() string {
	if agentsMDPath != "" {
		return fmt.Sprintf(" (loaded from %s)", agentsMDPath)
	}
	return " (AGENTS.md not found; using built-in fallback)"
}

// handleAgentCommand implements the /agent command family.
func handleAgentCommand(
	ctx context.Context,
	line string,
	mgr *agentManager,
	in *bufio.Scanner,
	model ChatModel,
	registry agent.ToolRegistry,
	mem agent.Memory,
	rag agent.RAG,
) {
	fields := strings.Fields(line)
	sub := "list"
	if len(fields) >= 2 {
		sub = strings.ToLower(fields[1])
	}

	switch sub {
	case "list", "ls":
		agents := mgr.list()
		fmt.Printf("Agents (%d):\n", len(agents))
		for _, a := range agents {
			marker := " "
			if a.Name == mgr.active {
				marker = "*"
			}
			fmt.Printf(" %s %-14s runs=%-3d prompt: %s\n",
				marker, a.Name, a.Runs, truncate(strings.ReplaceAll(a.SystemPrompt, "\n", " "), 80))
		}

	case "create", "new", "spawn":
		if len(fields) < 3 {
			fmt.Println("usage: /agent create <name> [prompt | -f file.txt]")
			return
		}
		name := fields[2]
		prompt := ""
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[strings.Index(line, fields[2])+len(fields[2]):]), " "))

		switch {
		case strings.HasPrefix(rest, "-f ") || strings.HasPrefix(rest, "--file "):
			path := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(rest, "--file "), "-f "))
			data, err := os.ReadFile(path)
			if err != nil {
				fmt.Println("could not read prompt file:", err)
				return
			}
			prompt = strings.TrimSpace(string(data))
		case rest != "":
			prompt = rest
		default:
			if preset, ok := builtinAgentPrompts[normalizeAgentName(name)]; ok {
				prompt = preset
				fmt.Printf("Using built-in %q role prompt.\n", normalizeAgentName(name))
			} else {
				prompt = readMultilinePrompt(in)
			}
		}

		a, err := mgr.create(name, prompt)
		if err != nil {
			fmt.Println("agent error:", err)
			return
		}
		_ = mgr.use(a.Name)
		fmt.Printf("Created agent %q and switched to it (fresh conversation).\n", a.Name)
		fmt.Printf("System prompt:\n%s\n", a.SystemPrompt)

	case "use", "switch":
		if len(fields) < 3 {
			fmt.Println("usage: /agent use <name>")
			return
		}
		if err := mgr.use(fields[2]); err != nil {
			fmt.Println("agent error:", err)
			return
		}
		fmt.Printf("Active agent: %s\n", mgr.active)

	case "run":
		if len(fields) < 4 {
			fmt.Println("usage: /agent run <name> <task>")
			return
		}
		target, ok := mgr.get(fields[2])
		if !ok {
			fmt.Printf("unknown agent: %s (create it with /agent create %s)\n", fields[2], fields[2])
			return
		}
		idx := strings.Index(line, fields[3])
		taskText := strings.TrimSpace(line[idx:])
		fmt.Printf("[%s] running: %s\n", target.Name, truncate(taskText, 160))
		answer, err := runAgentTask(ctx, model, registry, mem, rag, target, taskText)
		if err != nil {
			fmt.Println("Task failed:", err)
			return
		}
		fmt.Printf("\n[%s] %s\n\n", target.Name, answer)
		// Let the calling agent see what its delegate reported.
		if cur := mgr.current(); cur != nil && cur.Name != target.Name {
			cur.appendNotes(fmt.Sprintf("delegated to %s: %s", target.Name, taskText), answer)
		}

	case "show", "prompt":
		name := mgr.active
		if len(fields) >= 3 {
			name = fields[2]
		}
		a, ok := mgr.get(name)
		if !ok {
			fmt.Println("unknown agent:", name)
			return
		}
		fmt.Printf("Agent %s (runs=%d, created %s)\nSystem prompt:\n%s\n",
			a.Name, a.Runs, a.CreatedAt.Format(time.RFC3339), a.SystemPrompt)

	case "reset", "clear":
		name := mgr.active
		if len(fields) >= 3 {
			name = fields[2]
		}
		a, ok := mgr.get(name)
		if !ok {
			fmt.Println("unknown agent:", name)
			return
		}
		a.Notes.Reset()
		fmt.Printf("Cleared conversation history for %s (DB unchanged)\n", a.Name)

	case "delete", "rm", "remove":
		if len(fields) < 3 {
			fmt.Println("usage: /agent delete <name>")
			return
		}
		if err := mgr.remove(fields[2]); err != nil {
			fmt.Println("agent error:", err)
			return
		}
		fmt.Printf("Deleted agent %s (active: %s)\n", normalizeAgentName(fields[2]), mgr.active)

	case "help", "?":
		agentUsage()

	default:
		fmt.Println("unknown /agent subcommand:", sub)
		agentUsage()
	}
}
