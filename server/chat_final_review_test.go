package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatFinalExternalBusy(t *testing.T) {
	f := newHistoryFixture(t)
	c, _ := f.s.m.pg.GetConversation(f.ref.ConversationID)
	key := f.ref.GateKey()
	f.s.chatBusy[key] = true
	_, err := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "queued-before-external-reserve", "queue", "", chatstate.Prompt{Text: "user queued"})
	if err != nil {
		t.Fatal(err)
	}
	f.s.runConversationSync(c, "generated trigger", key)
	state, err := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provider calls=%d active=%v queued=%+v", f.calls.Load(), state.Active, state.Inputs)
	if f.calls.Load() != 2 || state.Active != nil || len(state.Inputs) != 0 {
		t.Fatal("generated and user inputs did not both execute", f.calls.Load(), state)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(fmt.Sprint(f.requests[0].Messages), "user queued") || !strings.Contains(fmt.Sprint(f.requests[1].Messages), "generated trigger") {
		t.Fatal("claimed input text/order was changed", f.requests)
	}
}
func TestChatFinalExternalRetestAfterPause(t *testing.T) {
	s, fid := newRetestServer(t)
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
	}})
	retest, c, _, err := s.m.pg.CreateFindingRetest(t.Context(), fid, "")
	if err != nil {
		t.Fatal(err)
	}
	ref := chatstate.SessionRef{ConversationID: c.ID}
	_, err = s.m.pg.AdmitExternalChatInput(t.Context(), ref, "external-paused", chatstate.Prompt{Text: retest.InitialMessage()})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.PauseChatGate(t.Context(), ref, -1); err != nil {
		t.Fatal(err)
	}
	if run, e := s.m.pg.ClaimNextChatInput(t.Context(), ref.GateKey()); e != nil || run != nil {
		t.Fatal(run, e)
	}
	state, _ := s.m.pg.ChatControlState(t.Context(), ref)
	if err = s.m.pg.ResumeChatGate(t.Context(), ref, state.Revision); err != nil {
		t.Fatal(err)
	}
	s.dispatchChatGate(ref.GateKey())
	waitRetestIdle(t, s)
	got, err := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retest after dispatch: %+v", got)
	if got.Status == "pending" || got.Status == "running" {
		t.Error("resumed external run skipped retest start/finalization")
	}
}
func TestChatFinalLegacyBranchDisplay(t *testing.T) {
	f := newHistoryFixture(t)
	old := f.turn(t, "legacy original prompt")
	// Simulate the persisted form of an unambiguous pre-feature transcript/display.
	if _, err := f.s.m.pg.Exec(`UPDATE conversation_activities SET run_id=NULL,turn_id=NULL,active_attempt=NULL,history_mutable=false WHERE conversation_id=$1`, f.ref.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.m.pg.Exec(`DELETE FROM chat_control_runs WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	next := f.turn(t, "new prompt")
	child := f.change(t, "branch", "legacy-child", next.TurnID, nil)
	defer f.s.m.pg.DeleteConversation(child.Session.ConversationID)
	var found int
	if err := f.s.m.pg.QueryRow(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1 AND kind='user' AND detail='legacy original prompt'`, child.Session.ConversationID).Scan(&found); err != nil {
		t.Fatal(err)
	}
	t.Logf("legacy user rows in child=%d", found)
	if found == 0 {
		t.Error("child model has legacy history but copied display drops it")
	}
	_ = db.FindingRetestAgentKey
}

func TestChatFinalBranchAfterCompactAttachment(t *testing.T) {
	f := newHistoryFixture(t)
	parent := f.ref
	dir := f.s.chatPromptDir(parent)
	if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "uploads", "report.txt")
	if err := os.WriteFile(path, []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	first := f.turn(t, "first "+strings.Repeat("history detail ", 500))
	second := f.turn(t, "second", chatstate.Attachment{Name: "report.txt", Path: "uploads/report.txt", Abs: path, Size: 5})
	third := f.turn(t, "third")
	active, err := f.s.m.pg.ActiveChatTranscript(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.store.Load(f.store.MainPath(active.TranscriptID))
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := agent.PrepareChatCompaction(t.Context(), records, llm.CompletionRequest{System: []string{"system"}, MaxTokens: 100}, chatstate.CompactOptions{KeepRecent: 2, TurnStarts: []string{first.StartRecordUUID, second.StartRecordUUID, third.StartRecordUUID}}, func(context.Context, []llm.Message) (string, error) { return "Old exchange summary", nil }, 32000)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := chatstate.PrepareTranscript(t.Context(), f.store, parent, candidate)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := f.s.m.pg.ChatControlState(t.Context(), parent)
	if err = f.s.m.pg.PublishChatHistory(t.Context(), parent, state.Revision, revision); err != nil {
		t.Fatal(err)
	}
	// Take the branch on a new completed exchange after the compression marker.
	fourth := f.turn(t, "fourth")
	child := f.change(t, "branch", "after-compact", fourth.TurnID, nil)
	defer f.s.m.pg.DeleteConversation(child.Session.ConversationID)
	if err = f.s.m.pg.DeleteConversation(parent.ConversationID); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(f.s.chatPromptDir(parent)); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(f.s.chatPromptDir(child.Session), "uploads", "report.txt")
	if data, e := os.ReadFile(childPath); e != nil || string(data) != "proof" {
		t.Fatal("child attachment unavailable", string(data), e)
	}
	f.ref = child.Session
	f.turn(t, "please read the attachment")
	f.mu.Lock()
	request := fmt.Sprint(f.requests[len(f.requests)-1].Messages)
	f.mu.Unlock()
	if !strings.Contains(request, childPath) {
		t.Error("child model request lacks its independent upload path")
	}
	if strings.Contains(request, path) {
		t.Errorf("child request retains parent upload path %s", path)
	}
}
func TestChatFinalExternalShutdown(t *testing.T) {
	f := newHistoryFixture(t)
	ctx, cancel := context.WithCancel(f.s.ctx)
	f.s.ctx = ctx
	started := make(chan struct{})
	setRetestProvider(f.s, retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		close(started)
		<-ctx.Done()
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}})
	c, _ := f.s.m.pg.GetConversation(f.ref.ConversationID)
	key := f.ref.GateKey()
	f.s.chatBusy[key] = true
	f.s.runConversationWithContext(t.Context(), c, "generated", key)
	<-started
	cancel()
	waitRetestIdle(t, f.s)
	var status string
	if err := f.s.m.pg.QueryRow(`SELECT status FROM chat_control_runs WHERE session_key=$1`, key).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "unknown" {
		t.Errorf("shutdown terminal status=%s; lost-run acknowledgment omitted", status)
	}
}

