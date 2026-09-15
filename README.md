# toolloop — Local Tool-Using Agent (Go)

A local Go agent with one CLI and pluggable model backends. The default backend
is llama.cpp; Ollama remains available with `-backend ollama`.

## Quick start: easiest Ollama path

If you want the fastest first run, use Ollama before setting up a llama.cpp
server on `localhost:8080`. Make sure Ollama is running locally and the model
you name is already available.

```bash
go run ./cmd/toolloop -backend ollama -model qwen2.5:14b \
  -task "Summarize what this project does"

go run ./cmd/toolloop -backend ollama -model qwen2.5:14b -repl
```

The default backend is still llama.cpp when `-backend` is omitted. If a
`./documents` directory exists, toolloop indexes it automatically on startup;
use `-skip-index` to disable that optional pass.

Example llama.cpp server usage:

```bash
go run ./cmd/toolloop -repl -server http://localhost:8080
```

| Command | Backend | Notes |
|---------|---------|-------|
| `go run ./cmd/toolloop` | llama.cpp `/completion` | Default. Uses prompt-based JSON tool calls and `/apply-template` when available. |
| `go run ./cmd/toolloop -backend ollama` | Ollama `/api/chat` | Uses Ollama native tool calls plus JSON fallback. |

The CLI entry point lives in `cmd/toolloop`. The execution loop, tools,
memory, and RAG live under `internal/`.
There is one active implementation; the former `src/` and `llama-go/` trees
have been removed.

## Features

- Tools: `fs`, `shell`, `browser`, `web_search`, `scrape`, `python`.
- `fs` supports `list`, `tree`, `read`, `read_many`, `info`, `write`, and safe
  `edit` by unique substring replacement.
- `python` runs a script (`path`) or inline source (`code`) using the
  project's own `.venv` interpreter, so scripts in `python/` get their
  installed dependencies instead of a bare system Python.
- Durable memory in `agent_memory.db` and durable RAG in `agent_rag.db`.
- Directory indexing with `-index`; if `./documents` exists it is indexed
  automatically on startup unless you pass `-skip-index`.
- Multi-task CLI with repeatable `-task` and shared context between tasks.
- REPL mode with direct tools and named sub-agents loaded from `AGENTS.md`.
- Prompt override with `-prompt <file>`.
- Debug traces with `-debug`, including prompt sizes and tool parsing details.

## Repository layout

```text
cmd/toolloop/        CLI wiring, model backends, REPL, Git, and task helpers
internal/engine/     task execution loop, step guards, and model contracts
internal/tools/      filesystem, shell, browser, search, Python, and scrape tools
internal/memory/     SQLite memory and RAG
python/              scripts executed by the Python tool
examples/prompts/    reusable role prompt examples
testdata/            test fixtures
docs/                architecture notes
```

## Requirements

- Go 1.26.x (the module currently declares `go 1.26.5`).
- Git 2.30+ for the `/git <revision>` REPL command. This version supports the
  safe `rev-parse --end-of-options` delimiter used for revision arguments that
  begin with `-`.
- Default backend: a llama.cpp server running at `http://localhost:8080`
  (override with `-server`) and serving `/completion`.
