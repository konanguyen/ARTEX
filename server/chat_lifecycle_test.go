package server

import (
	"errors"
	"testing"

	"github.com/Autumn-27/artex/chatstate"
)

func TestChatDeleteClaimBarrier(t *testing.T) {
	f := newHistoryFixture(t)
	a, err := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "claimed-before-delete", "send", "", chatstate.Prompt{Text: "must not infer"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.s.m.pg.ClaimNextChatInput(t.Context(), f.ref.GateKey())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.m.pg.CloseChatGate(t.Context(), f.ref); err != nil {
		t.Fatal(err)
	}
	if err = f.s.executeChatRun(t.Context(), *run); !errors.Is(err, chatstate.ErrGone) {
		t.Fatal("closed claim executed", a, err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("provider called after delete barrier")
	}
	if _, err = f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "late", "queue", "", chatstate.Prompt{Text: "late"}); !errors.Is(err, chatstate.ErrGone) {
		t.Fatal("closed parent accepted work", err)
	}
	f.s.m.pg.DeleteConversation(f.ref.ConversationID)
	if err = f.s.m.pg.SaveChatContext(t.Context(), f.ref, run.ID, chatstate.ContextView{Sequence: 99}); !errors.Is(err, chatstate.ErrGone) {
		t.Fatal("late context resurrected parent", err)
	}
}
