package chatstate

import "testing"

func TestChatSessionKeys(t *testing.T) {
	a := SessionRef{TaskID: 7, ExplorationID: 9}
	b := a
	b.MainSeg = 1
	if a.Key() == b.Key() || a.GateKey() != b.GateKey() {
		t.Fatal("segment identity/gate mismatch")
	}
	for _, ref := range []SessionRef{{}, {ConversationID: 1, TaskID: 2}, {TaskID: 1}, {ConversationID: 1, MainSeg: 1}, {TaskID: 1, ExplorationID: 2, MainSeg: -1}} {
		if ref.Validate() == nil {
			t.Fatalf("accepted %+v", ref)
		}
	}
	if (SessionRef{ConversationID: 4}).Key() != "conv-4" || a.Key() != "task-7-exp-9-main-s0" {
		t.Fatal("unstable keys")
	}
}
