package suites

import (
	"time"

	"github.com/396xwz/toolloop/internal/eval"
)

const FilesystemToolSuite = "fs-tool"

// NewFilesystemToolSuite evaluates filesystem operations through direct,
// deterministic tool calls rather than model-generated instructions.
func NewFilesystemToolSuite() eval.Suite {
	return eval.Suite{
		Name:      FilesystemToolSuite,
		Component: "tool:fs",
		Cases: []eval.Case{
			{
				Name: "fs-read-existing-file",
				Input: eval.Input{
					Files: map[string]string{"hello.txt": "Hello, World!\n"},
					Task:  `{"tool":"fs","args":{"op":"read","path":"hello.txt"}}`,
				},
				Scorers: []eval.Scorer{
					eval.ContainsScorer{Substrings: []string{"Hello, World!"}},
					eval.ToolCallScorer{Tool: "fs", Args: map[string]string{"op": "read", "path": "hello.txt"}},
					eval.LatencyScorer{Max: time.Second},
				},
			},
			{
				Name: "fs-write-file",
				Input: eval.Input{
					Task: `{"tool":"fs","args":{"op":"write","path":"main.go","content":"package main\n"}}`,
				},
				Scorers: []eval.Scorer{
					eval.ExactScorer{Want: "written successfully"},
					eval.ArtifactScorer{Key: "content", Pattern: `^package main\n$`},
				},
			},
			{
				Name: "fs-edit-unique-substring",
				Input: eval.Input{
					Files: map[string]string{"src.go": "func add(a, b int) int { return a + b }\n"},
					Task:  `{"tool":"fs","args":{"op":"edit","path":"src.go","old_snippet":"return a + b","new_snippet":"return a + b + 0"}}`,
				},
				Scorers: []eval.Scorer{
					eval.ContainsScorer{Substrings: []string{"edited successfully", "-func add", "+func add"}},
					eval.ArtifactScorer{Key: "content", Pattern: `return a \+ b \+ 0`},
				},
			},
			{
				Name: "fs-read-missing-file-errors",
				Input: eval.Input{
					Task: `{"tool":"fs","args":{"op":"read","path":"does-not-exist.txt"}}`,
				},
				Scorers: []eval.Scorer{
					eval.ToolErrorScorer{Tool: "fs", Contains: "does-not-exist.txt"},
				},
			},
			{
				Name: "fs-edit-ambiguous-snippet-errors",
				Input: eval.Input{
					Files: map[string]string{"dup.go": "a()\na()\n"},
					Task:  `{"tool":"fs","args":{"op":"edit","path":"dup.go","old_snippet":"a()","new_snippet":"b()"}}`,
				},
				Scorers: []eval.Scorer{
					eval.ToolErrorScorer{Tool: "fs", Contains: "found 2 matches"},
					eval.ArtifactScorer{Key: "content", Pattern: `^a\(\)\na\(\)\n$`},
				},
			},
		},
	}
}

var FsSuite = NewFilesystemToolSuite()
