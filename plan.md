# Plan: split the CLI and collapse pass-through packages

## Goal

Implement only Suggested order of work #1 from `REVIEW.md`:

1. Split the CLI implementation into focused files without changing behavior.
2. Move `ChatModel` beside `Engine` in `internal/engine`.
3. Replace pass-through package usage with direct `engine`, `tools`, and
   `memory` imports, then remove the obsolete pass-through packages.

This is an organizational refactor only. Runtime behavior, command output,
model prompts, safeguards, constants, and test semantics must remain unchanged.

## Scope constraints

- Do not address any other bug, design feedback, test gap, or hygiene item from
  `REVIEW.md`.
- Do not alter `/git`, REPL, Ollama/llama.cpp, filesystem heuristic, batch task,
  memory, RAG, or tool behavior.
- Do not change exported APIs in `internal/tools` or `internal/memory`.
- Do not add dependencies.
- Keep `REVIEW.md` untouched and untracked.
- Preserve unrelated working-tree changes.

## Final layout

### `cmd/toolloop`

- `main.go`: application configuration and constants, debug support,
  `stringSlice`, batch execution (`runTasks`), and the `main` entrypoint.
- `llm_runtime_factory.go`: `OllamaModel`, Ollama tool definitions and tool-call conversion,
  planning, and final-answer generation.
- `llm_runtime.go`: llama.cpp client/model/embedder and llama.cpp response handling.
- `repl.go`: REPL input validation, direct-tool dispatch, REPL loop, and REPL
  command handling.
- `git.go`: Git limits/version state and `/git` command helpers.
- `git_process_*.go`: platform-specific Git process cancellation.
- `heuristics.go`: shared tool argument parsing and recovery, RAG/memory
  formatting, knowledge-question detection, filesystem path/operation guards,
  and generic normalization/truncation helpers.
- `agents.go`: agent command family, conversations, notes, and role loading.

There are no separate `toolargs.go`, `fsguards.go`, or `tasks.go` files.

### `internal`

- `engine`: task/step/tool-call types, execution loop, and the `Model` and
  `ChatModel` contracts.
- `tools`: tool interfaces, registry, and concrete tools.
- `memory`: memory, RAG, and embedder contracts and implementations.

The pass-through `internal/agent`, `internal/model`, and `internal/prompt`
packages are removed. CLI code imports the owning packages directly.

## Implementation

1. Add the existing `ChatModel` contract to `internal/engine` beside `Model`.
2. Replace CLI alias-package imports and type references with direct imports
   from `internal/engine`, `internal/tools`, and `internal/memory`.
3. Pure-move Ollama, REPL, Git, and shared heuristic code into the final files
   listed above.
4. Keep configuration, batch execution, and entrypoint code together in
   `cmd/toolloop/main.go`.
5. Delete the obsolete pass-through packages and update directly affected
   repository-layout documentation.
6. Format and validate the refactor without changing behavior.

## Validation

1. Run `gofmt` on changed Go files.
2. Verify no source or documentation references remain to `internal/agent`,
   `internal/model`, or `internal/prompt`.
3. Run targeted `cmd/toolloop` tests and `go test ./...`.
4. Run `go vet ./...` and `go build ./cmd/toolloop`.
5. Run a Windows cross-build of `./cmd/toolloop`, placing and removing the
   artifact within the repository.
6. Run `git diff --check` and review final status/diff.

## Acceptance criteria

- `cmd/toolloop/main.go` retains configuration, batch execution, and the
  entrypoint.
- `cmd/toolloop/llm_runtime_factory.go`, `repl.go`, `git.go`, and `heuristics.go` exist with
  the responsibilities documented above.
- `cmd/toolloop/toolargs.go`, `fsguards.go`, and `tasks.go` do not exist.
- `filterRAGChunks`, `formatRAGContext`, `formatMemoryContext`, and
  `isKnowledgeQuestion` are defined in `heuristics.go`.
- `engine.ChatModel` is the single chat-model contract.
- CLI callers import `engine`, `tools`, and `memory` directly.
- `internal/agent`, `internal/model`, and `internal/prompt` are deleted with no
  remaining references.
- Existing function bodies, behavior, and tests remain unchanged.
- Targeted tests, full tests, vet, native build, Windows cross-build, and
  `git diff --check` pass.
- `REVIEW.md` remains unmodified.
