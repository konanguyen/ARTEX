package agent

import (
	"context"
	"fmt"
	"log"
	"sync"

	runtimeconfig "github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
)

// RuntimePolicy selects the native Ollama context safeguards without changing
// the historical context management of cloud and compatible providers.
type RuntimePolicy struct {
	NativeOllama  bool
	ContextWindow int
}

// LocalLLMOnly reports whether strict local LLM mode is currently active.
func LocalLLMOnly() bool {
	strict, _ := runtimeconfig.LocalLLMPolicy()
	return strict
}

// RunLocal applies the native Ollama runtime policy for local execution.
func RunLocal(opts *agentcore.Options, window int) {
	applyRuntimePolicy(opts, RuntimePolicy{NativeOllama: true, ContextWindow: window})
}

var (
	nativeOllamaResolversMu sync.RWMutex
	nativeOllamaResolvers   = make(map[any]func() bool)
)

// SetNativeOllamaResolver resolves whether the current run uses native Ollama.
func (w *Worker) SetNativeOllamaResolver(fn func() bool) {
	nativeOllamaResolversMu.Lock()
	nativeOllamaResolvers[w] = fn
	nativeOllamaResolversMu.Unlock()
}

// SetNativeOllamaResolver resolves whether the current run uses native Ollama.
func (p *Planner) SetNativeOllamaResolver(fn func() bool) {
	nativeOllamaResolversMu.Lock()
	nativeOllamaResolvers[p] = fn
	nativeOllamaResolversMu.Unlock()
}

// SetNativeOllamaResolver resolves whether the current run uses native Ollama.
func (m *MainAgent) SetNativeOllamaResolver(fn func() bool) {
	nativeOllamaResolversMu.Lock()
	nativeOllamaResolvers[m] = fn
	nativeOllamaResolversMu.Unlock()
}

// SetNativeOllamaResolver resolves whether the current run uses native Ollama.
func (c *ChatAgent) SetNativeOllamaResolver(fn func() bool) {
	nativeOllamaResolversMu.Lock()
	nativeOllamaResolvers[c] = fn
	nativeOllamaResolversMu.Unlock()
}

func applyRuntimePolicy(opts *agentcore.Options, policy RuntimePolicy) {
	if !policy.NativeOllama || policy.ContextWindow <= 0 {
		return
	}
	if policy.ContextWindow <= 32*1024 {
		opts.MaxToolOutputChars = 16 * 1024
	} else if policy.ContextWindow <= 64*1024 {
		opts.MaxToolOutputChars = 32 * 1024
	}
	if policy.ContextWindow <= 64*1024 && opts.Compactor == nil {
		summaryLimit := min(2048, policy.ContextWindow/8)
		if opts.MaxTokens > 0 {
			summaryLimit = min(summaryLimit, opts.MaxTokens)
		}
		opts.Compactor = &localCompactor{window: policy.ContextWindow, maxOutput: summaryLimit, summarize: compaction.ProviderSummarizer(opts.Provider, summaryLimit, opts.NonStreaming), warn: opts.OnWarn}
		opts.Compaction = nil
	}
}

// localCompactor uses the SDK's non-destructive boundary/summary mechanism, but
// triggers at 72% of the actual model context instead of cloud-sized reserves.
// The raw transcript remains intact, including complete tool call/result pairs.
type localCompactor struct {
	window, maxOutput int
	failures          int
	warn              func(string)
	summarize         compaction.Summarizer
}

func (c *localCompactor) Pre(ctx context.Context, msgs []llm.Message, lastInputTokens int) []llm.Message {
	// Include newly appended tool results even when the previous response reported
	// a smaller prompt. The SDK estimate includes a conservative 4/3 factor.
	tokens := max(lastInputTokens, compaction.EstimateTokens(llm.MessagesForAPI(msgs))*4/3)
	if tokens < c.window*72/100 {
		return msgs
	}
	out, _ := c.Reactive(ctx, msgs)
	return out
}

func (c *localCompactor) Reactive(ctx context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	if c.failures >= 3 {
		return msgs, false
	}
	start := llm.LastBoundaryIndex(msgs) + 1
	tail := max(start, len(msgs)-8)
	// If the tail begins with a tool result, retain its originating assistant
	// call too. Parallel calls may span several messages, so inspect every result.
	for {
		required := tail
		for _, msg := range msgs[tail:] {
			for _, block := range msg.Content {
				if block.Type != llm.BlockToolResult {
					continue
				}
				for j := tail - 1; j >= start; j-- {
					for _, call := range msgs[j].Content {
						if call.Type == llm.BlockToolUse && call.ID == block.ToolUseID {
							required = min(required, j)
						}
					}
				}
			}
		}
		if required == tail {
			break
		}
		tail = required
	}
	if tail <= start {
		return msgs, false
	}
	keep := 0
	for _, msg := range msgs[tail:] {
		if !llm.IsBoundaryMarker(msg) {
			keep++
		}
	}
	sdk := compaction.New(compaction.Config{ContextWindow: c.window, MaxOutputTokens: c.maxOutput, KeepRecent: keep}, c.summarize)
	out, ok := sdk.Reactive(ctx, msgs)
	if !ok {
		c.failures++
		warning := fmt.Sprintf("Native Ollama context compaction failed to shrink the prompt (%d/3); original transcript retained; provider context errors will surface", c.failures)
		if c.warn != nil {
			c.warn(warning)
		} else {
			log.Printf("[compaction] %s", warning)
		}
		return msgs, false
	}
	c.failures = 0
	return out, true
}

func (c *localCompactor) IsOverflow(err error) bool { return compaction.IsOverflow(err) }
