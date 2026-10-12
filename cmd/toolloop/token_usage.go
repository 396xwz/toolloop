package main

import (
	"context"
	"sync"

	"github.com/396xwz/toolloop/internal/eval"
)

type tokenUsageCollectorKey struct{}

type tokenUsageCollector struct {
	mu    sync.Mutex
	usage eval.TokenUsage
}

func withTokenUsageCollector(ctx context.Context) (context.Context, *tokenUsageCollector) {
	collector := &tokenUsageCollector{}
	return context.WithValue(ctx, tokenUsageCollectorKey{}, collector), collector
}

func recordTokenUsage(ctx context.Context, usage eval.TokenUsage) {
	collector, _ := ctx.Value(tokenUsageCollectorKey{}).(*tokenUsageCollector)
	if collector == nil {
		return
	}
	collector.mu.Lock()
	collector.usage.Prompt += usage.Prompt
	collector.usage.Output += usage.Output
	collector.usage.Total += usage.Total
	collector.mu.Unlock()
}

func (c *tokenUsageCollector) snapshot() eval.TokenUsage {
	if c == nil {
		return eval.TokenUsage{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}
