package server

import (
	"testing"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/llm"
)

func newShutdownFlushFixture(t *testing.T) (*Server, chatstate.SessionRef, int64) {
	t.Helper()
	s, fid := newRetestServer(t)
	c, err := s.m.pg.CreateConversation("auto", "shutdown final flush", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteConversation(c.ID) })
	ref := chatstate.SessionRef{ConversationID: c.ID}
	if _, err := s.m.pg.ChatControlState(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	s.side = &sideQuestionState{pending: map[string]sidequestion.Snapshot{}, latest: map[string]sidequestion.Snapshot{}, contextPending: map[string]pendingChatContext{}, runs: map[string]sideRun{}, done: make(chan struct{})}
	// All writes below arrive after the periodic publisher has already stopped.
	close(s.side.done)
	snap := sidequestion.Snapshot{Parent: sidequestion.Parent{ConversationID: c.ID}, RunID: 1, Version: 1, Request: llm.CompletionRequest{Messages: []llm.Message{llm.UserText("last checkpoint")}}}
	s.side.pending[snap.Parent.Key()] = snap
	s.side.latest[snap.Parent.Key()] = snap
	if err := s.captureChatContext(t.Context(), ref, "", chatstate.ContextView{Sequence: 1, Used: 123}); err != nil {
		t.Fatal(err)
	}
	return s, ref, fid
}

func TestChatContextNULFlushClearsPending(t *testing.T) {
	s, ref, _ := newShutdownFlushFixture(t)
	view := chatstate.ContextView{Sequence: 2, Request: llm.CompletionRequest{
		Messages: []llm.Message{llm.UserText("binary\x00payload")},
	}}
	if err := s.captureChatContext(t.Context(), ref, "", view); err != nil {
		t.Fatal(err)
	}
	if err := s.flushChatContexts(); err != nil {
		t.Fatal(err)
	}
	if len(s.side.contextPending) != 0 {
		t.Fatal("successful NUL snapshot remained pending for retry")
	}
	got, err := s.m.pg.ChatContext(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sequence != 2 || got.Request.Messages[0].Text() != "binarypayload" {
		t.Fatalf("snapshot not persisted: %+v", got)
	}
	if err := s.flushChatContexts(); err != nil {
		t.Fatal("empty retry flush failed", err)
	}
}
