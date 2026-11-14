package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestChatSteerAcknowledgedAfterTranscript(t *testing.T) {
	s, _ := newRetestServer(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	store := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	c, err := s.m.pg.CreateConversation("auto", "steer checkpoint", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.DeleteConversation(c.ID)
	ref := chatstate.SessionRef{ConversationID: c.ID}
	p := retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return llm.Message{}, "", llm.Usage{}, ctx.Err()
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "blocked-read", Name: "Read", Input: json.RawMessage(`{"file_path":"/does-not-exist"}`)}}}, "tool_use", llm.Usage{}, nil
		}
		raw, _ := json.Marshal(req.Messages)
		if !strings.Contains(string(raw), "focus auth") {
			t.Error("correction missing from replan")
		}
		records, e := store.Load(store.MainPath(ref.Key()))
		if e != nil {
			t.Error(e)
		}
		found := false
		for _, r := range records {
			if r.Message != nil {
				raw, _ := json.Marshal(r.Message.Content)
				if strings.Contains(string(raw), "[Chat steer") {
					found = true
				}
			}
		}
		if !found {
			t.Error("correction acknowledged without transcript")
		}
		var delivered int
		if e = s.m.pg.QueryRow(`SELECT count(*) FROM chat_control_inputs WHERE session_key=$1 AND kind='steer' AND status='delivered'`, ref.Key()).Scan(&delivered); e != nil || delivered != 1 {
			t.Errorf("delivery %d %v", delivered, e)
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("corrected")}}, "end_turn", llm.Usage{}, nil
	}}
	s.chatAgent = agent.NewChatAgent(p, "test", s.m.dir, store, 10000)
	s.chatAgent.SetNonStreaming(func() bool { return true })
	w := retestRequest(s.pgSendConversationMessage, http.MethodPost, c.ID, `{"message":"read"}`)
	if w.Code != 202 {
		t.Fatal(w.Body)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	state, e := s.m.pg.ChatControlState(t.Context(), ref)
	if e != nil || state.Active == nil {
		t.Fatal(state, e)
	}
	if _, e = s.m.pg.AdmitChatInput(t.Context(), ref, "correction", "steer", state.Active.ID, chatstate.Prompt{Text: "focus auth"}); e != nil {
		t.Fatal(e)
	}
	close(release)
	waitRetestIdle(t, s)
	r, e := s.m.pg.ChatRun(t.Context(), state.Active.ID)
	if e != nil || r.Status != "completed" || r.StartRecordUUID == "" || r.EndRecordUUID == "" {
		t.Fatalf("run %+v %v", r, e)
	}
	activities, _, e := s.m.pg.ConvActivityList(c.ID, 0, 100)
	if e != nil {
		t.Fatal(e)
	}
	for _, a := range activities {
		if a.TurnID != r.TurnID || a.RunID != r.ID || a.HistoryMutable == nil || !*a.HistoryMutable {
			t.Fatalf("activity missing run/boundary metadata %+v", a)
		}
	}
}
