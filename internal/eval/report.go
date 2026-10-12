package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type report struct {
	Suite     string         `json:"suite"`
	Component string         `json:"component"`
	Metrics   reportMetrics  `json:"metrics"`
	Cases     []reportResult `json:"cases"`
}

type reportMetrics struct {
	TotalCases   int                     `json:"total_cases"`
	Passed       int                     `json:"passed"`
	Failed       int                     `json:"failed"`
	PassRate     float64                 `json:"pass_rate"`
	AvgLatencyMS float64                 `json:"avg_latency_ms"`
	P50LatencyMS float64                 `json:"p50_latency_ms"`
	P95LatencyMS float64                 `json:"p95_latency_ms"`
	P99LatencyMS float64                 `json:"p99_latency_ms"`
	AvgCost      float64                 `json:"avg_cost_usd"`
	TotalCost    float64                 `json:"total_cost_usd"`
	ToolCalls    int                     `json:"tool_calls"`
	ToolErrors   int                     `json:"tool_errors"`
	PromptTokens int                     `json:"prompt_tokens"`
	OutputTokens int                     `json:"output_tokens"`
	TotalTokens  int                     `json:"total_tokens"`
	AvgToolCalls float64                 `json:"avg_tool_calls"`
	RepeatRate   float64                 `json:"repeat_rate_pct"`
	ScorerStats  map[string]ScorerMetric `json:"scorer_stats"`
}

type reportResult struct {
	CaseName     string            `json:"case_name"`
	Passed       bool              `json:"passed"`
	ScoreDetails []ScoreDetail     `json:"score_details"`
	Output       string            `json:"output"`
	Artifacts    map[string]string `json:"artifacts,omitempty"`
	ToolCalls    []reportToolCall  `json:"tool_calls,omitempty"`
	LatencyMS    float64           `json:"latency_ms"`
	PromptTokens int               `json:"prompt_tokens"`
	OutputTokens int               `json:"output_tokens"`
	TotalTokens  int               `json:"total_tokens"`
	Cost         float64           `json:"cost_usd"`
	Error        string            `json:"error,omitempty"`
}

type reportToolCall struct {
	Tool       string         `json:"tool"`
	Args       map[string]any `json:"args,omitempty"`
	DurationMS float64        `json:"duration_ms"`
	Repeated   bool           `json:"repeated"`
	Error      string         `json:"error,omitempty"`
}

func WriteReport(outputDir string, suite Suite, results []Result, metrics Metrics) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	base := reportBaseName(suite.Name)
	data := makeReport(suite, results, metrics)
	if err := writeJSONReport(filepath.Join(outputDir, base+".json"), data); err != nil {
		return err
	}
	if err := writeMarkdownReport(filepath.Join(outputDir, base+".md"), data); err != nil {
		return err
	}
	return nil
}

func makeReport(suite Suite, results []Result, metrics Metrics) report {
	data := report{
		Suite:     suite.Name,
		Component: suite.Component,
		Metrics: reportMetrics{
			TotalCases:   metrics.TotalCases,
			Passed:       metrics.Passed,
			Failed:       metrics.Failed,
			PassRate:     metrics.PassRate,
			AvgLatencyMS: durationMS(metrics.AvgLatency),
			P50LatencyMS: durationMS(metrics.P50Latency),
			P95LatencyMS: durationMS(metrics.P95Latency),
			P99LatencyMS: durationMS(metrics.P99Latency),
			AvgCost:      metrics.AvgCost,
			TotalCost:    metrics.TotalCost,
			ToolCalls:    metrics.ToolCalls,
			ToolErrors:   metrics.ToolErrors,
			PromptTokens: metrics.PromptTokens,
			OutputTokens: metrics.OutputTokens,
			TotalTokens:  metrics.TotalTokens,
			AvgToolCalls: metrics.AvgToolCalls,
			RepeatRate:   metrics.RepeatRate,
			ScorerStats:  metrics.ScorerStats,
		},
		Cases: make([]reportResult, 0, len(results)),
	}
	for _, result := range results {
		item := reportResult{
			CaseName:     result.CaseName,
			Passed:       result.Passed,
			ScoreDetails: result.ScoreDetails,
			Output:       result.Output,
			Artifacts:    result.Artifacts,
			LatencyMS:    durationMS(result.Latency),
			PromptTokens: result.PromptTokens,
			OutputTokens: result.OutputTokens,
			TotalTokens:  result.TotalTokens,
			Cost:         result.Cost,
		}
		if result.Err != nil {
			item.Error = result.Err.Error()
		}
		for _, call := range result.ToolCalls {
			reportCall := reportToolCall{
				Tool:       call.Tool,
				Args:       call.Args,
				DurationMS: durationMS(call.Duration),
				Repeated:   call.Repeated,
			}
			if call.Err != nil {
				reportCall.Error = call.Err.Error()
			}
			item.ToolCalls = append(item.ToolCalls, reportCall)
		}
		data.Cases = append(data.Cases, item)
	}
	return data
}

