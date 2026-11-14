package db

import (
	"errors"
	"github.com/Autumn-27/artex/chatstate"
	"sync"
	"sync/atomic"
	"testing"
)

func TestChatAdmission(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, err := d.CreateConversation("mainagent", "admission", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteConversation(c.ID)
	ref := chatstate.SessionRef{ConversationID: c.ID}
	ctx := t.Context()
	prompt := chatstate.Prompt{Text: "review", Attachments: []chatstate.Attachment{{Name: "report.pdf", Path: "uploads/report.pdf", Size: 4}}}
	first, err := d.AdmitChatInput(ctx, ref, "one", "queue", "", prompt)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := d.AdmitChatInput(ctx, ref, "one", "queue", "", prompt)
	if err != nil || first.ID != duplicate.ID {
		t.Fatalf("duplicate %+v %v", duplicate, err)
	}
	if _, err = d.AdmitChatInput(ctx, ref, "one", "queue", "", chatstate.Prompt{Text: "changed"}); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatalf("changed duplicate %v", err)
	}
	if _, err = d.UpdateChatInput(ctx, ref, first.ID, first.Revision+1, prompt); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatalf("stale edit %v", err)
	}
	reopened, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Both Open calls reapply the idempotent schema.
	state, err := reopened.ChatControlState(ctx, ref)
	if err != nil || len(state.Inputs) != 1 {
		t.Fatalf("state %+v %v", state, err)
	}
	if state.Inputs[0].Prompt.Attachments[0].Path != "uploads/report.pdf" {
		t.Fatal("manifest lost")
	}
}

