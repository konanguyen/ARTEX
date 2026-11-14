package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func compatibilityTask(t *testing.T) (*Server, *Task, http.Handler) {
	t.Helper()
	s, _ := newRetestServer(t)
	s.m.assets = s.m.pg.Assets()
	s.engine = NewEngine(s.m)
	task, err := s.m.CreateTask("legacy chat fixture", "local compatibility checks", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		waitRetestIdle(t, s)
		if _, err := s.m.DeleteTask(task.ID, DeleteTaskOptions{}); err != nil {
			t.Error(err)
		}
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("POST /api/tasks/{id}/chat/controls/{rest...}", s.chatControls)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.pgSendConversationMessage)
	return s, task, mux
}

func compatibilityRequest(h http.Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func compatibilityJSON(t *testing.T, w *httptest.ResponseRecorder, status int, want map[string]string) {
	t.Helper()
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Errorf("response is not the legacy string object: HTTP %d %s: %v", w.Code, w.Body.String(), err)
		return
	}
	if w.Code != status || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("HTTP %d %v; want HTTP %d %v", w.Code, got, status, want)
	}
}

func TestChatCompatibilityRuleAndSegments(t *testing.T) {
	s, task, h := compatibilityTask(t)
	current, err := task.Store.NewMainSession()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, segment string
		want          int
	}{
		{"omitted", "", current.Seq},
		{"null", `,"seg":null`, current.Seq},
		{"negative-one", `,"seg":-1`, current.Seq},
		{"other-negative", `,"seg":-7`, current.Seq},
		{"explicit-zero", `,"seg":0`, 0},
		{"explicit-current", fmt.Sprintf(`,"seg":%d`, current.Seq), current.Seq},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "compatibility-" + tc.name
			w := compatibilityRequest(h, "/api/chat?task="+task.ID, `{"message":"hint `+text+`"`+tc.segment+`}`)
			compatibilityJSON(t, w, 200, map[string]string{"reply": "Hint saved for the planner's next round: " + text, "mode": "rule"})
			waitRetestIdle(t, s)
			nodes, err := task.Store.ListByKind(db.KindHint, 100)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, node := range nodes {
				var payload struct{ Text string }
				if err := json.Unmarshal(node.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Text == text {
					count++
				}
			}
			if count != 1 {
				t.Errorf("rule side effects=%d; want exactly one saved hint", count)
			}
			ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID, MainSeg: tc.want}
			runs, err := s.m.pg.ChatHistoryRuns(t.Context(), ref)
			if err != nil {
				t.Fatal(err)
			}
			var matched []chatstate.Run
			for _, run := range runs {
				if run.Prompt.Text == "hint "+text {
					matched = append(matched, run)
				}
			}
			if len(matched) != 1 || matched[0].Status != "completed" {
				t.Fatalf("durable run for segment %d: %+v", tc.want, matched)
			}
			var users, replies int
			if err := s.m.pg.QueryRow(`SELECT count(*) FILTER (WHERE kind='user'),count(*) FILTER (WHERE kind='text') FROM activity WHERE exploration_id=$1 AND run_id=$2 AND main_seg=$3`, task.ExpID, matched[0].ID, tc.want).Scan(&users, &replies); err != nil {
				t.Fatal(err)
			}
			if users != 1 || replies != 1 {
				t.Errorf("persisted user/reply rows=%d/%d; want 1/1", users, replies)
			}
		})
	}
}

