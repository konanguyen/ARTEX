package db

import (
	"encoding/json"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/transcript"
	"strings"
	"testing"
)

func TestChatControlsArchiveLegacySnapshotOwnershipAndRollback(t *testing.T) {
	d, external := controlFixture(t)
	view := chatContextSnapshotFixture()
	if err := d.SaveChatContext(t.Context(), external, "", view); err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err := d.QueryRow(`SELECT context_snapshot FROM chat_control_sessions WHERE session_key=$1`, external.Key()).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var different persistedChatContext
	if err := json.Unmarshal(body, &different); err != nil {
		t.Fatal(err)
	}
	different.Sequence, different.View.Sequence = 888, 888
	differentBody, err := json.Marshal(different)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := d.CreateConversation("mainagent", "archive ownership", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteConversation(conversation.ID) })
	archived := chatstate.SessionRef{ConversationID: conversation.ID}
	if err := d.SaveChatContext(t.Context(), archived, "", view); err != nil {
		t.Fatal(err)
	}
	var gates, sessions []byte
	if err := d.QueryRow(`SELECT jsonb_agg(to_jsonb(g)) FROM chat_control_gates g WHERE gate_key=$1`, archived.GateKey()).Scan(&gates); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT jsonb_agg(to_jsonb(s)) FROM chat_control_sessions s WHERE session_key=$1`, archived.Key()).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM chat_control_gates WHERE gate_key=$1`, archived.GateKey()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, target, message string
		externalSnapshot      []byte
		sequence              int64
	}{
		{"external-null", external.Key(), "not part of this archive", nil, view.Sequence},
		{"external-matching", external.Key(), "not part of this archive", body, view.Sequence},
		{"external-different", external.Key(), "not part of this archive", differentBody, view.Sequence},
		{"missing-archive-session", "missing", "not part of this archive", body, view.Sequence},
		{"archive-session-conflict", archived.Key(), "conflicting payload", body, 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := d.Exec(`UPDATE chat_control_sessions SET context_snapshot=$2::jsonb WHERE session_key=$1`, external.Key(), tc.externalSnapshot); err != nil {
				t.Fatal(err)
			}
			var observation map[string]json.RawMessage
			if err := json.Unmarshal(body, &observation); err != nil {
				t.Fatal(err)
			}
			observation["session_key"], _ = json.Marshal(tc.target)
			observation["sequence"], _ = json.Marshal(tc.sequence)
			legacy, err := json.Marshal([]map[string]json.RawMessage{observation})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := d.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			err = RestoreChatControls(t.Context(), tx, map[string]json.RawMessage{
				"chat_control_gates": gates, "chat_control_sessions": sessions, "chat_context_snapshots": legacy,
			})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("snapshot import error = %v, want %q", err, tc.message)
			}
			var inserted int
			if err := tx.QueryRow(`SELECT count(*) FROM chat_control_sessions WHERE session_key=$1`, archived.Key()).Scan(&inserted); err != nil || inserted != 1 {
				t.Fatal("fixture did not insert preceding archive session", inserted, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			var retained bool
			if err := d.QueryRow(`SELECT context_snapshot IS NOT DISTINCT FROM $2::jsonb FROM chat_control_sessions WHERE session_key=$1`, external.Key(), tc.externalSnapshot).Scan(&retained); err != nil || !retained {
				t.Fatal("failed import changed external context", retained, err)
			}
			var remaining int
			if err := d.QueryRow(`SELECT (SELECT count(*) FROM chat_control_gates WHERE gate_key=$1)+(SELECT count(*) FROM chat_control_sessions WHERE session_key=$2)`, archived.GateKey(), archived.Key()).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatal("failed restore retained preceding archive inserts", remaining, err)
			}
		})
	}
}

func TestChatControlsArchiveRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name           string
		legacy, inline bool
	}{
		{"session-snapshot-v4", false, true},
		{"legacy-snapshot-table-v4", true, false},
		{"matching-inline-and-legacy-v4", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) { chatControlsArchiveRoundTrip(t, tc.legacy, tc.inline) })
	}
}

