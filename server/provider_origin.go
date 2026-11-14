package server

import (
	"context"
	"iter"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
)

// This wrapper sits on each concrete pool member, never on the pool head or
// task-chain routing ID (which is zero for bound and global profiles).
type originProvider struct {
	inner llm.Provider
	cfg   agent.Config
	id    int64
}

func (p *originProvider) mark(ctx context.Context) {
	intercept.SetProviderOrigin(ctx, intercept.ProviderOrigin{ProfileID: p.id, Native: p.cfg.Provider() == "ollama", Data: p.cfg})
}
func (p *originProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.mark(ctx)
		for ev, err := range p.inner.Stream(ctx, req) {
			if err != nil {
				intercept.SetProviderError(ctx, err)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}
func (p *originProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.mark(ctx)
	msg, stop, usage, err := p.inner.Complete(ctx, req)
	if err != nil {
		intercept.SetProviderError(ctx, err)
	}
	return msg, stop, usage, err
}