func TestChatFinalLegacyHistoryActions(t *testing.T) {
	f := newHistoryFixture(t)
	old := f.turn(t, "legacy original prompt")
	if _, err := f.s.m.pg.Exec(`UPDATE conversation_activities SET run_id=NULL,turn_id=NULL,active_attempt=NULL,history_mutable=false WHERE conversation_id=$1`, f.ref.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.m.pg.Exec(`DELETE FROM chat_control_runs WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/conversations/%d/controls", f.ref.ConversationID), nil)
	req.SetPathValue("id", fmt.Sprint(f.ref.ConversationID))
	r := httptest.NewRecorder()
	f.s.chatControls(r, req)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	state, err := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if err != nil {
		t.Fatal(err)
	}
	if state.LatestTurnID == "" {
		t.Fatal("unambiguous legacy history has no mutation identity")
	}
}

func TestChatFinalAmbiguousLegacyDisplayPreserved(t *testing.T) {
	f := newHistoryFixture(t)
	old := f.turn(t, "raw original prompt")
	if _, e := f.s.m.pg.Exec(`UPDATE conversation_activities SET run_id=NULL,turn_id=NULL,active_attempt=NULL,history_mutable=false,detail=CASE WHEN kind='user' THEN 'legacy display cannot map' ELSE detail END WHERE conversation_id=$1`, f.ref.ConversationID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.m.pg.Exec(`DELETE FROM chat_control_runs WHERE id=$1`, old.ID); e != nil {
		t.Fatal(e)
	}
	next := f.turn(t, "new prompt")
	child := f.change(t, "branch", "ambiguous-legacy-child", next.TurnID, nil)
	defer f.s.m.pg.DeleteConversation(child.Session.ConversationID)
	var count int
	if e := f.s.m.pg.QueryRow(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1 AND kind='user' AND detail='legacy display cannot map' AND run_id IS NULL AND NOT history_mutable`, child.Session.ConversationID).Scan(&count); e != nil {
		t.Fatal(e)
	}
	if count != 1 {
		t.Fatal("read-only legacy display disappeared", count)
	}
}
func TestChatFinalGeneratedDoesNotMapAsLegacy(t *testing.T) {
	f := newHistoryFixture(t)
	c, _ := f.s.m.pg.GetConversation(f.ref.ConversationID)
	if _, e := f.s.m.pg.AppendConvActivity(c.ID, db.Activity{Worker: c.AgentKey, Kind: "user", Summary: "generated", Detail: "generated"}); e != nil {
		t.Fatal(e)
	}
	f.s.runConversationSync(c, "generated", f.ref.GateKey())
	if e := f.s.ensureLegacyChatHistory(t.Context(), f.ref); e != nil {
		t.Fatal(e)
	}
	state, e := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if e != nil {
		t.Fatal(e)
	}
	if state.LatestTurnID != "" {
		t.Fatal("generated input imported as human legacy history")
	}
}

func TestChatFinalUndeliveredSteerFollowup(t *testing.T) {
	f := newHistoryFixture(t)
	_, e := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "turn", "send", "", chatstate.Prompt{Text: "active"})
	if e != nil {
		t.Fatal(e)
	}
	run, e := f.s.m.pg.ClaimNextChatInput(t.Context(), f.ref.GateKey())
	if e != nil {
		t.Fatal(e)
	}
	steer, e := f.s.m.pg.AdmitChatInput(t.Context(), f.ref, "steer", "steer", run.ID, chatstate.Prompt{Text: "late correction"})
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.m.pg.PauseChatGate(t.Context(), f.ref, -1); e != nil {
		t.Fatal(e)
	}
	if e = f.s.m.pg.FinishChatRun(t.Context(), run.ID, "completed"); e != nil {
		t.Fatal(e)
	}
	stateBefore, _ := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	for _, item := range stateBefore.Inputs {
		if item.ID == steer.ID {
			steer = item
		}
	}
	other, e := f.s.m.pg.CreateConversation("auto", "other scope", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer f.s.m.pg.DeleteConversation(other.ID)
	wrong := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/conversations/%d/controls", other.ID), strings.NewReader(fmt.Sprintf(`{"expected_revision":%q}`, fmt.Sprint(steer.Revision))))
	wrong.SetPathValue("id", fmt.Sprint(other.ID))
	wrong.SetPathValue("rest", "inputs/"+steer.ID+"/followup")
	denied := httptest.NewRecorder()
	f.s.chatControls(denied, wrong)
	if denied.Code != 404 {
		t.Fatal("steer converted outside its original session", denied.Code)
	}
	var previous string
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/conversations/%d/controls/inputs/%s/followup", f.ref.ConversationID, steer.ID), strings.NewReader(fmt.Sprintf(`{"expected_revision":%q}`, fmt.Sprint(steer.Revision))))
		req.SetPathValue("id", fmt.Sprint(f.ref.ConversationID))
		req.SetPathValue("rest", "inputs/"+steer.ID+"/followup")
		response := httptest.NewRecorder()
		f.s.chatControls(response, req)
		if response.Code != 200 {
			t.Fatalf("follow-up action unavailable: %d %s", response.Code, response.Body.String())
		}
		var admission chatstate.Admission
		if e = json.Unmarshal(response.Body.Bytes(), &admission); e != nil {
			t.Fatal(e)
		}
		if admission.Session != f.ref || admission.Prompt.Text != "late correction" || admission.Kind != "turn" {
			t.Fatal(admission)
		}
		if previous != "" && previous != admission.ID {
			t.Fatal("lost-response retry duplicated follow-up")
		}
		previous = admission.ID
	}
	state, e := f.s.m.pg.ChatControlState(t.Context(), f.ref)
	if e != nil {
		t.Fatal(e)
	}
	if !state.Paused || len(state.Inputs) != 1 || state.Inputs[0].ID != previous || f.calls.Load() != 0 {
		t.Fatal("conversion did not preserve pause/replace warning exactly once", state)
	}
}

