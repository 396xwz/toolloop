package model

import (
	"context"

	"github.com/396xwz/toolloop/internal/engine"
)

// Model is the minimum contract required by the task engine.
type Model interface {
	PlanNextStep(context.Context, *engine.Task) (*engine.Step, error)
}

// ChatModel adds final-answer generation and runtime prompt/model selection.
type ChatModel interface {
	Model
	GenerateFinalAnswer(context.Context, *engine.Task) (string, error)
	ModelName() string
	SetModel(string)
	SystemPromptValue() string
	SetSystemPrompt(string)
}
