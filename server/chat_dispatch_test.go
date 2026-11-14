package server

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/llm"
)

func TestChatDispatcherCancellationAndQueue(t *testing.T) {
	s, _ := newRetestServer(t)
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	setRetestProvider(s, retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}})
	c, err := s.m.pg.CreateConversation("auto", "dispatcher", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.DeleteConversation(c.ID)
	send := retestRequest(s.pgSendConversationMessage, http.MethodPost, c.ID, `{"message":"first"}`)
	if send.Code != 202 {
		t.Fatalf("send %d %s", send.Code, send.Body)
	}
	s.chatMu.Lock()
	registered := s.chatCancel[s.convBusyKey(c.ID)] != nil
	s.chatMu.Unlock()
	if !registered {
		t.Fatal("202 without cancellation")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never started")
	}
	ref := chatstate.SessionRef{ConversationID: c.ID}
	if _, err = s.m.pg.AdmitChatInput(t.Context(), ref, "follow", "queue", "", chatstate.Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	stop := retestRequest(s.pgStopConversation, http.MethodPost, c.ID, "")
	if stop.Code != 200 {
		t.Fatal(stop.Body)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("stop missed run")
	}
	waitRetestIdle(t, s)
	state, err := s.m.pg.ChatControlState(t.Context(), ref)
	if err != nil || !state.Paused || state.Active != nil {
		t.Fatalf("state %+v %v", state, err)
	}
	var queued int
	for _, a := range state.Inputs {
		if a.Status == "queued" {
			queued++
		}
	}
	if queued != 1 {
		t.Fatal(fmt.Sprint(state))
	}
	select {
	case <-started:
		t.Fatal("queue ran after stop")
	default:
	}
}

func TestChatQueueDrainsAfterExternalRun(t *testing.T) {
	s, _ := newRetestServer(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	followup := make(chan struct{}, 1)
	var calls atomic.Int32
	setRetestProvider(s, retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return llm.Message{}, "", llm.Usage{}, ctx.Err()
			}
		} else {
			followup <- struct{}{}
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
	}})
	c, err := s.m.pg.CreateConversation("auto", "external gate", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.DeleteConversation(c.ID)
	key := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	s.chatBusy[key] = true
	s.chatMu.Unlock()
	s.runConversationWithContext(t.Context(), c, "generated turn", key)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("external run did not start")
	}
	ref := chatstate.SessionRef{ConversationID: c.ID}
	if _, err = s.m.pg.AdmitChatInput(t.Context(), ref, "follow", "queue", "", chatstate.Prompt{Text: "follow-up"}); err != nil {
		t.Fatal(err)
	}
	s.dispatchChatGate(ref.GateKey())
	close(release)
	select {
	case <-followup:
	case <-time.After(3 * time.Second):
		t.Fatal("successful external run did not wake durable queue")
	}
	waitRetestIdle(t, s)
}