func TestChatFinalBranchLiteralManifestNotRewritten(t *testing.T) {
	f := newHistoryFixture(t)
	dir := f.s.chatPromptDir(f.ref)
	os.MkdirAll(filepath.Join(dir, "uploads"), 0700)
	path := filepath.Join(dir, "uploads", "report.txt")
	os.WriteFile(path, []byte("proof"), 0600)
	attachment := chatstate.Attachment{Name: "report.txt", Path: "uploads/report.txt", Abs: path, Size: 5}
	f.turn(t, "attached", attachment)
	literal := composeAgentMessage("", []chatstate.Attachment{attachment}, dir)
	last := f.turn(t, literal)
	child := f.change(t, "branch", "literal-manifest", last.TurnID, nil)
	defer f.s.m.pg.DeleteConversation(child.Session.ConversationID)
	active, e := f.s.m.pg.ActiveChatTranscript(t.Context(), child.Session)
	if e != nil {
		t.Fatal(e)
	}
	records, e := f.store.Load(f.store.MainPath(active.TranscriptID))
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range records {
		if r.UUID == last.StartRecordUUID && r.Message.Text() != literal {
			t.Fatal("literal user-authored manifest rewritten")
		}
	}
}

func TestChatFinalUnclaimedRetestRecovery(t *testing.T) {
	s, fid := newRetestServer(t)
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
	}})
	retest, c, _, err := s.m.pg.CreateFindingRetest(t.Context(), fid, "")
	if err != nil {
		t.Fatal(err)
	}
	ref := chatstate.SessionRef{ConversationID: c.ID}
	if _, err = s.m.pg.AdmitExternalChatInput(t.Context(), ref, "unclaimed-at-restart", chatstate.Prompt{Text: retest.InitialMessage()}); err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.PauseChatGate(t.Context(), ref, -1); err != nil {
		t.Fatal(err)
	}
	// This is the Manager startup step, before Server.initChatControls.
	if err = s.m.pg.RecoverFindingRetests(); err != nil {
		t.Fatal(err)
	}
	got, err := s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Fatal("unclaimed durable retest incorrectly sealed during startup", got.Status)
	}
	s.initChatControls()
	state, _ := s.m.pg.ChatControlState(t.Context(), ref)
	if err = s.m.pg.ResumeChatGate(t.Context(), ref, state.Revision); err != nil {
		t.Fatal(err)
	}
	s.dispatchChatGate(ref.GateKey())
	waitRetestIdle(t, s)
	got, err = s.m.pg.FindingRetestForConversation(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StartedAt == nil || got.FinishedAt == nil || got.Status == "pending" || got.Status == "running" {
		t.Fatal("recovered input bypassed retest lifecycle", got)
	}
}