func TestChatCompatibilityLLMResponseAndBusy(t *testing.T) {
	s, task, h := compatibilityTask(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	p := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return llm.Message{}, "", llm.Usage{}, ctx.Err()
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
	}}
	main := agent.NewMainAgent(p, "compatibility", s.m.dir, transcript.NewStore(filepath.Join(s.m.dir, "transcripts")), 10000, 1)
	main.SetNonStreaming(func() bool { return true })
	s.llmOn, s.llmProv = true, p
	s.taskAgents = map[string]*taskAgentBundle{task.ID: {main: main}}
	w := compatibilityRequest(h, "/api/chat?task="+task.ID, `{"message":"first"}`)
	compatibilityJSON(t, w, 202, map[string]string{"status": "accepted", "mode": "llm"})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted legacy request did not start the model")
	}
	w = compatibilityRequest(h, "/api/chat?task="+task.ID, `{"message":"hint must-not-run"}`)
	if w.Code != 409 {
		t.Fatalf("busy legacy send: HTTP %d %s; want 409", w.Code, w.Body.String())
	}
	var inputs int
	ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID}
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM chat_control_inputs WHERE gate_key=$1`, ref.GateKey()).Scan(&inputs); err != nil || inputs != 1 || calls.Load() != 1 {
		t.Fatalf("busy send admitted/executed extra work: inputs=%d calls=%d err=%v", inputs, calls.Load(), err)
	}
}

func TestChatCompatibilityConversationResponse(t *testing.T) {
	s, _, h := compatibilityTask(t)
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
	}})
	c, err := s.m.pg.CreateConversation("auto", "compatibility", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitRetestIdle(t, s); s.m.pg.DeleteConversation(c.ID) })
	w := compatibilityRequest(h, fmt.Sprintf("/api/conversations/%d/messages", c.ID), `{"message":"hello"}`)
	compatibilityJSON(t, w, 202, map[string]string{"status": "accepted"})
}

func TestChatCompatibilityQueuedAndDeletingTask(t *testing.T) {
	s, task, h := compatibilityTask(t)
	ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID}
	if err := s.m.pg.PauseChatGate(t.Context(), ref, -1); err != nil {
		t.Fatal(err)
	}
	w := compatibilityRequest(h, "/api/tasks/"+task.ID+"/chat/controls/inputs?seg=0", `{"client_request_id":"durable-queue","mode":"queue","message":"hint queued-once"}`)
	var admission chatstate.Admission
	if err := json.Unmarshal(w.Body.Bytes(), &admission); err != nil || w.Code != 202 || admission.ID == "" || admission.Status != "queued" {
		t.Fatalf("new control endpoint lost durable admission: HTTP %d %s err=%v", w.Code, w.Body.String(), err)
	}
	w = compatibilityRequest(h, "/api/chat?task="+task.ID, `{"message":"hint cannot-jump-queue"}`)
	if w.Code != 409 {
		t.Fatalf("legacy send jumped paused queue: HTTP %d %s", w.Code, w.Body.String())
	}
	if err := s.m.pg.ResumeChatGate(t.Context(), ref, -1); err != nil {
		t.Fatal(err)
	}
	s.dispatchChatGate(ref.GateKey())
	waitRetestIdle(t, s)
	if !s.engine.BeginDelete(task.ID) {
		t.Fatal("could not install deletion barrier")
	}
	defer s.engine.AbortDelete(task.ID, false)
	w = compatibilityRequest(h, "/api/chat?task="+task.ID, `{"message":"hint cannot-run-after-delete"}`)
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("deleting task accepted work: HTTP %d %s", w.Code, w.Body.String())
	}
	nodes, err := task.Store.ListByKind(db.KindHint, 100)
	if err != nil || len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), `"queued-once"`) {
		t.Fatalf("rejected or queued work executed extra side effects: %+v err=%v", nodes, err)
	}
}

func TestChatCompatibilityCompletionFailures(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   error
	}{
		{"failed", agent.ErrChatPersistence},
		{"stopped", chatstate.ErrBusy},
		{"unknown", chatstate.ErrUnknownRun},
		{"completed", agent.ErrChatPersistence}, // No persisted reply must not become a successful empty response.
	} {
		t.Run(tc.status, func(t *testing.T) {
			s, task, _ := compatibilityTask(t)
			ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID}
			a, err := s.m.pg.AdmitChatInput(t.Context(), ref, "terminal-"+tc.status, "send", "", chatstate.Prompt{Text: "terminal status fixture"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.m.pg.ClaimNextChatInput(t.Context(), ref.GateKey())
			if err != nil || run == nil {
				t.Fatal(run, err)
			}
			if err = s.m.pg.FinishChatRun(t.Context(), run.ID, tc.status); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err = s.waitCompatibilityReply(ctx, a); !errors.Is(err, tc.want) {
				t.Fatalf("terminal %s: %v; want %v", tc.status, err, tc.want)
			}
		})
	}
}

func TestChatCompatibilityPauseDuringAdmission(t *testing.T) {
	s, task, _ := compatibilityTask(t)
	ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID}
	a, err := s.m.pg.AdmitChatInput(t.Context(), ref, "pause-before-claim", "send", "", chatstate.Prompt{Text: "hint must-not-survive-rejection"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.PauseChatGate(t.Context(), ref, -1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = s.waitCompatibilityReply(ctx, a); !errors.Is(err, chatstate.ErrBusy) {
		t.Fatalf("paused admitted request: %v; want busy", err)
	}
	var status string
	if err = s.m.pg.QueryRow(`SELECT status FROM chat_control_inputs WHERE id=$1`, a.ID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("paused legacy prompt remained queued: %s %v", status, err)
	}
	state, err := s.m.pg.ChatControlState(t.Context(), ref)
	if err != nil || state.Active != nil || len(state.Inputs) != 0 || !state.Paused {
		t.Fatalf("pause/cancellation readback: %+v %v", state, err)
	}
}

func TestChatCompatibilityWaitCancellationAndDeletion(t *testing.T) {
	s, task, _ := compatibilityTask(t)
	ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID}
	a, err := s.m.pg.AdmitChatInput(t.Context(), ref, "wait-cancellation", "send", "", chatstate.Prompt{Text: "no claim"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.waitCompatibilityReply(ctx, a); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled HTTP wait: %v", err)
	}
	if _, err = s.m.DeleteTask(task.ID, DeleteTaskOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = s.waitCompatibilityReply(ctx, a); !errors.Is(err, chatstate.ErrGone) {
		t.Fatalf("deleted request wait: %v; want gone", err)
	}
}

func TestChatCompatibilityBlankTaskPrompts(t *testing.T) {
	for _, llmMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("llm-%t", llmMode), func(t *testing.T) {
			s, task, h := compatibilityTask(t)
			if llmMode {
				p := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
				}}
				main := agent.NewMainAgent(p, "compatibility", s.m.dir, transcript.NewStore(filepath.Join(s.m.dir, "transcripts")), 10000, 1)
				main.SetNonStreaming(func() bool { return true })
				s.llmOn, s.llmProv = true, p
				s.taskAgents = map[string]*taskAgentBundle{task.ID: {main: main}}
			}
			for _, tc := range []struct{ body, text string }{
				{`{}`, ""},
				{`{"message":""}`, ""},
				{`{"message":"  \t\n"}`, "  \t\n"},
			} {
				w := compatibilityRequest(h, "/api/chat?task="+task.ID, tc.body)
				if llmMode {
					compatibilityJSON(t, w, 202, map[string]string{"status": "accepted", "mode": "llm"})
				} else {
					var out map[string]string
					if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || len(out) != 2 || out["mode"] != "rule" || !strings.HasPrefix(out["reply"], "(Rule mode, no LLM configured) Assets:") {
						t.Errorf("blank task %s: HTTP %d %s; want legacy rule summary", tc.body, w.Code, w.Body.String())
					}
				}
				waitRetestIdle(t, s)
				var stored string
				if err := s.m.pg.QueryRow(`SELECT prompt->>'text' FROM chat_control_inputs WHERE gate_key=$1 ORDER BY ordinal DESC LIMIT 1`, (chatstate.SessionRef{TaskID: mustTaskID(t, task.ID)}).GateKey()).Scan(&stored); err != nil || stored != tc.text {
					t.Errorf("blank task prompt changed or was not persisted: %q %v; want %q", stored, err, tc.text)
				}
			}
		})
	}
}

func TestChatCompatibilityBlankControlAndConversationRejected(t *testing.T) {
	s, task, h := compatibilityTask(t)
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("unused")}}, "end_turn", llm.Usage{}, nil
	}})
	c, err := s.m.pg.CreateConversation("auto", "blank rejection", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.pg.DeleteConversation(c.ID) })
	for _, path := range []string{
		"/api/tasks/" + task.ID + "/chat/controls/inputs?seg=0",
		fmt.Sprintf("/api/conversations/%d/messages", c.ID),
	} {
		w := compatibilityRequest(h, path, `{"client_request_id":"blank-is-invalid","mode":"send","message":" \t"}`)
		if w.Code != 400 {
			t.Errorf("nonlegacy blank prompt %s: HTTP %d %s; want 400", path, w.Code, w.Body.String())
		}
	}
}

func compatibilityStatusFault(t *testing.T, s *Server, table, keyColumn, key, from, to string) func() {
	t.Helper()
	name := fmt.Sprintf("compat_fault_%d", time.Now().UnixNano())
	_, err := s.m.pg.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF NEW.%s='%s' AND OLD.status='%s' AND NEW.status='%s' THEN RAISE EXCEPTION 'injected compatibility status failure'; END IF;
RETURN NEW; END $$;
CREATE TRIGGER %s BEFORE UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, keyColumn, key, from, to, name, table, name))
	if err != nil {
		t.Fatal(err)
	}
	var once atomic.Bool
	remove := func() {
		if once.CompareAndSwap(false, true) {
			if _, err := s.m.pg.Exec(fmt.Sprintf(`DROP TRIGGER %s ON %s; DROP FUNCTION %s()`, name, table, name)); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(remove)
	return remove
}

func TestChatCompatibilityClaimFailureCancelsQueuedInput(t *testing.T) {
	s, task, h := compatibilityTask(t)
	ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID}
	remove := compatibilityStatusFault(t, s, "chat_control_inputs", "gate_key", ref.GateKey(), "queued", "claimed")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/chat?task="+task.ID, strings.NewReader(`{"message":"hint rejected-claim"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, r)
	if w.Code != 500 || time.Since(start) >= 500*time.Millisecond {
		t.Errorf("claim failure waited for request cancellation: HTTP %d after %s %s", w.Code, time.Since(start), w.Body.String())
	}
	var status string
	if err := s.m.pg.QueryRow(`SELECT status FROM chat_control_inputs WHERE gate_key=$1`, ref.GateKey()).Scan(&status); err != nil || status != "cancelled" {
		t.Errorf("rejected claim retained runnable work: %s %v", status, err)
	}
	remove()
	s.dispatchChatGate(ref.GateKey())
	waitRetestIdle(t, s)
	nodes, err := task.Store.ListByKind(db.KindHint, 10)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("failed HTTP request executed later: %+v %v", nodes, err)
	}
}

