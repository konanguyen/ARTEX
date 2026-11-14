package intercept

import (
	"context"
	"time"
)

// targetScopeCtxKey is a separate unexported key type for the target-gate scope.
type targetScopeCtxKey struct{}

// DefaultTargetWaitBound is how long a gated tool call may wait for a human
// decision on an unpinned target before it fails with the request left open.
const DefaultTargetWaitBound = 300 * time.Second

// TargetScope is what an approval for an asset target is bound to: one task
// (TaskID) or one conversation (ConversationID), never both. Enabled reports the
// scope's opt-in flag and is read on every gated call; nil means the gate is off.
// WaitBound caps the wait for a human decision; 0 means never wait.
// Interceptor is the one that owns the gate of this session, so that code
// holding only a context (an HTTP redirect callback) can reach it.
type TargetScope struct {
	TaskID         string
	ConversationID int64
	Enabled        func() bool
	WaitBound      time.Duration
	Interceptor    *Interceptor
}

// WithTargetScope returns a child context carrying the target-gate scope of the
// session that runs on it.
func WithTargetScope(ctx context.Context, s TargetScope) context.Context {
	return context.WithValue(ctx, targetScopeCtxKey{}, s)
}

// TargetScopeFromContext extracts the target-gate scope (ok is false if absent).
func TargetScopeFromContext(ctx context.Context) (s TargetScope, ok bool) {
	s, ok = ctx.Value(targetScopeCtxKey{}).(TargetScope)
	return s, ok
}
