// Package eval — rubrics: weighted multi-criterion scoring with optional LLM judge.
package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// --- Data model ---

type Criterion struct {
	Name        string  `yaml:"name"`
	Description string  `yaml:"description"`
	Weight      float64 `yaml:"weight"` // should sum to 1.0 across criteria
	Type        string  `yaml:"type"`   // "llm" or "deterministic"
	Check       string  `yaml:"check,omitempty"`
	Scale       int     `yaml:"scale,omitempty"` // max score, default 1 (binary)
}

// scale returns the effective maximum for a criterion (0 normalizes to 1).
func (c Criterion) scale() int {
	if c.Scale <= 0 {
		return 1
	}
	return c.Scale
}

type Rubric struct {
	Name      string      `yaml:"name"`
	Threshold float64     `yaml:"threshold"` // e.g. 0.8
	Criteria  []Criterion `yaml:"criteria"`
}

type CriterionResult struct {
	Name       string
	Score      float64 // normalized 0-1
	Rationale  string
	JudgeModel string
	Err        error `json:"-"`
}

// --- Parsing ---

const weightSumTolerance = 1e-6

func ParseRubric(data []byte) (*Rubric, error) {
	var r Rubric
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("rubric: %w", err)
	}
	if strings.TrimSpace(r.Name) == "" {
		return nil, fmt.Errorf("rubric: name is required")
	}
	if len(r.Criteria) == 0 {
		return nil, fmt.Errorf("rubric: at least one criterion is required")
	}

	var sum float64
	for i, c := range r.Criteria {
		switch c.Type {
		case "llm":
			if strings.TrimSpace(c.Description) == "" {
				return nil, fmt.Errorf("rubric: criterion %d (%s): llm type requires a description", i+1, c.Name)
			}
		case "deterministic":
			if _, _, err := ParseCheck(c.Check); err != nil {
				return nil, fmt.Errorf("rubric: criterion %d (%s): %v", i+1, c.Name, err)
			}
		default:
			return nil, fmt.Errorf("rubric: criterion %d (%s): unknown type %q (want \"llm\" or \"deterministic\")", i+1, c.Name, c.Type)
		}
		if c.Scale < 0 {
			return nil, fmt.Errorf("rubric: criterion %d (%s): scale must not be negative", i+1, c.Name)
		}
		sum += c.Weight
	}
	if diff := sum - 1.0; diff > weightSumTolerance || diff < -weightSumTolerance {
		return nil, fmt.Errorf("rubric: weights must sum to 1.0 (got %v)", sum)
	}
	return &r, nil
}

// ParseCheck splits a deterministic check name. Supported forms:
// "json_valid", "contains:<substr>", "regex:<pattern>".
func ParseCheck(check string) (name, arg string, err error) {
	if check == "json_valid" {
		return "json_valid", "", nil
	}
	switch {
	case strings.HasPrefix(check, "contains:"):
		return "contains", strings.TrimPrefix(check, "contains:"), nil
	case strings.HasPrefix(check, "regex:"):
		return "regex", strings.TrimPrefix(check, "regex:"), nil
	default:
		return "", "", fmt.Errorf("unknown check %q (want json_valid, contains:X or regex:X)", check)
	}
}

// --- Deterministic checks ---

func ScoreDeterministic(c Criterion, output string) CriterionResult {
	name, arg, err := ParseCheck(c.Check)
	if err != nil {
		return CriterionResult{Name: c.Name, Rationale: "invalid_check: " + err.Error()}
	}
	var ok bool
	switch name {
	case "json_valid":
		ok = json.Valid([]byte(strings.TrimSpace(output))) // strict — no fence stripping
	case "contains":
		ok = strings.Contains(output, arg) // case-sensitive substring
	case "regex":
		re, err := regexp.Compile(arg)
		if err == nil {
			ok = re.MatchString(output)
		}
	}
	score := 0.0
	if ok {
		score = 1.0
	}
	return CriterionResult{Name: c.Name, Score: score, Rationale: c.Check}
}

// --- LLM judge ---

type JudgeFn func(prompt string) (string, error) // judge model call, provided by caller

// runJudge retries invalid verdicts once, but never retries transport errors or
// valid verdicts (including zero scores).
func runJudge(judge JudgeFn, prompt string, parse func(string) error) error {
	if judge == nil {
		return fmt.Errorf("judge function is not configured")
	}
	for attempt := 0; attempt < 2; attempt++ {
		body, err := judge(prompt)
		if err != nil {
			return err
		}
		if err := parse(body); err != nil {
			if attempt == 1 {
				return fmt.Errorf("invalid judge response after 2 attempts: %w", err)
			}
			continue
		}
		return nil
	}
	return nil
}

const judgePromptTemplate = `You are grading an agent's output against ONE criterion. Be strict.
Task prompt:
{{task_prompt}}
Criterion: {{criterion_name}} — {{criterion_description}}
Scale: 0 to {{scale}}. {{scale_desc}}
Tool calls:
{{tool_calls}}
Artifacts:
{{artifacts}}
Agent output:
{{output}}
Reply ONLY with JSON: {"score": <int>, "rationale": "<one sentence>"}`

