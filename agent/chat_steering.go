package agent

import (
	"context"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

type chatSteeringHooks struct {
	inner   harness.HookRunner
	run     chatstate.Run
	control ChatRunControl
}

func wrapChatSteering(inner harness.HookRunner, run chatstate.Run, control ChatRunControl) harness.HookRunner {
	if control == nil {
		return inner
	}
	return chatSteeringHooks{inner, run, control}
}
func (h chatSteeringHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if ctx.Err() != nil {
		return true, "Conversation stopped", nil
	}
	a, err := h.control.TakeSteer(ctx, h.run.ID)
	if err != nil {
		failChatRun(ctx, err)
		return true, "Chat correction could not be read; execution stopped", nil
	}
	if a != nil {
		marker := "[Chat steer " + a.ID + "]"
		if b := chatRunFrom(ctx); b != nil {
			b.mu.Lock()
			b.pending[a.ID] = marker
			b.mu.Unlock()
		}
		return true, marker + " " + a.Prompt.Text + "\nThe current tool call was not executed. Adjust the next action using this user's correction. Retain completed work and follow existing safety rules.", nil
	}
	if h.inner != nil {
		return h.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}
func (h chatSteeringHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}
func (h chatSteeringHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if h.inner != nil {
		return h.inner.Stop(ctx, messages)
	}
	return false, nil, ""
}