func TestChatCompatibilityFinishFailureHasFiniteWait(t *testing.T) {
	s, task, h := compatibilityTask(t)
	ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID}
	remove := compatibilityStatusFault(t, s, "chat_control_runs", "session_key", ref.Key(), "running", "completed")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/chat?task="+task.ID, strings.NewReader(`{"message":"hint completed-once"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		cancel()
		<-done
		t.Fatal("rule response has no finite wait after final-status persistence failed")
	}
	if w.Code != 500 {
		t.Errorf("failed final status: HTTP %d %s; want 500", w.Code, w.Body.String())
	}
	remove()
	s.dispatchChatGate(ref.GateKey())
	waitRetestIdle(t, s)
	var status string
	if err := s.m.pg.QueryRow(`SELECT status FROM chat_control_inputs WHERE gate_key=$1`, ref.GateKey()).Scan(&status); err != nil || status != "claimed" {
		t.Errorf("ambiguous claimed work was changed/replayed: %s %v", status, err)
	}
	nodes, err := task.Store.ListByKind(db.KindHint, 10)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ambiguous execution was replayed: %+v %v", nodes, err)
	}
}

func TestChatCompatibilityBlankTaskHistory(t *testing.T) {
	s, task, h := compatibilityTask(t)
	p := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
	}}
	main := agent.NewMainAgent(p, "compatibility", s.m.dir, transcript.NewStore(filepath.Join(s.m.dir, "transcripts")), 10000, 1)
	main.SetNonStreaming(func() bool { return true })
	s.llmOn, s.llmProv = true, p
	s.taskAgents = map[string]*taskAgentBundle{task.ID: {main: main}}
	w := compatibilityRequest(h, "/api/chat?task="+task.ID, `{}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	waitRetestIdle(t, s)
	ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID}
	state, err := s.m.pg.ChatControlState(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.m.pg.ChatRun(t.Context(), state.LatestRunID)
	if err != nil || original.Status != "completed" || original.Prompt.Text != "" || !original.Active {
		t.Fatalf("original blank task run: %+v %v", original, err)
	}
	if _, err = s.changeChatHistory(t.Context(), ref, chatstate.HistoryChange{ClientID: "retry-blank", Kind: "retry", ExpectedRevision: state.Revision}); err != nil {
		t.Fatal("valid persisted blank task prompt cannot be retried", err)
	}
	waitRetestIdle(t, s)
	runs, err := s.m.pg.ChatHistoryRuns(t.Context(), ref)
	if err != nil || len(runs) != 1 || runs[0].ID == original.ID || runs[0].SupersedesRunID != original.ID || runs[0].TurnID != original.TurnID || runs[0].Status != "completed" || runs[0].Prompt.Text != "" {
		t.Fatalf("blank retry changed prompt or did not execute: %+v %v", runs, err)
	}
	preserved, err := s.m.pg.ChatRun(t.Context(), original.ID)
	if err != nil || preserved.Active || preserved.Status != "completed" || preserved.Prompt.Text != original.Prompt.Text || preserved.StartRecordUUID != original.StartRecordUUID || preserved.EndRecordUUID != original.EndRecordUUID {
		t.Fatalf("retry did not preserve the superseded blank attempt: %+v %v", preserved, err)
	}
	state, err = s.m.pg.ChatControlState(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.changeChatHistory(t.Context(), ref, chatstate.HistoryChange{ClientID: "blank-edit", Kind: "edit", TurnID: state.LatestTurnID, ExpectedRevision: state.Revision, Replacement: &chatstate.Prompt{}}); err == nil {
		t.Fatal("new edit control admitted a blank replacement")
	}
	if _, err = s.changeChatHistory(t.Context(), ref, chatstate.HistoryChange{ClientID: "branch-blank", Kind: "branch", TurnID: state.LatestTurnID, ExpectedRevision: state.Revision}); err != nil {
		t.Fatal("valid persisted blank task prompt cannot be branched", err)
	}
}

func TestChatCompatibilityPausePreservesRevisedInput(t *testing.T) {
	s, task, _ := compatibilityTask(t)
	ref := chatstate.SessionRef{TaskID: mustTaskID(t, task.ID), ExplorationID: task.ExpID}
	a, err := s.m.pg.AdmitChatInput(t.Context(), ref, "revised-before-pause", "send", "", chatstate.Prompt{Text: "hint original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.UpdateChatInput(t.Context(), ref, a.ID, a.Revision, chatstate.Prompt{Text: "hint revised"}); err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.PauseChatGate(t.Context(), ref, -1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = s.waitCompatibilityReply(ctx, a); !errors.Is(err, chatstate.ErrBusy) {
		t.Fatal(err)
	}
	var status, prompt string
	if err = s.m.pg.QueryRow(`SELECT status,prompt->>'text' FROM chat_control_inputs WHERE id=$1`, a.ID).Scan(&status, &prompt); err != nil || status != "queued" || prompt != "hint revised" {
		t.Fatalf("legacy rejection cancelled another client's revision: %s %q %v", status, prompt, err)
	}
}