// BuildJudgePrompt fills the judge template for one llm-type criterion.
func BuildJudgePrompt(taskPrompt, output, toolCalls, artifacts string, c Criterion) string {
	toolCallsOrDash := toolCalls
	if strings.TrimSpace(toolCalls) == "" {
		toolCallsOrDash = "-"
	}
	artifactsOrDash := artifacts
	if strings.TrimSpace(artifacts) == "" {
		artifactsOrDash = "-"
	}
	scaleDesc := "0 means fail and 1 means full pass"
	if c.scale() != 1 {
		scaleDesc = fmt.Sprintf("0 is fully incorrect, %d is fully correct", c.scale())
	}
	return strings.NewReplacer(
		"{{task_prompt}}", taskPrompt,
		"{{criterion_name}}", c.Name,
		"{{criterion_description}}", c.Description,
		"{{scale}}", fmt.Sprintf("%d", c.scale()),
		"{{scale_desc}}", scaleDesc,
		"{{tool_calls}}", toolCallsOrDash,
		"{{artifacts}}", artifactsOrDash,
		"{{output}}", output,
	).Replace(judgePromptTemplate)
}

// ParseJudgeResponse extracts the first {...} JSON object from a judge reply,
// tolerating code fences and surrounding prose.
func ParseJudgeResponse(body string) (score int, rationale string, err error) {
	data, err := judgeJSON(body)
	if err != nil {
		return 0, "", err
	}
	var out struct {
		Score     *int   `json:"score"`
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, "", fmt.Errorf("invalid judge JSON: %w", err)
	}
	if out.Score == nil {
		return 0, "", fmt.Errorf("judge score is required")
	}
	if *out.Score < 0 {
		return 0, "", fmt.Errorf("score must be >= 0 (got %d)", *out.Score)
	}
	return *out.Score, out.Rationale, nil
}

func judgeJSON(body string) ([]byte, error) {
	start := strings.IndexByte(body, '{')
	if start < 0 {
		return nil, fmt.Errorf("no JSON object found in judge response")
	}
	end := firstBraceEnd(body[start:])
	if end <= 0 {
		return nil, fmt.Errorf("unterminated JSON object in judge response")
	}
	return []byte(body[start : start+end]), nil
}

// firstBraceEnd returns the offset just past the '}' closing s's first '{',
// ignoring braces inside JSON string literals. Returns -1 if unbalanced.
func firstBraceEnd(s string) int {
	depth := 0
	inStr, esc := false, false
	for i, r := range s {
		switch {
		case inStr:
			if esc {
				esc = false
			} else if r == '\\' {
				esc = true
			} else if r == '"' {
				inStr = false
			}
		case r == '"':
			inStr = true
		case r == '{':
			depth++
		case r == '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// --- Evaluation ---

// EvaluateRubric scores every criterion: deterministic checks run locally;
// llm criteria retry invalid verdicts once. Judge failures never abort the rubric;
// the affected criterion records an error and scores 0 with a diagnostic rationale.
func EvaluateRubric(r *Rubric, taskPrompt, output, toolCalls, artifacts string, judge JudgeFn, judgeModel string) ([]CriterionResult, float64) {
	results := make([]CriterionResult, 0, len(r.Criteria))
	for _, c := range r.Criteria {
		if c.Type == "deterministic" {
			results = append(results, ScoreDeterministic(c, output))
			continue
		}
		res := CriterionResult{Name: c.Name}
		if judge == nil {
			res.Rationale = "no_judge_configured"
		} else {
			var raw int
			var rationale string
			err := runJudge(judge, BuildJudgePrompt(taskPrompt, output, toolCalls, artifacts, c), func(body string) error {
				var err error
				raw, rationale, err = ParseJudgeResponse(body)
				return err
			})
			if err != nil {
				res.Err = fmt.Errorf("judge_error: %w", err)
				res.Rationale = res.Err.Error()
			} else {
				if raw > c.scale() {
					raw = c.scale() // clamp to [0, scale]
				}
				res.Score = float64(raw) / float64(c.scale())
				res.Rationale = rationale
				res.JudgeModel = judgeModel
			}
		}
		results = append(results, res)
	}
	return results, ScoreRubric(r, results)
}

// ScoreRubric computes the weighted total: Σ raw/scale * weight.
func ScoreRubric(r *Rubric, results []CriterionResult) float64 {
	total := 0.0
	for i, c := range r.Criteria {
		if i >= len(results) {
			break
		}
		total += results[i].Score * c.Weight
	}
	return total
}

// Passed reports whether a weighted total meets the rubric threshold.
func (r *Rubric) Passed(total float64) bool { return total >= r.Threshold }

// RubricVersion returns "name-<sha256 first 8 hex>" over the name and every
// criterion field, so any semantic change (e.g. a description edit) changes it.
func RubricVersion(r *Rubric) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", r.Name)
	for _, c := range r.Criteria {
		fmt.Fprintf(h, "%s|%s|%.17g|%s|%s|%d\n", c.Name, c.Description, c.Weight, c.Type, c.Check, c.Scale)
	}
	return fmt.Sprintf("%s-%.8s", r.Name, hex.EncodeToString(h.Sum(nil)))
}
