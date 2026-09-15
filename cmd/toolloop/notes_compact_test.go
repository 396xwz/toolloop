package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/396xwz/toolloop/internal/engine"
)

// mockChatModel implements engine.ChatModel and returns a canned summary from
// GenerateFinalAnswer, recording the system prompt active during the call.
type mockChatModel struct {
	summary       string
	err           error
	sysPrompt     string
	lastSysPrompt string
}

func (m *mockChatModel) PlanNextStep(ctx context.Context, task *engine.Task) (*engine.Step, error) {
	return &engine.Step{}, nil
}

func (m *mockChatModel) GenerateFinalAnswer(ctx context.Context, task *engine.Task) (string, error) {
	m.lastSysPrompt = m.sysPrompt
	return m.summary, m.err
}

func (m *mockChatModel) SetSystemPrompt(p string)  { m.sysPrompt = p }
func (m *mockChatModel) SystemPromptValue() string { return m.sysPrompt }

func (m *mockChatModel) ModelName() string    { return "mock" }
func (m *mockChatModel) SetModel(name string) {}

func (a *replAgent) fillNotes(n int) {
	for i := range n {
		a.appendNote("question "+strconv.Itoa(i), "answer "+strconv.Itoa(i))
	}
}

// CompactNotes folds older turns into the LLM summary, trims the raw window to
// keepRecentTurns, uses the dedicated compact prompt during the call, and
// restores the original system prompt afterwards.
func TestCompactNotesFoldsOlderTurnsIntoSummary(t *testing.T) {
	a := &replAgent{Name: "t"}
	model := &mockChatModel{summary: "Decided to use SQLite for memory."}
	a.fillNotes(compactTriggerTurns + 2) // 12 raw turns

	if err := a.compactNotes(context.Background(), model); err != nil {
		t.Fatalf("compactNotes: %v", err)
	}

	if model.lastSysPrompt != compactSystemPrompt {
		t.Errorf("summary call system prompt = %q, want %q", model.lastSysPrompt, compactSystemPrompt)
	}
	if model.sysPrompt != "" {
		t.Errorf("system prompt not restored, = %q, want empty", model.sysPrompt)
	}
	if !strings.Contains(a.notesString(), "Earlier conversation (summary):") {
		t.Errorf("notesString missing summary header:\n%s", a.notesString())
	}
	if !strings.Contains(a.notesString(), "Decided to use SQLite for memory.") {
		t.Errorf("notesString missing folded summary text:\n%s", a.notesString())
	}
	if got := len(a.notes.recent); got != keepRecentTurns {
		t.Errorf("recent = %d turns, want %d", got, keepRecentTurns)
	}
	// The oldest raw turns are folded away; the newest remain.
	if strings.Contains(a.notesString(), "Q: question 0\n") {
		t.Errorf("folded turn still present as raw:\n%s", a.notesString())
	}
	if !strings.Contains(a.notesString(), "Q: question 11\n") {
		t.Errorf("newest turn missing from buffer:\n%s", a.notesString())
	}
}

// On model failure the summary is left unset and the raw buffer is trimmed to
// a bounded size (at most 2x the normal window) so it cannot grow unbounded.
func TestCompactNotesFallbackBoundedTrimOnModelError(t *testing.T) {
	a := &replAgent{Name: "t"}
	model := &mockChatModel{err: errors.New("model down")}
	a.fillNotes(compactTriggerTurns + 4) // 14 raw turns

	if err := a.compactNotes(context.Background(), model); err == nil {
		t.Fatal("expected error to propagate from model failure")
	}
	if a.notes.summary != "" {
		t.Errorf("summary set despite model error: %q", a.notes.summary)
	}
	if got := len(a.notes.recent); got > keepRecentTurns*2 {
		t.Errorf("recent = %d, want <= %d after fallback trim", got, keepRecentTurns*2)
	}
}

// Below the trigger, compaction is a no-op: no summary, no trimming.
func TestCompactNotesNoopBelowTrigger(t *testing.T) {
	a := &replAgent{Name: "t"}
	model := &mockChatModel{summary: "should not be called"}
	a.fillNotes(compactTriggerTurns)

	if err := a.compactNotes(context.Background(), model); err != nil {
		t.Fatalf("compactNotes: %v", err)
	}
	if a.notes.summary != "" {
		t.Errorf("summary set below trigger: %q", a.notes.summary)
	}
	if got := len(a.notes.recent); got != compactTriggerTurns {
		t.Errorf("recent = %d, want %d (unchanged)", got, compactTriggerTurns)
	}
	if model.lastSysPrompt != "" {
		t.Errorf("GenerateFinalAnswer was called below trigger")
	}
}
