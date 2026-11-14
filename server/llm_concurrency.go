package server

import (
	"context"
	"iter"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// concurrencyProvider bounds simultaneous in-flight LLM requests across every
// caller that shares this provider instance. The token-bucket rate limiter
// smooths rate but allows bursts to overlap; backends with a hard per-access
// concurrency cap (e.g. "at most 2 concurrent requests per access") reject
// those overlaps with 429. This wrapper serializes admission with a weighted
// semaphore sized from the llm_max_concurrent setting.
//
// Each profile's provider gets its own semaphore, shared across its agents. The cap
// is read at construction (applyLLM / providerForProfile), so a settings change takes
// effect when the provider is rebuilt — the same lifecycle as rate-limit changes.
type concurrencyProvider struct {
	inner llm.Provider
	sem   chan struct{} // weighted semaphore; cap==0 means unlimited
}

// newConcurrencyProvider wraps prov with the cap from m. Returns prov unchanged
// when the cap is 0 (unlimited) so the hot path pays nothing.
func newConcurrencyProvider(prov llm.Provider, m *Manager) llm.Provider {
	n := m.LLMMaxConcurrent()
	if n <= 0 {
		return prov
	}
	return &concurrencyProvider{inner: prov, sem: make(chan struct{}, n)}
}

// acquire blocks until a slot frees or ctx is done. A zero cap passes through.
func (p *concurrencyProvider) acquire(ctx context.Context) (release func(), err error) {
	if cap(p.sem) == 0 {
		return func() {}, nil
	}
	start := time.Now()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case p.sem <- struct{}{}:
		if waited := time.Since(start); waited > time.Second {
			log.Printf("[llm-concurrency] waited %v for a slot (cap %d)", waited.Round(time.Millisecond), cap(p.sem))
		}
	}
	var once sync.Once
	return func() { once.Do(func() { <-p.sem }) }, nil
}

func (p *concurrencyProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		release, err := p.acquire(ctx)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		defer release()
		for ev, err := range p.inner.Stream(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (p *concurrencyProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	release, err := p.acquire(ctx)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	defer release()
	return p.inner.Complete(ctx, req)
}
