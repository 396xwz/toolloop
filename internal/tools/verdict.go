package tools

import (
	"context"
	"fmt"
	"strings"
)

// Verdict is the structured result of a task. The runner (or a future graph
// phase) routes on Status; Implicit is true when the engine produced the
// verdict (e.g. step exhaustion) rather than the model.
type Verdict struct {
	Status   string // VerdictOK or VerdictFail
	Reason   string
	Implicit bool
}

const (
	VerdictOK   = "ok"
	VerdictFail = "fail"
)

// ParseVerdict validates verdict tool arguments: status is required and must
// be ok|fail (case-insensitive); reason is optional.
func ParseVerdict(args map[string]string) (Verdict, error) {
	status := strings.ToLower(strings.TrimSpace(args["status"]))
	reason := strings.TrimSpace(args["reason"])
	switch status {
	case VerdictOK, VerdictFail:
		return Verdict{Status: status, Reason: reason}, nil
	default:
		return Verdict{}, fmt.Errorf("verdict status must be %q or %q, got %q", VerdictOK, VerdictFail, status)
	}
}

// VerdictTool is the model-facing tool that closes a task with a final status.
type VerdictTool struct{}

func (VerdictTool) Name() string { return "verdict" }

func (VerdictTool) Execute(_ context.Context, args map[string]string) (string, error) {
	v, err := ParseVerdict(args)
	if err != nil {
		return "", err
	}
	if v.Reason != "" {
		return fmt.Sprintf("verdict recorded: %s - %s", v.Status, v.Reason), nil
	}
	return "verdict recorded: " + v.Status, nil
}
