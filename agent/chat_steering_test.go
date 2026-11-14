package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type fakeChatControl struct {
	steer      *chatstate.Admission
	delivered  int
	start, end string
}

func (f *fakeChatControl) TakeSteer(_ context.Context, id string) (*chatstate.Admission, error) {
	if f.steer != nil && f.steer.TargetRunID == id {
		a := f.steer
		f.steer = nil
		return a, nil
	}
	return nil, nil
}
func (f *fakeChatControl) SteerDelivered(_ context.Context, runID, inputID string) error {
	f.delivered++
	return nil
}
func (f *fakeChatControl) RecordBoundary(_ context.Context, id, start, end string) error {
	f.start = start
	f.end = end
	return nil
}

type fakeChatGuard struct{ calls int }

func (f *fakeChatGuard) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	f.calls++
	return false, "", nil
}
func (f *fakeChatGuard) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f *fakeChatGuard) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}
func TestChatSteerPreservesGuard(t *testing.T) {
	guard := &fakeChatGuard{}
	control := &fakeChatControl{steer: &chatstate.Admission{ID: "s1", TargetRunID: "r1", Prompt: chatstate.Prompt{Text: "focus auth"}}}
	h := wrapChatSteering(guard, chatstate.Run{ID: "r1"}, control)
	if block, text, _ := h.PreToolUse(t.Context(), "Read", nil); !block || text == "" {
		t.Fatal("next tool not blocked")
	}
	if control.delivered != 0 {
		t.Fatal("ack before transcript persistence")
	}
	if block, _, _ := h.PreToolUse(t.Context(), "Read", nil); block || guard.calls != 1 {
		t.Fatal("guard lost or correction repeated")
	}
}
func TestChatRunBoundaries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{true: "stream", false: "atomic"}[stream], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "document.txt")
			if e := os.WriteFile(path, []byte("fixture"), 0600); e != nil {
				t.Fatal(e)
			}
			store := transcript.NewStore(filepath.Join(dir, "transcripts"))
			control := &fakeChatControl{}
			run := chatstate.Run{ID: "run", TurnID: "turn", Session: chatstate.SessionRef{ConversationID: 1}}
			ctx := WithChatRun(t.Context(), run, control)
			ca := NewChatAgent(&sideAgentProvider{path: path}, "fixture", dir, store, 10000)
			ca.SetNonStreaming(func() bool { return !stream })
			var activities []db.Activity
			_, e := ca.Chat(ctx, "auto", "conv-1", "read document", 5, 0, false, func(a db.Activity) { activities = append(activities, a) })
			if e != nil {
				t.Fatal(e)
			}
			records, e := store.Load(store.MainPath("conv-1"))
			if e != nil {
				t.Fatal(e)
			}
			if control.start == "" || control.end == "" {
				t.Fatal("missing boundaries")
			}
			foundStart, foundEnd := false, false
			for _, r := range records {
				if r.UUID == control.start {
					foundStart = r.Message != nil && r.Message.Text() == "read document"
				}
				if r.UUID == control.end {
					foundEnd = true
				}
			}
			if !foundStart || !foundEnd {
				t.Fatal("boundaries not real transcript records")
			}
			for _, a := range activities {
				if a.TurnID != "turn" || a.RunID != "run" {
					t.Fatal("unattributed display event")
				}
			}
		})
	}
}

func TestChatUnpersistedTerminalRetainsBilledUsage(t *testing.T) {
	dir := t.TempDir()
	store := transcript.NewStore(filepath.Join(dir, "transcripts"))
	path := store.MainPath("conv-1")
	p := captureUsageProvider{stream: func(ctx context.Context, y func(llm.StreamEvent, error) bool) {
		if !y(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 11}}, nil) {
			return
		}
		if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: "finished"}, nil) {
			return
		}
		if e := os.Remove(path); e != nil {
			t.Error(e)
			return
		}
		if e := os.Mkdir(path, 0700); e != nil {
			t.Error(e)
			return
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{OutputTokens: 7}}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}}
	ca := NewChatAgent(p, "fixture", dir, store, 10000)
	ctx := WithChatRun(t.Context(), chatstate.Run{ID: "run", TurnID: "turn"}, &fakeChatControl{})
	var activities []db.Activity
	_, err := ca.Chat(ctx, "auto", "conv-1", "test", 2, 0, false, func(a db.Activity) { activities = append(activities, a) })
	if !errors.Is(err, ErrChatPersistence) {
		t.Fatalf("lost transcript acknowledged: %v", err)
	}
	assertCapturedResultUsage(t, activities, 11, 7, 0, 0)
}

func TestChatTranscriptLogicalIdentity(t *testing.T) {
	dir := t.TempDir()
	store := transcript.NewStore(filepath.Join(dir, "transcripts"))
	ref := chatstate.SessionRef{ConversationID: 42}
	revision, err := chatstate.PrepareTranscript(t.Context(), store, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	logical := ""
	p := captureUsageProvider{stream: func(ctx context.Context, y func(llm.StreamEvent, error) bool) {
		logical = transcript.SessionIDFrom(ctx)
		if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: "answer"}, nil) {
			return
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}}
	ca := NewChatAgent(p, "fixture", dir, store, 10000)
	run := chatstate.Run{ID: "run", Session: ref, TranscriptID: revision.TranscriptID, HistoryRevision: 1}
	ctx := WithChatRun(t.Context(), run, &fakeChatControl{})
	_, err = ca.Chat(ctx, "auto", "conv-42", "test", 2, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if logical != "conv-42" {
		t.Fatalf("physical ID leaked into gateway identity: %q", logical)
	}
}

func TestChatTranscriptNoaRevisionIsolation(t *testing.T) {
	ref := chatstate.SessionRef{ConversationID: 42}
	a := WithChatRun(t.Context(), chatstate.Run{Session: ref, HistoryRevision: 1}, nil)
	b := WithChatRun(t.Context(), chatstate.Run{Session: ref, HistoryRevision: 2}, nil)
	if chatNoaSession(a, "chat-conv-42") == chatNoaSession(b, "chat-conv-42") {
		t.Fatal("Noa archives overlap history revisions")
	}
	if chatNoaSession(t.Context(), "legacy") != "legacy" {
		t.Fatal("legacy identity changed")
	}
}