func chatControlsArchiveRoundTrip(t *testing.T, legacy, inline bool) {
	d, _ := controlFixture(t)
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTaskWithOptions("archive chat", "idle", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
	}()
	ref := chatstate.SessionRef{TaskID: task.ID, ExplorationID: task.ExplorationID}
	child, err := d.NewMainSessionBranch(t.Context(), ref, "parent-turn", "child")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := chatstate.PrepareTranscript(t.Context(), transcript.NewStore(t.TempDir()), child, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := d.ChatControlState(t.Context(), child)
	if err = d.PublishChatHistory(t.Context(), child, state.Revision, prepared); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE main_sessions SET preparing=false WHERE exploration_id=$1 AND seq=$2`, child.ExplorationID, child.MainSeg); err != nil {
		t.Fatal(err)
	}
	queueInput(t, d, child, "completed before archive")
	run, e := d.ClaimNextChatInput(t.Context(), child.GateKey())
	if e != nil || run == nil {
		t.Fatal(run, e)
	}
	if e = d.FinishChatRun(t.Context(), run.ID, "completed"); e != nil {
		t.Fatal(e)
	}
	wantContext := chatContextSnapshotFixture()
	wantContext.HistoryRevision, wantContext.RunID = 1, run.ID
	if e = d.SaveChatContext(t.Context(), child, run.ID, wantContext); e != nil {
		t.Fatal(e)
	}
	parked := queueInput(t, d, child, "parked after restore")
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := d.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.ID != archive.ID || claimed.State != Archiving {
		t.Fatalf("archive claim: %+v, %v", claimed, err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FormatVersion != 4 || rawRowCount(snapshot.Tables["chat_transcript_revisions"]) == 0 {
		t.Fatal("v4 archive missing transcript revisions")
	}
	if _, exists := snapshot.Tables["chat_context_snapshots"]; exists {
		t.Fatal("new archive still contains a separate snapshot table")
	}
	var sessions []map[string]json.RawMessage
	if err := json.Unmarshal(snapshot.Tables["chat_control_sessions"], &sessions); err != nil {
		t.Fatal(err)
	}
	var observations []map[string]json.RawMessage
	for _, session := range sessions {
		if raw := session["context_snapshot"]; len(raw) > 0 && string(raw) != "null" {
			var observation map[string]json.RawMessage
			if err := json.Unmarshal(raw, &observation); err != nil {
				t.Fatal(err)
			}
			observation["session_key"] = session["session_key"]
			observations = append(observations, observation)
		}
		if !inline {
			delete(session, "context_snapshot")
		}
	}
	if len(observations) != 1 {
		t.Fatal("session archive lost the current context", observations)
	}
	if legacy {
		snapshot.Tables["chat_context_snapshots"], err = json.Marshal(observations)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Tables["chat_control_sessions"], err = json.Marshal(sessions)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.DataCounts["chat_context_snapshots"] = int64(len(observations))
	}
	// Exercise the serialized archive shape, including old v4's absent column.
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "/offline/chat-v4.tar.zst", "fixture", 100, 50); err != nil {
		t.Fatal(err)
	}
	var hotGates int
	if err := d.QueryRow(`SELECT count(*) FROM chat_control_gates WHERE task_id=$1`, task.ID).Scan(&hotGates); err != nil || hotGates != 0 {
		t.Fatalf("archive retained hot control gates: %d, %v", hotGates, err)
	}
	if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = d.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.ID != archive.ID || claimed.State != Restoring {
		t.Fatalf("restore claim: %+v, %v", claimed, err)
	}
	if _, err := d.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := d.ChatControlState(t.Context(), child)
	if err != nil || !restored.Paused || len(restored.Inputs) != 1 {
		t.Fatal(restored, err)
	}
	if restored.Inputs[0].ID != parked.ID || restored.Inputs[0].Prompt.Text != parked.Prompt.Text || restored.Inputs[0].Status != "queued" || restored.Inputs[0].Session != child {
		t.Fatal(restored, err)
	}
	revision, err := d.ActiveChatTranscript(t.Context(), child)
	if err != nil || revision.TranscriptID != prepared.TranscriptID {
		t.Fatal(revision, err)
	}
	contextView, e := d.ChatContext(t.Context(), child)
	if e != nil || contextView.Used != 100 {
		t.Fatal("context missing", contextView, e)
	}
	assertChatContextSnapshot(t, contextView, wantContext)
	restoredRun, e := d.ChatRun(t.Context(), run.ID)
	if e != nil || restoredRun.Session != child || restoredRun.HistoryRevision != 1 {
		t.Fatal("run identity changed", restoredRun, e)
	}
	if run, err := d.ClaimNextChatInput(t.Context(), ref.GateKey()); err != nil || run != nil {
		t.Fatal("restore dispatched pending work", run, err)
	}
	if TaskArchiveFormatVersion != 4 {
		t.Fatal("archive format not upgraded")
	}
	for _, v := range []int{1, 2, 3, 4} {
		if !IsTaskArchiveFormatSupported(v) {
			t.Fatal(v)
		}
	}
}
