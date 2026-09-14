# Architecture

`cmd/toolloop` is the executable entry point. It parses flags, starts the
REPL, selects a backend, and wires the internal packages together.

The task loop is in `internal/engine`. Tool implementations and their registry
are in `internal/tools`; SQLite memory and RAG are in `internal/memory`.
`internal/agent` provides compatibility aliases for the application-facing
types while the package boundaries continue to settle. Model contracts live in
`internal/model`.

The llama.cpp and Ollama implementations currently remain in the CLI package
because they share prompt and REPL wiring. They implement the contracts in
`internal/model` and can be extracted into backend files without changing the
engine or tools.
