package intercept

import (
	"context"
	"sync"
)

// ProviderOrigin is the concrete provider selected within one agent run. Data
// is owned by the provider integration (the interceptor does not depend on LLMs).
type ProviderOrigin struct {
	ProfileID int64
	Native    bool
	Data      any
	Err       error
}
type providerOriginState struct {
	sync.RWMutex
	origin ProviderOrigin
}
type providerOriginKey struct{}
type reviewOriginKey struct{}

// WithProviderOrigin always creates fresh state, isolating concurrent runs even
// when they share a cached provider or inherit a parent task context.
func WithProviderOrigin(ctx context.Context) context.Context {
	return context.WithValue(ctx, providerOriginKey{}, &providerOriginState{})
}
func SetProviderOrigin(ctx context.Context, origin ProviderOrigin) {
	if state, ok := ctx.Value(providerOriginKey{}).(*providerOriginState); ok {
		state.Lock()
		state.origin = origin
		state.Unlock()
	}
}

// SetProviderError attaches a terminal error without changing the concrete
// provider whose policy must govern any pending tool calls.
func SetProviderError(ctx context.Context, err error) {
	if state, ok := ctx.Value(providerOriginKey{}).(*providerOriginState); ok {
		state.Lock()
		state.origin.Err = err
		state.Unlock()
	}
}

func providerOrigin(ctx context.Context) ProviderOrigin {
	if state, ok := ctx.Value(providerOriginKey{}).(*providerOriginState); ok {
		state.RLock()
		defer state.RUnlock()
		return state.origin
	}
	return ProviderOrigin{}
}

// ReviewProviderOrigin is set only when no explicit judge profile overrides the
// concrete native origin. The captured config survives routing and later edits.
func ReviewProviderOrigin(ctx context.Context) (ProviderOrigin, bool) {
	v, ok := ctx.Value(reviewOriginKey{}).(ProviderOrigin)
	return v, ok
}