func writeJSONReport(path string, data report) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeMarkdownReport(path string, data report) error {
	var output strings.Builder
	fmt.Fprintf(&output, "# Evaluation: %s\n\n", data.Suite)
	fmt.Fprintf(&output, "- Component: `%s`\n", data.Component)
	fmt.Fprintf(&output, "- Cases: %d (%d passed, %d failed)\n", data.Metrics.TotalCases, data.Metrics.Passed, data.Metrics.Failed)
	fmt.Fprintf(&output, "- Pass rate: %.2f%%\n", data.Metrics.PassRate)
	fmt.Fprintf(&output, "- Latency (avg / p50 / p95 / p99): %.2f / %.2f / %.2f / %.2f ms\n", data.Metrics.AvgLatencyMS, data.Metrics.P50LatencyMS, data.Metrics.P95LatencyMS, data.Metrics.P99LatencyMS)
	fmt.Fprintf(&output, "- Cost (avg / total): $%.6f / $%.6f\n", data.Metrics.AvgCost, data.Metrics.TotalCost)
	fmt.Fprintf(&output, "- Tokens (prompt / output / total): %d / %d / %d\n", data.Metrics.PromptTokens, data.Metrics.OutputTokens, data.Metrics.TotalTokens)
	fmt.Fprintf(&output, "- Tool calls: %d (%d errors)\n\n", data.Metrics.ToolCalls, data.Metrics.ToolErrors)

	output.WriteString("## Cases\n\n| Case | Result | Latency | Tokens (prompt / output / total) | Cost | Error |\n|---|---:|---:|---:|---:|---|\n")
	for _, result := range data.Cases {
		status := "fail"
		if result.Passed {
			status = "pass"
		}
		fmt.Fprintf(&output, "| %s | %s | %.2f ms | %d / %d / %d | $%.6f | %s |\n",
			escapeMarkdown(result.CaseName), status, result.LatencyMS,
			result.PromptTokens, result.OutputTokens, result.TotalTokens,
			result.Cost, escapeMarkdown(result.Error))
	}

	hasOutput := false
	for _, result := range data.Cases {
		if result.Output != "" {
			hasOutput = true
			break
		}
	}
	if hasOutput {
		output.WriteString("\n## Outputs\n")
		for _, result := range data.Cases {
			if result.Output == "" {
				continue
			}
			fmt.Fprintf(&output, "\n### %s\n\n    %s\n",
				escapeMarkdown(result.CaseName),
				strings.ReplaceAll(result.Output, "\n", "\n    "))
		}
	}

	if len(data.Metrics.ScorerStats) > 0 {
		output.WriteString("\n## Scorers\n\n| Scorer | Pass rate | Average score |\n|---|---:|---:|\n")
		names := make([]string, 0, len(data.Metrics.ScorerStats))
		for name := range data.Metrics.ScorerStats {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			stat := data.Metrics.ScorerStats[name]
			fmt.Fprintf(&output, "| %s | %.2f%% | %.4f |\n", escapeMarkdown(name), stat.PassRate, stat.AvgScore)
		}
	}
	return os.WriteFile(path, []byte(output.String()), 0o644)
}

func durationMS(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}

func reportBaseName(name string) string {
	name = strings.TrimSpace(name)
	var output strings.Builder
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			output.WriteRune(char)
		} else {
			output.WriteByte('-')
		}
	}
	base := strings.Trim(output.String(), "-")
	if base == "" {
		return "evaluation"
	}
	return base
}

func escapeMarkdown(value string) string {
	value = strings.ReplaceAll(value, "|", `\|`)
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}
