package eval

import (
	"errors"
	"strings"
	"testing"
)

const validRubricYAML = `
name: task
threshold: 0.8
criteria:
  - name: correctness
    description: "Output calls the right tool with valid arguments"
    weight: 0.5
    type: llm
    scale: 2
  - name: valid_json
    description: "Output parses as JSON"
    weight: 0.2
    type: deterministic
    check: json_valid
  - name: no_hallucinated_fields
    description: "No fields invented beyond the tool schema"
    weight: 0.3
    type: llm
    scale: 2
`

func TestParseRubricValid(t *testing.T) {
	r, err := ParseRubric([]byte(validRubricYAML))
	if err != nil {
		t.Fatalf("ParseRubric: %v", err)
	}
	if r.Name != "task" || r.Threshold != 0.8 || len(r.Criteria) != 3 {
		t.Fatalf("unexpected rubric: %+v", r)
	}
	c := r.Criteria[0]
	if c.Type != "llm" || c.Scale != 2 || c.Weight != 0.5 {
		t.Errorf("criterion 0 = %+v", c)
	}
}

func TestParseRubricInvalid(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(doc string) string
		errPart string
	}{
		{
			name: "bad weight sum",
			mutate: func(doc string) string {
				return strings.ReplaceAll(doc, "weight: 0.5", "weight: 0.9") // sum 1.3
			},
			errPart: "sum to 1.0",
		},
		{
			name:    "unknown check type",
			mutate:  func(doc string) string { return strings.ReplaceAll(doc, "check: json_valid", "check: bogus_check") },
			errPart: `unknown check`,
		},
		{
			name: "missing description for llm",
			mutate: func(doc string) string {
				return strings.ReplaceAll(doc, `"Output calls the right tool with valid arguments"`, `""`)
			},
			errPart: "requires a description",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRubric([]byte(tt.mutate(validRubricYAML)))
			if err == nil || !strings.Contains(err.Error(), tt.errPart) {
				t.Fatalf("got %v, want error containing %q", err, tt.errPart)
			}
		})
	}
}

func TestParseCheck(t *testing.T) {
	tests := []struct {
		check, name, arg string
		wantErr          bool
	}{
		{"json_valid", "json_valid", "", false},
		{"contains:hello", "contains", "hello", false},
		{"regex:^a\\d+$", "regex", "^a\\d+$", false},
		{"unknown_check", "", "", true},
		{"startswith:x", "", "", true},
	}
	for _, tt := range tests {
		name, arg, err := ParseCheck(tt.check)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseCheck(%q): expected error", tt.check)
			}
			continue
		}
		if err != nil || name != tt.name || arg != tt.arg {
			t.Errorf("ParseCheck(%q) = (%q, %q, %v), want (%q, %q, nil)", tt.check, name, arg, err, tt.name, tt.arg)
		}
	}
}

func TestScoreDeterministic(t *testing.T) {
	tests := []struct {
		name, check, output string
		want                float64
	}{
		{"json_valid true", "json_valid", `{"a":1}`, 1},
		{"json_valid with whitespace", "json_valid", "\n\t{\"a\":1}\n", 1},
		{"json_valid false", "json_valid", `{not json`, 0},
		{"contains hit", "contains:Hello", "say Hello world", 1},
		{"contains miss (case-sensitive)", "contains:hello", "say Hello world", 0},
		{"regex hit", `regex:^a\d+$`, "a42", 1},
		{"regex miss", `regex:^b\d+$`, "a42", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ScoreDeterministic(Criterion{Name: "c", Check: tt.check}, tt.output)
			if res.Score != tt.want {
				t.Errorf("score = %v, want %v", res.Score, tt.want)
			}
			if res.Rationale != tt.check { // rationale is just the check name
				t.Errorf("rationale = %q, want %q", res.Rationale, tt.check)
			}
			if res.JudgeModel != "" {
				t.Errorf("judge model = %q, want empty", res.JudgeModel)
			}
		})
	}
}

func TestBuildJudgePrompt(t *testing.T) {
	c := Criterion{Name: "correctness", Description: "right tool", Scale: 2}
	got := BuildJudgePrompt("do the task", `{"ok":true}`, `- search(q=1)`, "", c)
	for _, want := range []string{
		"You are grading an agent's output against ONE criterion. Be strict.",
		"do the task", "correctness — right tool",
		"Scale: 0 to 2. 0 is fully incorrect, 2 is fully correct",
		"- search(q=1)",
		`{"ok":true}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
		t.Errorf("unfilled placeholder in prompt:\n%s", got)
	}

	bin := BuildJudgePrompt("", "", "", "", Criterion{Name: "n", Description: "d"}) // binary scale defaults to 1
	if !strings.Contains(bin, "Scale: 0 to 1. 0 means fail and 1 means full pass") {
		t.Errorf("binary scale_desc wrong:\n%s", bin)
	}
	if !strings.Contains(bin, "Tool calls:\n-") || !strings.Contains(bin, "Artifacts:\n-") {
		t.Errorf("empty tool_calls/artifacts should render as dash:\n%s", bin)
	}
}

func TestParseJudgeResponse(t *testing.T) {
	tests := []struct {
		name, body string
		wantScore  int
		wantRat    string
		wantErr    bool
	}{
		{"bare JSON", `{"score": 1, "rationale": "fine"}`, 1, "fine", false},
		{"fenced JSON", "```json\n{\"score\": 2, \"rationale\": \"good\"}\n```", 2, "good", false},
		{"prose-wrapped JSON", `Here is my grading: {"score": 0, "rationale": "bad"} as requested.`, 0, "bad", false},
		{"negative score", `{"score": -1, "rationale": "?"}`, 0, "", true},
		{"missing score", `{"rationale":"not a verdict"}`, 0, "", true},
		{"null score", `{"score":null}`, 0, "", true},
		{"wrong score type", `{"score":"1"}`, 0, "", true},
		{"noninteger score", `{"score":0.5}`, 0, "", true},
		{"malformed", "no json here at all", 0, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, rat, err := ParseJudgeResponse(tt.body)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got score=%d", score)
				}
				return
			}
			if err != nil || score != tt.wantScore || rat != tt.wantRat {
				t.Errorf("got (%d, %q, %v), want (%d, %q, nil)", score, rat, err, tt.wantScore, tt.wantRat)
			}
		})
	}
}

