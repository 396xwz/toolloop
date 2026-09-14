// Package agent provides the stable application-facing aliases for the
// engine, tools, and persistence packages.
package agent

import (
	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/memory"
	"github.com/396xwz/toolloop/internal/tools"
)

type (
	Task         = engine.Task
	TaskStatus   = engine.TaskStatus
	Step         = engine.Step
	ToolCall     = engine.ToolCall
	Model        = engine.Model
	Engine       = engine.Engine
	Tool         = tools.Tool
	ToolRegistry = tools.ToolRegistry
	Memory       = memory.Memory
	MemoryItem   = memory.MemoryItem
	RAG          = memory.RAG
	RAGChunk     = memory.RAGChunk
	Embedder     = memory.Embedder
)

const (
	TaskPending   = engine.TaskPending
	TaskRunning   = engine.TaskRunning
	TaskCompleted = engine.TaskCompleted
	TaskFailed    = engine.TaskFailed
)

var (
	NewToolRegistry = tools.NewToolRegistry
	NewSQLiteMemory = memory.NewSQLiteMemory
	NewSQLiteRAG    = memory.NewSQLiteRAG
)

type (
	WebSearchTool  = tools.WebSearchTool
	BrowserTool    = tools.BrowserTool
	ScrapeTool     = tools.ScrapeTool
	PythonTool     = tools.PythonTool
	FileSystemTool = tools.FileSystemTool
	ShellTool      = tools.ShellTool
	OllamaEmbedder = memory.OllamaEmbedder
)
