package db

import (
	"errors"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/transcript"
	"testing"
	"time"
)

func TestChatSideSnapshotHistoryRevision(t *testing.T) {
	d, ref := controlFixture(t)
	queueInput(t, d, ref, "first")
	run, err := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishChatRun(t.Context(), run.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	parent := sidequestion.Parent{ConversationID: ref.ConversationID}
	snap := sidequestion.Snapshot{Parent: parent, RunID: 1, Version: 1, CapturedAt: time.Now(), SourceRunID: run.ID}
	if err = d.SaveSideSnapshot(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	e, _, err := d.StartSideRequest(t.Context(), snap, "side", "question")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := chatstate.PrepareTranscript(t.Context(), transcript.NewStore(t.TempDir()), ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := d.ChatControlState(t.Context(), ref)
	if err = d.PublishChatHistory(t.Context(), ref, state.Revision, revision); err != nil {
		t.Fatal(err)
	}
	snap.Version = 99
	if err = d.SaveSideSnapshot(t.Context(), snap); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatal("late snapshot accepted", err)
	}
	if _, _, err = d.StartSideRequest(t.Context(), snap, "late", "question"); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatal("stale admission", err)
	}
	// In-flight answers remain visible and may finish; a revision does not cancel them.
	if err = d.SaveSideMemory(t.Context(), *e, sidequestion.Memory{History: "old transient memory"}); err != nil {
		t.Fatal("history publication aborted an in-flight side answer", err)
	}
	e.Answer = "sentinel obsolete fact"
	e.Status = "completed"
	e.Sequence = 1
	if ok, err := d.UpdateSideRequest(t.Context(), *e); !ok || err != nil {
		t.Fatal(ok, err)
	}
	hist, err := d.SideHistory(t.Context(), parent.Key(), 0, 20)
	if err != nil || len(hist) != 1 || hist[0].Answer != e.Answer {
		t.Fatal(hist, err)
	}
	got, err := d.SideSnapshot(t.Context(), parent.Key())
	if err != nil || got != nil {
		t.Fatal("stale checkpoint visible", got, err)
	}
	snap.HistoryRevision = 1
	snap.RunID = 2
	snap.Version = 1
	if err = d.SaveSideSnapshot(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	next, _, err := d.StartSideRequest(t.Context(), snap, "next", "followup")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := d.SideReplayPage(t.Context(), *next, 0)
	if err != nil || len(replay) != 1 {
		t.Fatal("compaction lost eligible lineage", replay, err)
	}
	next.Status = "completed"
	next.Sequence = 1
	d.UpdateSideRequest(t.Context(), *next)
	if _, err = d.Exec(`UPDATE chat_control_runs SET active=false WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	snap.SourceRunID = ""
	snap.Version = 2
	if err = d.SaveSideSnapshot(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	last, _, err := d.StartSideRequest(t.Context(), snap, "last", "question")
	if err != nil {
		t.Fatal(err)
	}
	replay, err = d.SideReplayPage(t.Context(), *last, 0)
	if err != nil || len(replay) != 0 {
		t.Fatal("superseded answer replayed", replay, err)
	}
}
func TestChatSideSnapshotSegmentIsolation(t *testing.T) {
	zero, one, two := 0, 1, 2
	p := sidequestion.Parent{TaskID: 1, ExplorationID: 2}
	base := p.Key()
	p.MainSeg = &zero
	if p.Key() != base {
		t.Fatal("legacy key changed")
	}
	p.MainSeg = &one
	a := p.Key()
	p.MainSeg = &two
	if a == p.Key() || a == base {
		t.Fatal("segments share side history")
	}
	p.IntentID = 5
	worker := p.Key()
	p.MainSeg = nil
	if worker != p.Key() {
		t.Fatal("worker key changed")
	}
}
