package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// --- Exact match ---

type ExactScorer struct{ Want string }

func (s ExactScorer) Name() string { return "exact" }
func (s ExactScorer) Score(_ context.Context, _ Input, r Result) ScoreDetail {
	pass := strings.TrimSpace(r.Output) == strings.TrimSpace(s.Want)
	return ScoreDetail{ScorerName: "exact", Passed: pass, Score: boolScore(pass),
		Reason: outputDiff(s.Want, r.Output)}
}

// --- Contains ---

type ContainsScorer struct{ Substrings []string }

func (s ContainsScorer) Name() string { return "contains" }
func (s ContainsScorer) Score(_ context.Context, _ Input, r Result) ScoreDetail {
	for _, sub := range s.Substrings {
		if !strings.Contains(r.Output, sub) {
			return ScoreDetail{ScorerName: "contains", Passed: false, Score: 0,
				Reason: "missing: " + sub}
		}
	}
	return ScoreDetail{ScorerName: "contains", Passed: true, Score: 1.0}
}

// --- Regex ---

type RegexScorer struct{ Pattern string }

func (s RegexScorer) Name() string { return "regex" }
func (s RegexScorer) Score(_ context.Context, _ Input, r Result) ScoreDetail {
	re, err := regexp.Compile(s.Pattern)
	if err != nil {
		return ScoreDetail{ScorerName: s.Name(), Reason: "invalid pattern: " + err.Error()}
	}
	pass := re.MatchString(r.Output)
	return ScoreDetail{ScorerName: "regex", Passed: pass, Score: boolScore(pass),
		Reason: "pattern: " + s.Pattern}
}

// --- Artifact present ---

type ArtifactScorer struct {
	Key     string // which artifact to check
	Pattern string // optional regex; if empty, just checks non-empty
}

func (s ArtifactScorer) Name() string { return "artifact:" + s.Key }
func (s ArtifactScorer) Score(_ context.Context, _ Input, r Result) ScoreDetail {
	val, ok := r.Artifacts[s.Key]
	if !ok || val == "" {
		return ScoreDetail{ScorerName: s.Name(), Passed: false, Score: 0,
			Reason: "artifact not produced"}
	}
	if s.Pattern != "" {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return ScoreDetail{ScorerName: s.Name(), Reason: "invalid pattern: " + err.Error()}
		}
		pass := re.MatchString(val)
		return ScoreDetail{ScorerName: s.Name(), Passed: pass, Score: boolScore(pass)}
	}
	return ScoreDetail{ScorerName: s.Name(), Passed: true, Score: 1.0}
}

// --- Tool call count ---

type ToolCallCountScorer struct {
	Tool string
	Min  int
	Max  int // -1 = unlimited
}

func (s ToolCallCountScorer) Name() string { return "tool_calls:" + s.Tool }
func (s ToolCallCountScorer) Score(_ context.Context, _ Input, r Result) ScoreDetail {
	count := 0
	for _, tc := range r.ToolCalls {
		if tc.Tool == s.Tool {
			count++
		}
	}
	pass := count >= s.Min && (s.Max == -1 || count <= s.Max)
	return ScoreDetail{ScorerName: s.Name(), Passed: pass, Score: boolScore(pass),
		Reason: fmt.Sprintf("got %d calls (want %d–%d)", count, s.Min, s.Max)}
}

// --- Structural tool call ---

type ToolCallScorer struct {
	Tool string
	Args map[string]string
}

func (s ToolCallScorer) Name() string { return "tool_call:" + s.Tool }
func (s ToolCallScorer) Score(_ context.Context, _ Input, r Result) ScoreDetail {
	for _, call := range r.ToolCalls {
		if call.Tool != s.Tool || len(call.Args) != len(s.Args) {
			continue
		}
		matches := true
		for key, want := range s.Args {
			got, ok := call.Args[key]
			if !ok || fmt.Sprint(got) != want {
				matches = false
				break
			}
		}
		if matches {
			return ScoreDetail{
				ScorerName: s.Name(),
				Passed:     true,
				Score:      1,
				Reason:     fmt.Sprintf("matched %s %v", s.Tool, s.Args),
			}
		}
	}
	return ScoreDetail{
		ScorerName: s.Name(),
		Reason:     fmt.Sprintf("no call matched %s %v", s.Tool, s.Args),
	}
}

// --- Tool error ---

type ToolErrorScorer struct {
	Tool     string
	Contains string
}