- Optional Ollama backend: [Ollama](https://ollama.com) running locally; enable
  it with `-backend ollama`.
- Optional for `scrape`: the Python Scrapling CLI on `PATH`, at
  `.venv/bin/scrapling`, or specified with `SCRAPLING_BIN`.
- Optional for `python`: a `.venv` at the repo root (or one/two directories
  above the working directory or the compiled binary) with `.venv/bin/python3`;
  falls back to `python3`/`python` on `PATH`, or set `PYTHON_BIN` to an exact
  interpreter path.
- Shell tool: Bash is required on Unix. On Windows, the shell tool uses Windows
  PowerShell with `-NoProfile` and `-NonInteractive`.

## Architecture

The control loop is:

1. `PlanNextStep` asks the selected backend what to do next and parses either
   native tool calls (`-backend ollama`) or a prompted JSON object
   (`-backend llama.cpp`, the default).
2. `Engine.RunTask` executes the named tool from the registry.
3. The loop repeats until no tool is needed, a repeat is detected, or the step
   limit is reached.
4. `GenerateFinalAnswer` makes a second model call grounded in the gathered
   tool/step data.

`LlamaCppModel` uses prompt-based tool calling because llama.cpp `/completion`
does not expose native tool calls. The model is instructed to emit exactly one
JSON object such as:

```json
{"tool":"fs","args":{"op":"read","path":"README.md"}}
```

## Tools

| Tool | Args | Notes |
|------|------|-------|
| `fs` | `op`, `path`, `depth`, `max_bytes`, `content`, `old_snippet`, `new_snippet`, `start_line`, `end_line`, `checksum` | Filesystem operations |
| `shell` | `cmd` | Runs Bash on Unix and Windows PowerShell (`-NoProfile`, `-NonInteractive`) on Windows; leading `fs ...` commands are intercepted and executed by the fs tool. Models receive OS-aware shell guidance. |
| `browser` | `url` | Fetches URL text |
| `web_search` | `query` | Web search helper |
| `python` | `path`, `code`, `args`, `cwd`, `timeout` | Runs a script or inline source with the project's `.venv` interpreter (falls back to system `python3`/`python`) |
| `scrape` | `url`, `output`, `mode`, `selector`, `timeout`, `wait`, `max_chars` | Uses Scrapling for dynamic pages; writes full output to a file and returns a capped excerpt |

### `fs edit`

`fs op=edit` is a safe substring replacement for code changes. It reads the
file first, requires `old_snippet` to appear exactly once, writes only after
validation succeeds, and returns a unified diff hunk. If the snippet appears
zero times or more than once, it errors and does not write.

```json
{
  "tool": "fs",
  "args": {
    "op": "edit",
    "path": "main.go",
    "old_snippet": "old unique text",
    "new_snippet": "new text"
  }
}
```

Optional checksum guard:

```json
{
  "tool": "fs",
  "args": {
    "op": "edit",
    "path": "main.go",
    "old_snippet": "old unique text",
    "new_snippet": "new text",
    "start_line": "10",
    "end_line": "14",
    "checksum": "<sha256 hex of original lines 10-14>"
  }
}
```

### `python`

`python` runs a script file or inline source with the project's `.venv`
interpreter (`.venv/bin/python3`), avoiding the common failure mode where
`shell cmd=python3 ...` silently falls back to the system Python and errors
with missing dependencies (e.g. `scipy`).

```json
{"tool": "python", "args": {"path": "python/wnba.py"}}
```

```json
{"tool": "python", "args": {"code": "import scipy\nprint('ok')", "timeout": "30"}}
```

Resolution order: `PYTHON_BIN` env var → `.venv/bin/python3` near the working
directory or the compiled binary → `python3`/`python` on `PATH`.

## Flags

| Flag | Description |
|------|-------------|
| `-task` | Task description; repeatable |
| `-backend` | Model backend: `llama.cpp` (default) or `ollama` |
| `-repl` | Interactive REPL mode |
| `-server` | llama.cpp server base URL (default `http://localhost:8080`) |
| `-model` | Model name/label; with llama.cpp this is informational because the server serves one loaded model |
| `-index` | Index a directory into RAG |
| `-skip-index` | Skip automatic indexing of `./documents` when it exists |
| `-web` | Pre-run web search and inject result as context |
| `-browser` | Pre-run URL fetch and inject result as context |
| `-scrape` | Pre-run Scrapling scrape and inject capped result as context |
| `-scrape-output` | Output path for `-scrape` (default `sports.md`) |
| `-fs` | Pre-run simple fs op, e.g. `"list ./agent"` |
| `-shell` | Pre-run shell command and inject result as context |
| `-output` | Save full run output to a file |
| `-prompt` | Prompt file to use as the system prompt |
| `-debug` | Enable debug logging |

## Runtime behavior and guards

- Path/op guards sanitize model-emitted fs arguments and reject operations that
  are not justified by the task in guarded paths.
- Repeated identical tool calls are detected and stopped.
- A successful `fs write` stops the loop immediately to avoid an unnecessary
  follow-up model call on slower backends.
- Large tool results are capped before being fed back into the model to avoid
  overflowing llama.cpp context; full scrape output is still written to disk.
- `GenerateFinalAnswer` includes the active agent's system prompt, so custom
  agents created with `-prompt` or `/agent create <name> -f <file>` keep their
  instructions for the final response.

## Examples

Build and run the CLI:

```bash
cd toolloop
go build -o toolloop ./cmd/toolloop
./toolloop -repl
```

Or run it directly with Ollama:

```bash
go run ./cmd/toolloop -backend ollama -model qwen2.5:14b -task "..."
```

Basic task:

```bash
go run ./cmd/toolloop -task "Summarize what this project does"
```

Run the default llama.cpp backend:

```bash
go run ./cmd/toolloop -server http://localhost:8080 \
  -task "Use fs read on README.md and summarize the project"
```

Run the Ollama backend:

```bash
go run ./cmd/toolloop -backend ollama -model qwen2.5:14b \
  -task "Use fs read on README.md and summarize the project"
```

REPL:

```bash
go run ./cmd/toolloop -repl

main> fs read README.md
main> python python/wnba.py
main> Use browser on https://go.dev and summarize the landing page
```

### Inspecting Git state from the REPL

`/git` is a native, **read-only** REPL command. It prints the repository root,
branch (or detached HEAD), status, and separate staged and unstaged diffs.
`/git <revision>` resolves one revision and shows its metadata and patch. Git
output is bounded; large sections end with `...[truncated]`. Git commands time
out after 10 seconds, disable configured fsmonitor support, and diffs disable
text-conversion drivers. On Unix, cancellation kills Git's process group. On
Windows, it directly kills Git and closes its Job Object, which terminates
descendants already assigned to that Job Object. It first runs
`taskkill /T /F` for the direct Git PID to best-effort target descendants
created before Job Object assignment; such a descendant may survive if
`taskkill` is unavailable or times out. On other platforms, cancellation kills
the direct Git process and closes its output reader so it still returns on
deadline, but a Git descendant that inherited the output handle may continue
running.

```text
main> /git
main> /git HEAD
main> /git v1.2.3
```

Filesystem read/write:

```bash
go run ./cmd/toolloop \
  -task "Use fs read_many to read README.md AGENTS.md and summarize each file"

go run ./cmd/toolloop \
  -task "Use the fs tool with op=write, path=sample.go, and content set to a complete runnable Go program. After writing, stop."
```

Safe edit by unique snippet:

```bash
go run ./cmd/toolloop \
  -task 'Use fs op=edit on sample.go with old_snippet set to `fmt.Println("old")` and new_snippet set to `fmt.Println("new")`. Return the diff hunk.'
```

Clone a repository through the shell tool:

```bash
go run ./cmd/toolloop \
  -task "Use the shell tool to run: scripts/clone-repo.sh https://github.com/octocat/Hello-World.git"
```

`scripts/clone-repo.sh` clones into `/tmp/<directory-name>` and refuses to
reuse an existing target there.

Scrape a dynamic page with Scrapling:

```bash
go run ./cmd/toolloop \
  -scrape "https://sportsbook.draftkings.com/leagues/football/ncaaf" \
  -scrape-output dk.md \
  -task "Summarize only the live College Football games from the scrape result"
```

Run a Python script from the model:

```bash
go run ./cmd/toolloop \
  -task "Use the python tool to run python/wnba.py and summarize the output"
```

Multi-task run with output:

```bash
go run ./cmd/toolloop \
  -task "Use fs tree with depth 2 on cmd/toolloop" \
  -task "Use fs read on README.md and summarize the project" \
  -output project-summary.txt
```

## REPL agents

The unified REPL includes an `/agent` command. An **agent** is a named
conversation with its own system prompt *and* its own session notes, so
switching agents starts a genuinely fresh context without restarting the
process.

```
/agent                          List agents (a * marks the active one)
/agent list                     Same as above
/agent create <name>            New/replaced conversation; uses a built-in role
                                prompt or asks you to type one
/agent create <name> <prompt>   New conversation with an inline system prompt
/agent create <name> -f <file>  New conversation with a prompt read from a file
/agent use <name>               Switch the active conversation
/agent run <name> <task>        Run one task as <name> without switching
/agent show [name]              Show an agent's system prompt
/agent reset [name]             Clear an agent's conversation history
/agent delete <name>            Remove an agent
```

Built-in role prompts are **auto-loaded from [`AGENTS.md`](AGENTS.md)** at REPL
startup: it searches the current directory and its parents (so it is found
when you run from the repository root), parses each `## role`
section's `role:`, `responsibilities:` and `output_format:` fields into a
system prompt, and merges the result into the built-in role table (AGENTS.md
wins over the Go-coded fallback used only if the file is missing). Edit
`AGENTS.md` and the new prompt takes effect on the next REPL start — no rebuild
needed. The REPL prints `Loaded agent roles from <path> (...)` when this
succeeds, and `/agent help` shows where the roles came from.

The AGENTS.md roles are pre-instantiated at startup, so `/agent`,
`/agent use tester`, and `/agent run researcher <task>` work immediately
without first typing `/agent create tester` or `/agent create researcher`.
Out of the box this defines six roles:

| Role | Purpose | Output format |
|------|---------|---------------|
| `orchestrator` | Owns the project, delegates, never writes code | JSON task objects + delegation commands |
| `planner`      | Turns goals into ordered steps | `plan.md` updates, task lists |
| `researcher`   | Reads the codebase and extracts facts | `research.md` |
| `builder`      | Implements exactly one assigned step | Unified (git-style) diff |
| `reviewer`     | Checks builder output for correctness/style | Review report + approved/corrected diff |
| `tester`       | Validates behavior by running tests | Test report |

For example the `orchestrator` prompt is:

```
You are the orchestrator.
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
- Always state which step is delegated next and its acceptance criteria.
```

Passing prompt text inline **overrides** the built-in role of the same name:

```
main> /agent create builder You are a Rust-only builder. Implement only the step you are given, touch only the files listed, and reply with a unified diff.
Created agent "builder" and switched to it (fresh conversation).
System prompt:
You are a Rust-only builder. Implement only the step you are given, touch only the files listed, and reply with a unified diff.
builder>
```

If you deleted or replaced one, omit the text to restore the AGENTS.md built-in:

```
main> /agent create builder
Using built-in "builder" role prompt.
```

Orchestrator / builder workflow:

```
go run ./cmd/toolloop -repl

main>         /agent use orchestrator
orchestrator> Plan how to add retry logic to the scrape tool
orchestrator> /agent run builder Implement Step 3: add a retry loop around the Scrapling call
```

Read and summarize a file with a specialist:

```
main> /agent run researcher Read dk.md with fs op=read and summarize only the live in-progress CFB games
```

Use a custom prompt file:

```
main> /agent create sports -f ./examples/prompts/SPORTS.md
sports> How to derive baseline probabilities from sportsbook lines?
```

Notes:
- The prompt shows the active agent (`orchestrator>` instead of `agent>`).
- `/agent run` executes under the target agent's prompt and history, then feeds
  the result back into the **calling** agent's notes, so an orchestrator sees
  what its delegate reported without losing its own context.
- `/reset` clears only the active agent's history; the memory DB is unchanged.
- `main` is the default agent, seeded from `-prompt` (or the built-in default
  system prompt), and cannot be deleted.
- Creating an agent with a name that already exists replaces its prompt and
  clears that agent's notes/runs.
- Agents live for the duration of the REPL session; they are not persisted
  across restarts.
- Delegation is driven by you typing `/agent run` — the model cannot yet spawn
  a sub-agent on its own (see TODO).

## Model notes

The default backend is `llama.cpp`, which serves the actual loaded model from
the server; `-model` is mostly an informational label in logs and output for
that backend. Use `-backend ollama -model <name>` to use Ollama native chat/tool
calls. Some reasoning models may emit scratch-work around tool calls, so run
with `-debug` when comparing backend/model behavior.

## Roadmap

Faster vector search at larger scale
Multi-tool calls per step

## TODO

- **Make the agent a callable tool.** Today `/agent run` is driven by the user
  typing the command; the model cannot delegate on its own. Register an `agent`
  tool (args: `name`, `task`, and optionally `prompt` to create-on-demand) so an
  orchestrator can emit
  `{"tool":"agent","args":{"name":"builder","task":"Implement Step 3"}}`
  and spawn/dispatch a sub-agent autonomously. Needs: recursion/depth limits to
  prevent runaway spawning, a per-delegation step budget, and returning the
  sub-agent's final answer as the tool result.
- Persist agents (prompt + notes) across REPL sessions.