func TestEvaluateRubric(t *testing.T) {
	r := &Rubric{Threshold: 0.5, Criteria: []Criterion{
		{Name: "correctness", Description: "d", Weight: 0.6, Type: "llm", Scale: 2},
		{Name: "valid_json", Weight: 0.4, Type: "deterministic", Check: "json_valid"},
	}}

	judgeCalls := 0
	judge := func(prompt string) (string, error) {
		judgeCalls++
		return `{"score": 1, "rationale": "half right"}`, nil
	}
	results, total := EvaluateRubric(r, "task", `{"a":1}`, "", "", judge, "gpt-judge")

	if len(results) != 2 {
		t.Fatalf("got %d results", len(results))
	}
	if got := results[0].Score; got != 0.5 { // raw 1 / scale 2
		t.Errorf("llm score = %v, want 0.5", got)
	}
	if results[0].Rationale != "half right" || results[0].JudgeModel != "gpt-judge" {
		t.Errorf("llm result = %+v", results[0])
	}
	if got := results[1].Score; got != 1.0 { // json_valid on valid output
		t.Errorf("deterministic score = %v, want 1.0", got)
	}
	if judgeCalls != 1 {
		t.Errorf("judge called %d times, want exactly 1 (only for llm criterion)", judgeCalls)
	}
	wantTotal := 0.5*0.6 + 1.0*0.4 // 0.7
	if diff := total - wantTotal; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("total = %v, want %v", total, wantTotal)
	}

	// nil judge: llm criterion scores 0 with diagnostic rationale, rubric still completes.
	results, total = EvaluateRubric(r, "task", `{"a":1}`, "", "", nil, "")
	if results[0].Score != 0 || results[0].Rationale != "no_judge_configured" {
		t.Errorf("nil judge result = %+v", results[0])
	}
	wantTotal = 0.0*0.6 + 1.0*0.4 // 0.4
	if diff := total - wantTotal; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("nil judge total = %v, want %v", total, wantTotal)
	}

	// failing judge: error never aborts the rubric.
	badJudge := func(string) (string, error) { return "", errors.New("boom") }
	results, _ = EvaluateRubric(r, "task", `{"a":1}`, "", "", badJudge, "")
	if results[0].Score != 0 || !strings.Contains(results[0].Rationale, "judge_error: boom") {
		t.Errorf("failing judge result = %+v", results[0])
	}

	// clamping: raw score above scale is clamped.
	clampJudge := func(string) (string, error) { return `{"score": 9, "rationale": "over"}`, nil }
	results, _ = EvaluateRubric(r, "task", "", "", "", clampJudge, "")
	if results[0].Score != 1.0 { // 9 clamped to 2, /2 = 1.0
		t.Errorf("clamped score = %v, want 1.0", results[0].Score)
	}
}

func TestRubricPassed(t *testing.T) {
	r := &Rubric{Threshold: 0.8}
	tests := []struct {
		total float64
		want  bool
	}{
		{0.79, false}, // just below
		{0.8, true},   // boundary is pass (>=)
		{1.0, true},
	}
	for _, tt := range tests {
		if got := r.Passed(tt.total); got != tt.want {
			t.Errorf("Passed(%v) = %v, want %v", tt.total, got, tt.want)
		}
	}
}

func TestRubricVersion(t *testing.T) {
	r, err := ParseRubric([]byte(validRubricYAML))
	if err != nil {
		t.Fatal(err)
	}
	v1 := RubricVersion(r)
	if !strings.HasPrefix(v1, "task-") || len(v1) != len("task-")+8 {
		t.Fatalf("version = %q, want name-<8 hex>", v1)
	}
	if v2 := RubricVersion(r); v2 != v1 { // stable across calls
		t.Errorf("version not stable: %q vs %q", v1, v2)
	}
	r.Criteria[0].Description = "changed description"
	v3 := RubricVersion(r)
	if v3 == v1 {
		t.Errorf("version unchanged after description change: %q", v3)
	}
}
