package db

import (
	"encoding/json"
	"errors"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"reflect"
	"testing"
)

func chatContextSnapshotFixture() chatstate.ContextView {
	return chatstate.ContextView{
		Sequence: 3, ProfileID: 42, ProfileIdentity: "fixture-profile", Model: "fixture", Source: "request",
		Used: 100, Window: 32000, OutputReserve: 4096, Estimated: true,
		Request:     llm.CompletionRequest{System: []string{"system 한글"}, Messages: []llm.Message{llm.UserText("preserve full request")}, MaxTokens: 4096},
		Attribution: json.RawMessage(`{"system":7,"conversation":11}`),
	}
}

func assertChatContextSnapshot(t *testing.T, got, want chatstate.ContextView) {
	t.Helper()
	var gotAttribution, wantAttribution any
	if err := json.Unmarshal(got.Attribution, &gotAttribution); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want.Attribution, &wantAttribution); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotAttribution, wantAttribution) {
		t.Fatalf("attribution changed: got %s, want %s", got.Attribution, want.Attribution)
	}
	got.Attribution, want.Attribution = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot changed: got %+v, want %+v", got, want)
	}
}

func TestChatContextSnapshotEnvelopePreservesLegacyPayload(t *testing.T) {
	want := chatContextSnapshotFixture()
	want.RunID = "legacy-run"
	want.HistoryRevision = 9007199254740993
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	request, err := chatContextRequestJSON(want)
	if err != nil {
		t.Fatal(err)
	}
	// Old SQL columns are JSON numbers; ContextView revisions remain strings.
	legacy := `{"run_id":"legacy-run","history_revision":9007199254740993,"sequence":3,"view":` + string(body) + `,"request":` + string(request) + `,"profile_identity":"fixture-profile"}`
	var snapshot persistedChatContext
	if err := json.Unmarshal([]byte(legacy), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.HistoryRevision != want.HistoryRevision || snapshot.Sequence != want.Sequence || snapshot.RunID != want.RunID {
		t.Fatal("legacy snapshot metadata lost precision or identity", snapshot)
	}
	got := snapshot.View
	got.ProfileIdentity = snapshot.ProfileIdentity
	if err := decodeChatContextRequestJSON(snapshot.Request, &got); err != nil {
		t.Fatal(err)
	}
	assertChatContextSnapshot(t, got, want)
}

func TestChatContextSessionSnapshotAndCompactCommit(t *testing.T) {
	d, ref := controlFixture(t)
	view, err := d.ChatContext(t.Context(), ref)
	if err != nil || view.Source != "unobserved" || !view.Estimated {
		t.Fatalf("unobserved session: %+v, %v", view, err)
	}
	var absent bool
	if err := d.QueryRow(`SELECT context_snapshot IS NULL FROM chat_control_sessions WHERE session_key=$1`, ref.Key()).Scan(&absent); err != nil || !absent {
		t.Fatal("new session already has an observation", absent, err)
	}
	view = chatContextSnapshotFixture()
	if err := d.SaveChatContext(t.Context(), ref, "", view); err != nil {
		t.Fatal(err)
	}
	got, err := d.ChatContext(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	assertChatContextSnapshot(t, got, view)
	state, err := d.ChatControlState(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	token, _, reserved, err := d.BeginChatContextChange(t.Context(), ref, "compact", state.Revision, "compact", chatstate.CompactOptions{KeepRecent: 1})
	if err != nil {
		t.Fatal(err)
	}
	prepared := mustPrepareContextTranscript(t, ref)
	view.Used = 40
	view.Summary = "compacted"
	out, err := d.CommitChatContextChange(t.Context(), ref, token, reserved, prepared, &view, "")
	if err != nil || out.HistoryRevision != 1 || view.Sequence != 4 {
		t.Fatal(out, view, err)
	}
	got, err = d.ChatContext(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	assertChatContextSnapshot(t, got, view)
	if err := d.SaveChatContext(t.Context(), ref, "", view); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatal("compact snapshot sequence accepted twice", err)
	}
	if err := d.PublishChatHistory(t.Context(), ref, out.Revision, mustPrepareContextTranscript(t, ref)); err != nil {
		t.Fatal(err)
	}
	got, err = d.ChatContext(t.Context(), ref)
	if err != nil || got.Source != "unobserved" {
		t.Fatal("older-history context remained visible", got, err)
	}
}

func mustPrepareContextTranscript(t *testing.T, ref chatstate.SessionRef) chatstate.TranscriptRevision {
	t.Helper()
	prepared, err := chatstate.PrepareTranscript(t.Context(), transcript.NewStore(t.TempDir()), ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestChatContextStaleHistoryRunAndSequence(t *testing.T) {
	d, ref := controlFixture(t)
	queueInput(t, d, ref, "one")
	run, err := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if err != nil {
		t.Fatal(err)
	}
	view := chatstate.ContextView{HistoryRevision: 0, Sequence: 2, Model: "fixture", Used: 100, Window: 32000, OutputReserve: 4096, Estimated: true}
	if err = d.SaveChatContext(t.Context(), ref, run.ID, view); err != nil {
		t.Fatal(err)
	}
	older := view
	older.Sequence = 1
	if err = d.SaveChatContext(t.Context(), ref, run.ID, older); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatal("stale sequence accepted", err)
	}
	if err = d.FinishChatRun(t.Context(), run.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	queueInput(t, d, ref, "two")
	next, err := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SaveChatContext(t.Context(), ref, next.ID, view); err != nil {
		t.Fatal(err)
	}
	view.Sequence = 100
	if err = d.SaveChatContext(t.Context(), ref, run.ID, view); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatal("old run overwrote context", err)
	}
	view.HistoryRevision = 9
	if err = d.SaveChatContext(t.Context(), ref, next.ID, view); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatal("old history overwrote context", err)
	}
	saved, err := d.ChatContext(t.Context(), ref)
	if err != nil || saved.Used != 100 {
		t.Fatal(saved, err)
	}
}

func TestChatContextBillingRetainsSupersededAttempts(t *testing.T) {
	d, ref := controlFixture(t)
	inactive := false
	ptr := func(n int) *int { return &n }
	for _, a := range []Activity{
		{Kind: "usage", RunID: "old", InputTokens: ptr(10)},
		{Kind: "result", RunID: "old", ActiveAttempt: &inactive, InputTokens: ptr(10), OutputTokens: ptr(2)},
		{Kind: "usage", RunID: "new", InputTokens: ptr(20)},
		{Kind: "usage", RunID: "new", InputTokens: ptr(30)},
	} {
		if _, err := d.AppendConvActivity(ref.ConversationID, a); err != nil {
			t.Fatal(err)
		}
	}
	state, err := d.ChatControlState(t.Context(), ref)
	if err != nil || state.BilledUsage == nil || state.BilledUsage.InputTokens != 40 || state.BilledUsage.OutputTokens != 2 {
		t.Fatal(state.BilledUsage, err)
	}
	if _, err = d.AppendConvActivity(ref.ConversationID, Activity{Kind: "result", RunID: "new", InputTokens: ptr(30), OutputTokens: ptr(4)}); err != nil {
		t.Fatal(err)
	}
	state, err = d.ChatControlState(t.Context(), ref)
	if err != nil || state.BilledUsage.InputTokens != 40 || state.BilledUsage.OutputTokens != 6 {
		t.Fatal("live usage double-counted", state.BilledUsage, err)
	}
}

func TestChatContextGeneratedRunCannotOfferUserHistoryActions(t *testing.T) {
	d, ref := controlFixture(t)
	queueInput(t, d, ref, "generated trigger")
	run, err := d.ClaimNextChatInput(t.Context(), ref.GateKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE chat_control_runs SET source_kind='external' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = d.RecordBoundary(t.Context(), run.ID, "start", "end"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.AppendConvActivity(ref.ConversationID, Activity{Kind: "result", RunID: run.ID}); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishChatRun(t.Context(), run.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	var mutable bool
	if err = d.QueryRow(`SELECT history_mutable FROM conversation_activities WHERE run_id=$1`, run.ID).Scan(&mutable); err != nil || mutable {
		t.Fatal("generated result advertises a genuine user history action", mutable, err)
	}
	state, err := d.ChatControlState(t.Context(), ref)
	if err != nil || state.LatestTurnID != "" {
		t.Fatal("generated trigger became latest user turn", state, err)
	}
}
