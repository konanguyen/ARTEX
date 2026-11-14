package llmrec

import (
	"context"
	"sync"

	"github.com/Autumn-27/norma/llm"
)

type usageMetadataKey struct{}

// UsageMetadata carries provider fields absent from Norma's Usage. The transport
// replaces these on each HTTP attempt, independently of optional body recording.
type UsageMetadata struct {
	mu        sync.Mutex
	usage     llm.Usage
	reasoning int
}

func NewUsageMetadata(ctx context.Context) (context.Context, *UsageMetadata) {
	m := &UsageMetadata{}
	return context.WithValue(ctx, usageMetadataKey{}, m), m
}

func UsageMetadataFrom(ctx context.Context) *UsageMetadata {
	m, _ := ctx.Value(usageMetadataKey{}).(*UsageMetadata)
	return m
}

func (m *UsageMetadata) Set(usage llm.Usage, reasoning int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usage = usage
	m.reasoning = reasoning
}

func (m *UsageMetadata) Enrich(usage llm.Usage) (llm.Usage, int) {
	if m == nil {
		return usage, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if usage.CacheReadTokens == 0 {
		usage.CacheReadTokens = m.usage.CacheReadTokens
	}
	usage.CacheWriteTokens = max(usage.CacheWriteTokens, m.usage.CacheWriteTokens)
	return usage, m.reasoning
}

// usageDelta converts the recorder's cumulative maxima for Add-based consumers.
// It never creates, removes or delays events (message_delta commits failover).
func usageDelta(current, previous llm.Usage) llm.Usage {
	return llm.Usage{InputTokens: current.InputTokens - previous.InputTokens, OutputTokens: current.OutputTokens - previous.OutputTokens, CacheReadTokens: current.CacheReadTokens - previous.CacheReadTokens, CacheWriteTokens: current.CacheWriteTokens - previous.CacheWriteTokens}
}

// EnrichFailure retains totals read from the final wire attempt when Norma exits
// before forwarding its terminal usage event. Successful parsed totals stay intact.
func (m *UsageMetadata) EnrichFailure(usage llm.Usage) llm.Usage {
	if m == nil {
		return usage
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	usage.InputTokens = max(usage.InputTokens, m.usage.InputTokens)
	usage.OutputTokens = max(usage.OutputTokens, m.usage.OutputTokens)
	return usage
}