func controlFixture(t *testing.T) (*DB, chatstate.SessionRef) {
	t.Helper()
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	c, e := d.CreateConversation("mainagent", "control lifecycle", nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.DeleteConversation(c.ID) })
	return d, chatstate.SessionRef{ConversationID: c.ID}
}
func queueInput(t *testing.T, d *DB, ref chatstate.SessionRef, id string) chatstate.Admission {
	t.Helper()
	a, e := d.AdmitChatInput(t.Context(), ref, id, "queue", "", chatstate.Prompt{Text: id})
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func TestChatQueueClaimOnce(t *testing.T) {
	d, ref := controlFixture(t)
	queueInput(t, d, ref, "one")
	var wg sync.WaitGroup
	var successful atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := d.ClaimNextChatInput(t.Context(), ref.GateKey())
			if e != nil {
				t.Error(e)
			}
			if r != nil {
				successful.Add(1)
			}
		}()
	}
	wg.Wait()
	if successful.Load() != 1 {
		t.Fatalf("claims %d", successful.Load())
	}
}
func TestChatRestartUnknownBarrier(t *testing.T) {
	d, ref := controlFixture(t)
	first := queueInput(t, d, ref, "one")
	queueInput(t, d, ref, "two")
	if r, e := d.ClaimNextChatInput(t.Context(), ref.GateKey()); e != nil || r == nil {
		t.Fatal(r, e)
	}
	if e := d.RecoverChatRuns(t.Context()); e != nil {
		t.Fatal(e)
	}
	state, e := d.ChatControlState(t.Context(), ref)
	if e != nil || !state.Paused || state.Inputs[0].Status != "unknown" {
		t.Fatalf("recovery %+v %v", state, e)
	}
	if e = d.ResumeChatGate(t.Context(), ref, state.Revision); !errors.Is(e, chatstate.ErrUnknownRun) {
		t.Fatal(e)
	}
	if e = d.DiscardUnknownChatRun(t.Context(), ref, first.ID, state.Revision); e != nil {
		t.Fatal(e)
	}
	state, _ = d.ChatControlState(t.Context(), ref)
	if !state.Paused {
		t.Fatal("discard resumed queue")
	}
	if e = d.ResumeChatGate(t.Context(), ref, state.Revision); e != nil {
		t.Fatal(e)
	}
	r, e := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if e != nil || r == nil || r.Prompt.Text != "two" {
		t.Fatal(r, e)
	}
	if second, e := d.ClaimNextChatInput(t.Context(), ref.GateKey()); e != nil || second != nil {
		t.Fatal(second, e)
	}
}
func TestChatQueueFailurePauses(t *testing.T) {
	d, ref := controlFixture(t)
	queueInput(t, d, ref, "one")
	queueInput(t, d, ref, "two")
	r, e := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if e != nil {
		t.Fatal(e)
	}
	if e = d.FinishChatRun(t.Context(), r.ID, "failed"); e != nil {
		t.Fatal(e)
	}
	state, _ := d.ChatControlState(t.Context(), ref)
	if !state.Paused || state.Active != nil {
		t.Fatal(state)
	}
	next, e := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if e != nil || next != nil {
		t.Fatal(next, e)
	}
}
func TestChatStopPausesAllSegments(t *testing.T) {
	d, _ := controlFixture(t)
	task, e := d.CreateTaskWithOptions("segment gate", "idle", TaskCreateOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
	_, e = d.Exec(`INSERT INTO main_sessions(exploration_id,seq) VALUES($1,1)`, task.ExplorationID)
	if e != nil {
		t.Fatal(e)
	}
	a := chatstate.SessionRef{TaskID: task.ID, ExplorationID: task.ExplorationID}
	b := a
	b.MainSeg = 1
	queueInput(t, d, a, "one")
	queueInput(t, d, b, "two")
	r, e := d.ClaimNextChatInput(t.Context(), a.GateKey())
	if e != nil || r == nil {
		t.Fatal(r, e)
	}
	state, _ := d.ChatControlState(t.Context(), b)
	if e = d.PauseChatGate(t.Context(), b, state.Revision); e != nil {
		t.Fatal(e)
	}
	if e = d.FinishChatRun(t.Context(), r.ID, "stopped"); e != nil {
		t.Fatal(e)
	}
	other, e := d.ClaimNextChatInput(t.Context(), b.GateKey())
	if e != nil || other != nil {
		t.Fatal("another segment started after stop", other, e)
	}
}

func TestChatSendCannotImplicitlyQueue(t *testing.T) {
	d, ref := controlFixture(t)
	queueInput(t, d, ref, "queued")
	if _, err := d.AdmitChatInput(t.Context(), ref, "send", "send", "", chatstate.Prompt{Text: "must choose queue"}); !errors.Is(err, chatstate.ErrBusy) {
		t.Fatalf("send overtook admission: %v", err)
	}
}

func TestChatSteerTargetsRun(t *testing.T) {
	d, ref := controlFixture(t)
	queueInput(t, d, ref, "one")
	r, e := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if e != nil {
		t.Fatal(e)
	}
	a, e := d.AdmitChatInput(t.Context(), ref, "correction", "steer", r.ID, chatstate.Prompt{Text: "focus auth"})
	if e != nil {
		t.Fatal(e)
	}
	again, e := d.AdmitChatInput(t.Context(), ref, "correction", "steer", r.ID, chatstate.Prompt{Text: "focus auth"})
	if e != nil || again.ID != a.ID {
		t.Fatal(again, e)
	}
	if other, e := d.TakeSteer(t.Context(), "later-run"); e != nil || other != nil {
		t.Fatal(other, e)
	}
	take, e := d.TakeSteer(t.Context(), r.ID)
	if e != nil || take == nil || take.ID != a.ID {
		t.Fatal(take, e)
	}
	if second, e := d.TakeSteer(t.Context(), r.ID); e != nil || second != nil {
		t.Fatal(second, e)
	}
	if e = d.SteerDelivered(t.Context(), r.ID, a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = d.AdmitChatInput(t.Context(), ref, "file", "steer", r.ID, chatstate.Prompt{Text: "x", Attachments: []chatstate.Attachment{{Path: "uploads/a"}}}); !errors.Is(e, chatstate.ErrConflict) {
		t.Fatal("steer accepted file", e)
	}
	if _, e = d.Exec(`INSERT INTO intercept_pending(conversation_id,tool_name) VALUES($1,'Read')`, ref.ConversationID); e != nil {
		t.Fatal(e)
	}
	if _, e = d.AdmitChatInput(t.Context(), ref, "approval", "steer", r.ID, chatstate.Prompt{Text: "x"}); !errors.Is(e, chatstate.ErrBusy) {
		t.Fatal("approval accepted steer", e)
	}
}
func TestChatSteerUndeliveredAfterFinish(t *testing.T) {
	d, ref := controlFixture(t)
	queueInput(t, d, ref, "one")
	r, e := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if e != nil {
		t.Fatal(e)
	}
	a, e := d.AdmitChatInput(t.Context(), ref, "correction", "steer", r.ID, chatstate.Prompt{Text: "focus"})
	if e != nil {
		t.Fatal(e)
	}
	if e = d.FinishChatRun(t.Context(), r.ID, "completed"); e != nil {
		t.Fatal(e)
	}
	state, e := d.ChatControlState(t.Context(), ref)
	if e != nil {
		t.Fatal(e)
	}
	for _, input := range state.Inputs {
		if input.ID == a.ID && input.Status == "undelivered" {
			return
		}
	}
	t.Fatal("steer not visibly undelivered")
}

func TestChatAdmissionExternalSourceIsAtomic(t *testing.T) {
	d, ref := controlFixture(t)
	prompt := chatstate.Prompt{Text: "generated trigger"}
	input, err := d.AdmitExternalChatInput(t.Context(), ref, "external-fixture", prompt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.AdmitChatInput(t.Context(), ref, input.ClientID, "send", "", prompt); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatal("human request reused an external admission", err)
	}
	run, err := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if err != nil || run == nil || run.SourceKind != "external" {
		t.Fatal("source identity was not claimed atomically", run, err)
	}
	saved, err := d.ChatRun(t.Context(), run.ID)
	if err != nil || saved.SourceKind != "external" {
		t.Fatal(saved, err)
	}
}
