package server

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/chatstate"
)

func TestChatQueuePreflightFailureVisibleAndEditable(t *testing.T) {
	f := newHistoryFixture(t)
	f.s.chatAgent = nil
	_, err := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "no-profile", "queue", "", chatstate.Prompt{Text: "visible failed prompt"})
	if err != nil {
		t.Fatal(err)
	}
	f.s.dispatchChatGate(f.ref.GateKey())
	waitRetestIdle(t, f.s)
	var users, errors, mutable int
	err = f.s.m.pg.QueryRow(`SELECT count(*) FILTER(WHERE kind='user'),count(*) FILTER(WHERE is_error),count(*) FILTER(WHERE kind='user' AND history_mutable) FROM conversation_activities WHERE conversation_id=$1`, f.ref.ConversationID).Scan(&users, &errors, &mutable)
	if err != nil || users != 1 || errors < 1 || mutable != 1 {
		t.Fatal("accepted failed prompt invisible or cannot be corrected", users, errors, mutable, err)
	}
}

func TestChatQueuePreflightErrorDetail(t *testing.T) {
	f := newHistoryFixture(t)
	f.s.chatAgent = nil
	_, err := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "english-failure", "queue", "", chatstate.Prompt{Text: "original text"})
	if err != nil {
		t.Fatal(err)
	}
	f.s.dispatchChatGate(f.ref.GateKey())
	waitRetestIdle(t, f.s)
	var detail string
	if err = f.s.m.pg.QueryRow(`SELECT detail FROM conversation_activities WHERE conversation_id=$1 AND is_error ORDER BY id DESC LIMIT 1`, f.ref.ConversationID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "Failed to start conversation") {
		t.Fatal("expected preflight error message in English", detail)
	}
}
