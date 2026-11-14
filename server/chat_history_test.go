package server

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type historyFixtureState struct {
	s        *Server
	ref      chatstate.SessionRef
	store    *transcript.Store
	calls    atomic.Int32
	mu       sync.Mutex
	requests []llm.CompletionRequest
}

func newHistoryFixture(t *testing.T) *historyFixtureState {
	t.Helper()
	s, _ := newRetestServer(t)
	c, err := s.m.pg.CreateConversation("auto", "history fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.pg.DeleteConversation(c.ID) })
	f := &historyFixtureState{s: s, ref: chatstate.SessionRef{ConversationID: c.ID}, store: transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))}
	p := retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		n := f.calls.Add(1)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(fmt.Sprintf("answer %d", n))}}, "end_turn", llm.Usage{InputTokens: int(n) * 10, OutputTokens: 1}, nil
	}}
	s.chatAgent = agent.NewChatAgent(p, "fixture", s.m.dir, f.store, 100000)
	s.chatAgent.SetNonStreaming(func() bool { return true })
	return f
}
func (f *historyFixtureState) turn(t *testing.T, text string, atts ...chatstate.Attachment) chatstate.Run {
	t.Helper()
	_, err := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, fmt.Sprintf("input-%d", f.calls.Load()), "send", "", chatstate.Prompt{Text: text, Attachments: atts})
	if err != nil {
		t.Fatal(err)
	}
	f.s.dispatchChatGate(f.ref.GateKey())
	waitRetestIdle(t, f.s)
	runs, err := f.s.m.pg.ChatHistoryRuns(t.Context(), f.ref)
	if err != nil || len(runs) == 0 {
		t.Fatal(runs, err)
	}
	return runs[len(runs)-1]
}
func (f *historyFixtureState) change(t *testing.T, kind, id, turn string, replacement *chatstate.Prompt) ChatHistoryResult {
	t.Helper()
	state, err := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.s.changeChatHistory(t.Context(), f.ref, chatstate.HistoryChange{ClientID: id, Kind: kind, TurnID: turn, ExpectedRevision: state.Revision, Replacement: replacement, Title: "Child"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestChatRetryLatestUserTurn(t *testing.T) {
	f := newHistoryFixture(t)
	f.turn(t, "first")
	latest := f.turn(t, "second")
	writer := f.store.ResumeWriter(latest.TranscriptID, latest.EndRecordUUID)
	writer.RecordMessage(llm.UserText("[Chat steer old] obsolete correction"), llm.Usage{})
	var fresh atomic.Int32
	f.s.chatAgent = agent.NewChatAgent(retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		fresh.Add(1)
		raw := fmt.Sprint(req.Messages)
		if strings.Contains(raw, "obsolete correction") {
			t.Error("old steering replayed")
		}
		if req.Messages[len(req.Messages)-1].Text() != "second" {
			t.Error("retry used wrong prompt")
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("new model")}}, "end_turn", llm.Usage{InputTokens: 5}, nil
	}}, "current model", f.s.m.dir, f.store, 100000)
	f.s.chatAgent.SetNonStreaming(func() bool { return true })
	result := f.change(t, "retry", "retry-once", "", nil)
	if result.Session != f.ref {
		t.Fatal("retry created a session")
	}
	waitRetestIdle(t, f.s)
	if fresh.Load() != 1 {
		t.Fatal("retry did not select current provider")
	}
	var superseded int
	if err := f.s.m.pg.QueryRow(`SELECT count(*) FROM chat_control_runs WHERE session_key=$1 AND NOT active`, f.ref.Key()).Scan(&superseded); err != nil || superseded != 1 {
		t.Fatal(superseded, err)
	}
	attempts, err := f.s.m.pg.ChatAttempts(t.Context(), f.ref, latest.TurnID)
	if err != nil || len(attempts) != 2 {
		t.Fatal(attempts, err)
	}
	audit, _, err := f.s.m.pg.ChatAttemptActivities(t.Context(), f.ref, latest.ID, 0, 200)
	if err != nil || len(audit) == 0 {
		t.Fatal("superseded audit missing", audit, err)
	}
	active, _, err := f.s.m.pg.ConvActivityPage(f.ref.ConversationID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.RunID == latest.ID {
			t.Fatal("inactive attempt leaked into active display")
		}
	}
	var billed int
	if err := f.s.m.pg.QueryRow(`SELECT sum(input_tokens) FROM conversation_activities WHERE conversation_id=$1 AND kind='result'`, f.ref.ConversationID).Scan(&billed); err != nil || billed != 35 {
		t.Fatal(billed, err)
	}
}
func TestChatEditLatestAndEarlier(t *testing.T) {
	f := newHistoryFixture(t)
	first := f.turn(t, "first")
	last := f.turn(t, "second")
	latest := f.change(t, "edit", "edit-last", last.TurnID, &chatstate.Prompt{Text: "second edited"})
	if latest.Session != f.ref {
		t.Fatal("latest edit branched")
	}
	waitRetestIdle(t, f.s)
	earlier := f.change(t, "edit", "edit-first", first.TurnID, &chatstate.Prompt{Text: "first edited"})
	if earlier.Session == f.ref || earlier.Session.ConversationID == 0 {
		t.Fatal("earlier edit did not branch")
	}
	waitRetestIdle(t, f.s)
}
func TestChatBranchBeforeCompaction(t *testing.T) {
	f := newHistoryFixture(t)
	first := f.turn(t, "first")
	f.turn(t, "later distinctive fact")
	active, err := f.s.m.pg.ActiveChatTranscript(t.Context(), f.ref)
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.store.Load(f.store.MainPath(active.TranscriptID))
	if err != nil {
		t.Fatal(err)
	}
	marker := llm.BoundaryMessage(llm.BoundaryMeta{Trigger: "manual", MessagesSummarized: 4})
	summary := llm.UserText("summary contains later distinctive fact")
	records = append(records, transcript.Record{UUID: "boundary", Type: "message", Message: &marker}, transcript.Record{UUID: "summary", Type: "message", Message: &summary})
	prepared, err := chatstate.PrepareTranscript(t.Context(), f.store, f.ref, records)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if err = f.s.m.pg.PublishChatHistory(t.Context(), f.ref, state.Revision, prepared); err != nil {
		t.Fatal(err)
	}
	before := f.calls.Load()
	child := f.change(t, "branch", "old-point", first.TurnID, nil)
	if f.calls.Load() != before {
		t.Fatal("branch ran a model")
	}
	revision, err := f.s.m.pg.ActiveChatTranscript(t.Context(), child.Session)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.store.MainPath(revision.TranscriptID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "later distinctive fact") {
		t.Fatal("future summary leaked into branch")
	}
}
func TestChatBranchAttachmentRetention(t *testing.T) {
	f := newHistoryFixture(t)
	dir := f.s.chatPromptDir(f.ref)
	if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "uploads", "report.pdf")
	if err := os.WriteFile(source, []byte("pdf bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	run := f.turn(t, "review", chatstate.Attachment{Name: "report.pdf", Path: "uploads/report.pdf", Size: 9, Abs: source})
	child := f.change(t, "branch", "copy-doc", run.TurnID, nil)
	if err := f.s.m.pg.DeleteConversation(f.ref.ConversationID); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.s.chatPromptDir(child.Session), "uploads/report.pdf"))
	if err != nil || string(data) != "pdf bytes" {
		t.Fatal(string(data), err)
	}
	active, err := f.s.m.pg.ActiveChatTranscript(t.Context(), child.Session)
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.store.Load(f.store.MainPath(active.TranscriptID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(records[0].Message.Text(), source) {
		t.Fatal("attachment manifest still references deleted parent")
	}
}
func TestChatBranchIdempotency(t *testing.T) {
	f := newHistoryFixture(t)
	run := f.turn(t, "first")
	state, _ := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	change := chatstate.HistoryChange{Kind: "branch", ClientID: "same", TurnID: run.TurnID, ExpectedRevision: state.Revision, Title: "One child"}
	var wg sync.WaitGroup
	results := make(chan ChatHistoryResult, 2)
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := f.s.changeChatHistory(t.Context(), f.ref, change)
			if err != nil {
				failures <- err
			} else {
				results <- result
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var ref chatstate.SessionRef
	count := 0
	for result := range results {
		if count > 0 && ref != result.Session {
			t.Fatal("duplicate branch created two children")
		}
		ref = result.Session
		count++
	}
	if count != 2 {
		t.Fatal("duplicate branch did not return durable result")
	}
	var children int
	if err := f.s.m.pg.QueryRow(`SELECT count(*) FROM chat_control_sessions WHERE parent_session_key=$1 AND NOT preparing`, f.ref.Key()).Scan(&children); err != nil || children != 1 {
		t.Fatal(children, err)
	}
}

func TestChatBranchCopyRejectsDestinationSymlink(t *testing.T) {
	source, target, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "uploads"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "uploads", "doc.txt"), []byte("doc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "uploads")); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyChatAttachments(t.Context(), source, target, chatstate.Prompt{Attachments: []chatstate.Attachment{{Name: "doc.txt", Path: "uploads/doc.txt", Size: 3}}}); err == nil {
		t.Fatal("copy followed a destination symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "doc.txt")); !os.IsNotExist(err) {
		t.Fatal("copy wrote outside child")
	}
}

type historyWireProvider struct {
	complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (p historyWireProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(ctx, req)
}
func (p historyWireProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		message, stop, usage, err := p.Complete(ctx, req)
		if err != nil {
			y(llm.StreamEvent{}, err)
			return
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: usage.InputTokens}}, nil) {
			return
		}
		for _, block := range message.Content {
			if block.Type == llm.BlockText && !y(llm.StreamEvent{Type: llm.SETextDelta, Text: block.Text}, nil) {
				return
			}
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: llm.Usage{OutputTokens: usage.OutputTokens}}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}
func TestChatBranchResumeAfterRestart(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			f := newHistoryFixture(t)
			f.turn(t, "first")
			last := f.turn(t, "second")
			child := f.change(t, "branch", "restart-child", last.TurnID, nil)
			restart := &Server{ctx: t.Context(), m: f.s.m, chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}}
			t.Cleanup(func() { waitRetestIdle(t, restart) })
			var resumed atomic.Int32
			restart.chatAgent = agent.NewChatAgent(historyWireProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				resumed.Add(1)
				raw := fmt.Sprint(req.Messages)
				if !strings.Contains(raw, "first") || !strings.Contains(raw, "second") || req.Messages[len(req.Messages)-1].Text() != "child follow-up" {
					t.Error("restarted child lost structured prefix")
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("resumed")}}, "end_turn", llm.Usage{InputTokens: 4}, nil
			}}, "current", restart.m.dir, f.store, 100000)
			restart.chatAgent.SetNonStreaming(func() bool { return !stream })
			_, err := restart.m.pg.AdmitChatInput(t.Context(), child.Session, "child-follow-up", "send", "", chatstate.Prompt{Text: "child follow-up"})
			if err != nil {
				t.Fatal(err)
			}
			restart.dispatchChatGate(child.Session.GateKey())
			waitRetestIdle(t, restart)
			if resumed.Load() != 1 {
				t.Fatal("child did not resume exactly once")
			}
			state, err := restart.m.pg.ChatControlState(t.Context(), child.Session)
			if err != nil || state.Paused {
				t.Fatal(state, err)
			}
		})
	}
}
func TestChatRetryKeepsPausedFollowups(t *testing.T) {
	f := newHistoryFixture(t)
	f.s.chatAgent = agent.NewChatAgent(retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("fixture provider offline")
	}}, "offline", f.s.m.dir, f.store, 100000)
	f.s.chatAgent.SetNonStreaming(func() bool { return true })
	f.turn(t, "failed prompt")
	if _, err := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "parked", "queue", "", chatstate.Prompt{Text: "parked follow-up"}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	f.s.chatAgent = agent.NewChatAgent(retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		if req.Messages[len(req.Messages)-1].Text() != "failed prompt" {
			t.Error("parked queue ran during retry")
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("recovered")}}, "end_turn", llm.Usage{}, nil
	}}, "online", f.s.m.dir, f.store, 100000)
	f.s.chatAgent.SetNonStreaming(func() bool { return true })
	f.change(t, "retry", "retry-failed", "", nil)
	waitRetestIdle(t, f.s)
	state, err := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if err != nil || !state.Paused || calls.Load() != 1 {
		t.Fatal(state, calls.Load(), err)
	}
	queued := 0
	for _, input := range state.Inputs {
		if input.Status == "queued" {
			queued++
		}
	}
	if queued != 1 {
		t.Fatal("retry released parked follow-ups", state)
	}
}

