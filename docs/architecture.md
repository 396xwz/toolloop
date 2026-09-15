# Architecture

`cmd/toolloop/main.go` is the composition root. It parses flags, selects a
backend, initializes memory, RAG, and tools, and dispatches batch or REPL work.
The Ollama backend lives in `llm_runtime_factory.go`, alongside the existing
llama.cpp backend in `llm_runtime.go`. REPL routing is in `repl.go`, `/git` rendering is in
`git.go`, and shared model/tool-call recovery, context formatting, and
filesystem policy helpers are in `heuristics.go`. Batch task execution remains
in `main.go` with configuration and the entrypoint.

The task loop is in `internal/engine`. Tool implementations and their registry
are in `internal/tools`; SQLite memory and RAG are in `internal/memory`.
Callers import these owning packages directly. The minimal model and chat-model
contracts live beside the task engine in `internal/engine`.
