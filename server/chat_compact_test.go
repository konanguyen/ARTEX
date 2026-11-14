package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/llm"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatManualCompactAtomic(t *testing.T) {
	f := newHistoryFixture(t)
	for i := 0; i < 5; i++ {
		f.turn(t, fmt.Sprintf("turn-%d ", i)+strings.Repeat("detail ", 500))
	}
	last, _ := f.s.m.pg.ChatContext(t.Context(), f.ref)
	last.Source = "request"
	last.Request = llm.CompletionRequest{System: []string{"system"}, MaxTokens: 100}
	last.Sequence = 1
	last.ProfileIdentity = "fixture"
	last.Window = 32000
	last.OutputReserve = 100
	last.Compactor = "native"
	runs, _ := f.s.m.pg.ChatHistoryRuns(t.Context(), f.ref)
	if err := f.s.m.pg.SaveChatContext(t.Context(), f.ref, runs[len(runs)-1].ID, last); err != nil {
		t.Fatal(err)
	}
	before, _ := f.s.m.pg.ActiveChatTranscript(t.Context(), f.ref)
	state, _ := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	calls := 0
	runtime := chatCompactRuntime{Model: sidequestion.Model{Model: "fixture", Identity: "fixture", WindowTokens: 32000}, MaxTokens: 100, Provider: retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.UserText("summary"), "end_turn", llm.Usage{}, errors.New("summary offline")
	}}}
	if _, err := f.s.compactChatContext(t.Context(), f.ref, "failure", state.Revision, chatstate.CompactOptions{KeepRecent: 2}, runtime); err == nil {
		t.Fatal("summary failure published")
	}
	after, _ := f.s.m.pg.ActiveChatTranscript(t.Context(), f.ref)
	if before.TranscriptID != after.TranscriptID {
		t.Fatal("failed summary changed pointer")
	}
	// A failed candidate write cannot change the published pointer.
	txDir := filepath.Join(f.s.m.dir, "transcripts")
	backup := txDir + "-saved"
	runtime.Provider = retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if e := os.Rename(txDir, backup); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(txDir, []byte("block candidate directory"), 0600); e != nil {
			t.Fatal(e)
		}
		return llm.UserText("summary"), "end_turn", llm.Usage{}, nil
	}}
	state, _ = f.s.m.pg.ChatControlState(t.Context(), f.ref)
	_, diskErr := f.s.compactChatContext(t.Context(), f.ref, "disk-failure", state.Revision, chatstate.CompactOptions{KeepRecent: 2}, runtime)
	if e := os.Remove(txDir); e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(backup, txDir); e != nil {
		t.Fatal(e)
	}
	if diskErr == nil {
		t.Fatal("disk failure published")
	}
	unchanged, _ := f.s.m.pg.ActiveChatTranscript(t.Context(), f.ref)
	if unchanged.TranscriptID != before.TranscriptID {
		t.Fatal("disk failure changed pointer")
	}
	runtime.Provider = retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.UserText("concise summary"), "end_turn", llm.Usage{}, nil
	}}
	state, _ = f.s.m.pg.ChatControlState(t.Context(), f.ref)
	result, err := f.s.compactChatContext(t.Context(), f.ref, "compact-once", state.Revision, chatstate.CompactOptions{KeepRecent: 2}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	count := calls
	duplicate, err := f.s.compactChatContext(t.Context(), f.ref, "compact-once", state.Revision, chatstate.CompactOptions{KeepRecent: 2}, runtime)
	if err != nil || duplicate.HistoryRevision != result.HistoryRevision || calls != count {
		t.Fatal("lost response reran summary", duplicate, err, calls)
	}
	if result.HistoryRevision != 1 {
		t.Fatal(result)
	}
	after, _ = f.s.m.pg.ActiveChatTranscript(t.Context(), f.ref)
	if after.TranscriptID == before.TranscriptID {
		t.Fatal("candidate not published")
	}
	// A concurrent gate mutation invalidates the prepared candidate.
	runtime.Provider = retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		_, e := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "race-queue", "queue", "", chatstate.Prompt{Text: "followup"})
		if e != nil {
			t.Error(e)
		}
		return llm.UserText("summary"), "end_turn", llm.Usage{}, nil
	}}
	state, _ = f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if _, err = f.s.compactChatContext(t.Context(), f.ref, "stale", state.Revision, chatstate.CompactOptions{KeepRecent: 2}, runtime); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatal("CAS race accepted", err)
	}
	waitRetestIdle(t, f.s)
	stable, _ := f.s.m.pg.ActiveChatTranscript(t.Context(), f.ref)
	if stable.TranscriptID != after.TranscriptID {
		t.Fatal("CAS failure changed pointer")
	}
}

func TestChatManualCompactDiscardDoesNotReplayLostPrompt(t *testing.T) {
	f := newHistoryFixture(t)
	first := f.turn(t, "safe first")
	old, _ := f.store.Load(f.store.MainPath(first.TranscriptID))
	prepared, e := chatstate.PrepareTranscript(t.Context(), f.store, f.ref, old)
	if e != nil {
		t.Fatal(e)
	}
	before, _ := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if e = f.s.m.pg.PublishChatHistory(t.Context(), f.ref, before.Revision, prepared); e != nil {
		t.Fatal(e)
	}
	input, err := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "lost", "send", "", chatstate.Prompt{Text: "lost-prompt-sentinel"})
	if err != nil {
		t.Fatal(err)
	}
	lost, err := f.s.m.pg.ClaimNextChatInput(t.Context(), f.ref.GateKey())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.m.pg.RecordChatTranscriptBaseline(t.Context(), lost.ID, first.EndRecordUUID); err != nil {
		t.Fatal(err)
	}
	writer := f.store.ResumeWriter(lost.TranscriptID, first.EndRecordUUID)
	writer.RecordMessage(llm.UserText("lost-prompt-sentinel"), llm.Usage{})
	writer.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "unfinished", Name: "Read", Input: []byte(`{}`)}}}, llm.Usage{})
	if err = f.s.m.pg.RecoverChatRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "followup", "queue", "", chatstate.Prompt{Text: "continue safely"}); err != nil {
		t.Fatal(err)
	}
	state, _ := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if err = f.s.discardUnknownChatRun(t.Context(), f.ref, input.ID, state.Revision); err != nil {
		t.Fatal(err)
	}
	state, _ = f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if !state.Paused {
		t.Fatal("discard resumed queue")
	}
	if err = f.s.m.pg.ResumeChatGate(t.Context(), f.ref, state.Revision); err != nil {
		t.Fatal(err)
	}
	f.s.dispatchChatGate(f.ref.GateKey())
	waitRetestIdle(t, f.s)
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.requests[len(f.requests)-1]
	if strings.Contains(fmt.Sprint(last.Messages), "lost-prompt-sentinel") || !strings.Contains(fmt.Sprint(last.Messages), "safe first") {
		t.Fatal("discarded prompt replayed or prior context lost", last.Messages)
	}
}