func (s ToolErrorScorer) Name() string { return "tool_error:" + s.Tool }
func (s ToolErrorScorer) Score(_ context.Context, _ Input, r Result) ScoreDetail {
	for _, call := range r.ToolCalls {
		if call.Tool != s.Tool || call.Err == nil {
			continue
		}
		if s.Contains != "" && !strings.Contains(call.Err.Error(), s.Contains) {
			continue
		}
		return ScoreDetail{
			ScorerName: s.Name(),
			Passed:     true,
			Score:      1,
			Reason:     call.Err.Error(),
		}
	}
	reason := "tool did not return an error"
	if s.Contains != "" {
		reason = fmt.Sprintf("tool did not return an error containing %q", s.Contains)
	}
	return ScoreDetail{ScorerName: s.Name(), Reason: reason}
}

// --- No repeated tool calls ---

type NoRepeatScorer struct{}

func (s NoRepeatScorer) Name() string { return "no_repeat" }
func (s NoRepeatScorer) Score(_ context.Context, _ Input, r Result) ScoreDetail {
	for _, tc := range r.ToolCalls {
		if tc.Repeated {
			return ScoreDetail{ScorerName: "no_repeat", Passed: false, Score: 0,
				Reason: fmt.Sprintf("repeat detected: tool=%s", tc.Tool)}
		}
	}
	return ScoreDetail{ScorerName: "no_repeat", Passed: true, Score: 1.0}
}

// --- Latency budget ---

type LatencyScorer struct{ Max time.Duration }

func (s LatencyScorer) Name() string { return "latency" }
func (s LatencyScorer) Score(_ context.Context, _ Input, r Result) ScoreDetail {
	pass := r.Latency <= s.Max
	return ScoreDetail{ScorerName: "latency", Passed: pass,
		Score:  boolScore(pass),
		Reason: fmt.Sprintf("%v (limit %v)", r.Latency, s.Max)}
}

// --- LLM-as-judge ---
// Sends (input.Task + result.Output) to a cheap model and asks it to score 0–10.

type LLMJudgeScorer struct {
	Criterion  string  // e.g. "Does the output contain valid Go syntax?"
	MinScore   float64 // pass threshold (0–10 scale)
	JudgeModel string  // e.g. "qwen2.5:7b" (cheap, fast)
	Judge      JudgeFunc
}

type JudgeFunc func(ctx context.Context, model, prompt string) (string, error)

func (s LLMJudgeScorer) Name() string { return "llm_judge:" + s.Criterion }
func (s LLMJudgeScorer) Score(ctx context.Context, input Input, r Result) ScoreDetail {
	prompt := fmt.Sprintf(`
Task: %s

Output:
%s

Criterion: %s
Score the output from 0 to 10. Reply with only a JSON object:
{"score": <number>, "reason": "<one sentence>"}
`, input.Task, r.Output, s.Criterion)

	var response struct {
		Score     *float64 `json:"score"`
		Reason    string   `json:"reason"`
		Rationale string   `json:"rationale"`
	}
	var judge JudgeFn
	if s.Judge != nil {
		judge = func(prompt string) (string, error) { return s.Judge(ctx, s.JudgeModel, prompt) }
	}
	err := runJudge(judge, prompt, func(raw string) error {
		body, err := judgeJSON(raw)
		if err != nil {
			return err
		}
		response.Score = nil
		response.Reason = ""
		response.Rationale = ""
		if err := json.Unmarshal(body, &response); err != nil {
			return fmt.Errorf("invalid judge JSON: %w", err)
		}
		if response.Score == nil {
			return fmt.Errorf("judge score is required")
		}
		if *response.Score < 0 {
			return fmt.Errorf("score must be >= 0 (got %v)", *response.Score)
		}
		return nil
	})
	if err != nil {
		judgeErr := fmt.Errorf("judge_error: %w", err)
		return ScoreDetail{ScorerName: s.Name(), Reason: judgeErr.Error(), Err: judgeErr}
	}
	score := max(0, min(10, *response.Score))
	pass := score >= s.MinScore
	if response.Reason == "" {
		response.Reason = response.Rationale
	}
	return ScoreDetail{
		ScorerName: s.Name(),
		Passed:     pass,
		Score:      score / 10.0,
		Reason:     response.Reason,
	}
}

func outputDiff(want, got string) string {
	if strings.TrimSpace(want) == strings.TrimSpace(got) {
		return ""
	}
	return fmt.Sprintf("want %q, got %q", strings.TrimSpace(want), strings.TrimSpace(got))
}
