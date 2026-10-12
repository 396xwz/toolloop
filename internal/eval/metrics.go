package eval

import (
	"sort"
	"time"
)

// Metrics aggregates Results from a full Suite run.
type Metrics struct {
	SuiteName    string
	Component    string
	TotalCases   int
	Passed       int
	Failed       int
	PassRate     float64
	AvgLatency   time.Duration
	P50Latency   time.Duration
	P95Latency   time.Duration
	P99Latency   time.Duration
	AvgCost      float64
	TotalCost    float64
	ToolCalls    int
	ToolErrors   int
	PromptTokens int
	OutputTokens int
	TotalTokens  int
	AvgToolCalls float64 // avg tool calls per case
	RepeatRate   float64 // % of cases that triggered repeat detection
	ScorerStats  map[string]ScorerMetric
}

type ScorerMetric struct {
	PassRate float64
	AvgScore float64
}

func Aggregate(suiteName, component string, results []Result) Metrics {
	m := Metrics{
		SuiteName:   suiteName,
		Component:   component,
		TotalCases:  len(results),
		ScorerStats: map[string]ScorerMetric{},
	}

	latencies := make([]time.Duration, 0, len(results))
	scorerPasses := map[string][]float64{}
	scorerScores := map[string][]float64{}

	for _, r := range results {
		if r.Passed {
			m.Passed++
		} else {
			m.Failed++
		}
		m.TotalCost += r.Cost
		m.ToolCalls += len(r.ToolCalls)
		m.PromptTokens += r.PromptTokens
		m.OutputTokens += r.OutputTokens
		m.TotalTokens += r.TotalTokens
		m.AvgToolCalls += float64(len(r.ToolCalls))
		latencies = append(latencies, r.Latency)

		for _, tc := range r.ToolCalls {
			if tc.Err != nil {
				m.ToolErrors++
			}
			if tc.Repeated {
				m.RepeatRate++
				break
			}
		}

		for _, sd := range r.ScoreDetails {
			scorerPasses[sd.ScorerName] = append(scorerPasses[sd.ScorerName], boolScore(sd.Passed))
			scorerScores[sd.ScorerName] = append(scorerScores[sd.ScorerName], sd.Score)
		}
	}

	if len(results) == 0 {
		return m
	}

	n := float64(len(results))
	m.PassRate = float64(m.Passed) / n * 100
	m.AvgCost = m.TotalCost / n
	m.AvgToolCalls /= n
	m.RepeatRate = m.RepeatRate / n * 100
	m.AvgLatency = avgDuration(latencies)
	m.P50Latency = percentileDuration(latencies, 50)
	m.P95Latency = percentileDuration(latencies, 95)
	m.P99Latency = percentileDuration(latencies, 99)

	for name, passes := range scorerPasses {
		m.ScorerStats[name] = ScorerMetric{
			PassRate: avg(passes) * 100,
			AvgScore: avg(scorerScores[name]),
		}
	}
	return m
}

func boolScore(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func avg(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func avgDuration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	var total time.Duration
	for _, value := range values {
		total += value
	}
	return total / time.Duration(len(values))
}

func percentileDuration(values []time.Duration, percentile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if percentile <= 0 {
		return sorted[0]
	}
	if percentile >= 100 {
		return sorted[len(sorted)-1]
	}
	index := int((percentile/100)*float64(len(sorted)) + 0.999999999)
	if index < 1 {
		index = 1
	}
	return sorted[index-1]
}
