package server

import (
	"context"
	"crypto/rand"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

// conversationRunContext builds a per-turn cancellable context for one
// conversation, like the async dispatch builds for its runs. Test-only: the
// port's chat dispatch is async, but the target-gate tests need the turn
// finished when the call returns.
func (s *Server) conversationRunContext(id int64, busyKey string) (context.Context, context.CancelCauseFunc) {
	return context.WithCancelCause(intercept.WithConvID(context.Background(), id))
}

// runConversationTurn admits, claims and runs ONE conversation turn
// synchronously on the given ctx — the test counterpart of the async
// admit → dispatch → executeChatRun path. The async dispatch is bypassed
// entirely, so the run cannot execute twice.
func (s *Server) runConversationTurn(ctx context.Context, _ context.CancelCauseFunc, c *db.Conversation, msg, busyKey string) {
	ref := chatstate.SessionRef{ConversationID: c.ID}
	prompt := chatstate.Prompt{Text: msg}
	if _, err := s.m.pg.AdmitChatInput(ctx, ref, rand.Text(), "send", "", prompt); err != nil {
		return
	}
	run, err := s.m.pg.ClaimNextChatInput(ctx, ref.GateKey())
	if err != nil || run == nil {
		return
	}
	_ = s.executeChatRun(ctx, *run)
}