func TestChatActionsLatestIdentity(t *testing.T) {
	f := newHistoryFixture(t)
	f.turn(t, "same prompt")
	last := f.turn(t, "same prompt")
	state, err := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if err != nil || state.LatestTurnID != last.TurnID || state.LatestRunID != last.ID || state.LatestRunStatus != "completed" {
		t.Fatal(state, err)
	}
}

func TestChatRetryStoppedIncompleteTail(t *testing.T) {
	f := newHistoryFixture(t)
	f.turn(t, "safe original")
	f.change(t, "retry", "first-retry", "", nil)
	waitRetestIdle(t, f.s)
	runs, err := f.s.m.pg.ChatHistoryRuns(t.Context(), f.ref)
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	run := runs[0]
	writer := f.store.ResumeWriter(run.TranscriptID, run.EndRecordUUID)
	writer.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "unfinished", Name: "Read", Input: []byte(`{}`)}}}, llm.Usage{})
	if _, err = f.s.m.pg.Exec(`UPDATE chat_control_runs SET status='stopped' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	state, _ := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if _, err = f.s.changeChatHistory(t.Context(), f.ref, chatstate.HistoryChange{ClientID: "retry-stopped", Kind: "retry", ExpectedRevision: state.Revision}); err != nil {
		t.Fatal("stopped attempt's discarded suffix blocked Retry", err)
	}
	waitRetestIdle(t, f.s)
}
