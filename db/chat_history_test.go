package db

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestChatTranscriptPublishCrashWindows(t *testing.T) {
	d, ref := controlFixture(t)
	ctx := t.Context()
	store := transcript.NewStore(t.TempDir())
	legacy, err := d.ActiveChatTranscript(ctx, ref)
	if err != nil || legacy.TranscriptID != ref.LegacyTranscriptID() {
		t.Fatal(legacy, err)
	}
	prompt := llm.UserText("first")
	answer := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("answer")}}
	records := []transcript.Record{{UUID: "first", Type: "message", Message: &prompt}, {UUID: "answer", Type: "message", Message: &answer, Usage: &llm.Usage{InputTokens: 10}}}
	prepared, err := chatstate.PrepareTranscript(ctx, store, ref, records)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.PublishChatHistory(ctx, ref, 0, prepared); err != nil {
		t.Fatal(err)
	}
	before, err := d.ActiveChatTranscript(ctx, ref)
	if err != nil || before.TranscriptID != prepared.TranscriptID {
		t.Fatal(before, err)
	}
	next, err := chatstate.PrepareTranscript(ctx, store, ref, records)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.PublishChatHistory(ctx, ref, 0, next); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatalf("stale publication %v", err)
	}
	after, err := d.ActiveChatTranscript(ctx, ref)
	if err != nil || after.TranscriptID != before.TranscriptID {
		t.Fatal(after, err)
	}
	bad := next
	bad.SHA256 = "mismatch"
	if err = d.PublishChatHistory(ctx, ref, 1, bad); err == nil {
		t.Fatal("published mismatched checksum")
	}
	if err = os.WriteFile(store.MainPath(next.TranscriptID), []byte("{partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = d.PublishChatHistory(ctx, ref, 1, next); err == nil {
		t.Fatal("published partial transcript")
	}
	unchanged, err := d.ActiveChatTranscript(ctx, ref)
	if err != nil || unchanged.TranscriptID != before.TranscriptID {
		t.Fatal(unchanged, err)
	}
	// Preparing/copying history consumes no model usage and never writes billing rows.
	for _, cost := range []int{10, 20} {
		_, err = d.AppendConvActivity(ref.ConversationID, Activity{Kind: "result", InputTokens: &cost})
		if err != nil {
			t.Fatal(err)
		}
	}
	var billed int
	if err = d.QueryRow(`SELECT sum(input_tokens) FROM conversation_activities WHERE conversation_id=$1`, ref.ConversationID).Scan(&billed); err != nil || billed != 30 {
		t.Fatal(billed, err)
	}
}

func TestChatBranchTaskSegments(t *testing.T) {
	d, _ := controlFixture(t)
	task, err := d.CreateTaskWithOptions("task branches", "shared task", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
	ref := chatstate.SessionRef{TaskID: task.ID, ExplorationID: task.ExplorationID}
	a, err := d.NewMainSessionBranch(t.Context(), ref, "turn", "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.NewMainSessionBranch(t.Context(), ref, "turn", "b")
	if err != nil {
		t.Fatal(err)
	}
	again, err := d.NewMainSessionBranch(t.Context(), ref, "turn", "a")
	if err != nil || again != a {
		t.Fatal(again, err)
	}
	if a.MainSeg == b.MainSeg || a.GateKey() != ref.GateKey() || b.TaskID != task.ID {
		t.Fatal(a, b)
	}
	segments, err := d.Exploration(task.ExplorationID).ListMainSessions()
	if err != nil || len(segments) != 1 {
		t.Fatal("preparing segments became visible", segments, err)
	}
}

func TestChatBranchSegmentAllocationSharesLegacyLock(t *testing.T) {
	d, _ := controlFixture(t)
	task, err := d.CreateTaskWithOptions("segment allocation", "idle", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
	start := make(chan struct{})
	var wg sync.WaitGroup
	seqs := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			segment, e := d.Exploration(task.ExplorationID).NewMainSession()
			if e != nil {
				t.Error(e)
			} else {
				seqs <- segment.Seq
			}
		}()
	}
	close(start)
	wg.Wait()
	close(seqs)
	seen := map[int]bool{}
	for seq := range seqs {
		if seen[seq] {
			t.Error("reused segment")
		}
		seen[seq] = true
	}
	if len(seen) != 12 {
		t.Fatalf("allocated %d segments", len(seen))
	}
}

func TestChatBranchPreparationRecovery(t *testing.T) {
	d, ref := controlFixture(t)
	state, err := d.ChatControlState(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	target, err := d.PrepareChatHistoryTarget(t.Context(), ref, chatstate.HistoryChange{ClientID: "crashed-branch", Kind: "branch", ExpectedRevision: state.Revision}, true, "turn")
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := d.RecoverChatPreparations(t.Context())
	found := false
	for _, candidate := range abandoned {
		if candidate == target {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatal(abandoned, err)
	}
	if exists, err := d.ChatConversationExists(t.Context(), target.ConversationID); err != nil || exists {
		t.Fatal("crashed preparing child survived recovery", exists, err)
	}
	if exists, err := d.ChatConversationExists(t.Context(), ref.ConversationID); err != nil || !exists {
		t.Fatal("published source removed", exists, err)
	}
}
