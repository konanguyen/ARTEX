package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/transcript"
)

func TestChatTranscriptCleanupReconcilesHistoricalAndAmbiguousPublication(t *testing.T) {
	d, ref := controlFixture(t)
	store := transcript.NewStore(t.TempDir())
	prepare := func() chatstate.TranscriptRevision {
		t.Helper()
		r, err := chatstate.PrepareTranscript(t.Context(), store, ref, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cleanup := func(ctx context.Context, r chatstate.TranscriptRevision) error {
		return d.WithUnpublishedChatTranscript(ctx, r.SessionKey, r.TranscriptID, func() error { return chatstate.RemovePreparedTranscript(store, r) })
	}
	old := prepare()
	if err := d.PublishChatHistory(t.Context(), ref, 0, old); err != nil {
		t.Fatal(err)
	}
	current := prepare()
	if err := d.PublishChatHistory(t.Context(), ref, 1, current); err != nil {
		t.Fatal(err)
	}
	// A commit whose reply was lost is already referenced, just like this current revision.
	for _, r := range []chatstate.TranscriptRevision{old, current} {
		if err := cleanup(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(store.MainPath(r.TranscriptID)); err != nil {
			t.Fatal("removed committed history", err)
		}
	}
	orphan := prepare()
	if err := d.PublishChatHistory(t.Context(), ref, 0, orphan); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatal("expected stale publication", err)
	}
	if err := cleanup(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.MainPath(orphan.TranscriptID)); !os.IsNotExist(err) {
		t.Fatal("unpublished bytes survived", err)
	}
	uncertain := prepare()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cleanup(ctx, uncertain); err == nil {
		t.Fatal("cancelled database check succeeded")
	}
	if _, err := os.Stat(store.MainPath(uncertain.TranscriptID)); err != nil {
		t.Fatal("database uncertainty deleted bytes", err)
	}
	if err := d.DeleteConversation(ref.ConversationID); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(t.Context(), uncertain); err == nil {
		t.Fatal("unknown owner accepted")
	}
	if _, err := os.Stat(store.MainPath(uncertain.TranscriptID)); err != nil {
		t.Fatal("unknown owner bytes deleted", err)
	}
}

func TestChatTranscriptCleanupWaitsForPublicationLock(t *testing.T) {
	d, ref := controlFixture(t)
	if _, err := d.ChatControlState(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), `SELECT id FROM conversations WHERE id=$1 FOR UPDATE`, ref.ConversationID); err != nil {
		t.Fatal(err)
	}
	const name = "chat-rev-pending-publication"
	called := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- d.WithUnpublishedChatTranscript(t.Context(), ref.Key(), name, func() error { called <- struct{}{}; return nil })
	}()
	select {
	case <-called:
		t.Fatal("cleanup bypassed publication lock")
	case err := <-done:
		t.Fatal("cleanup returned before publication unlocked", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE chat_control_sessions SET transcript_id=$2 WHERE session_key=$1`, ref.Key(), name); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish after publication")
	}
	select {
	case <-called:
		t.Fatal("deleted bytes from publication that committed while cleanup waited")
	default:
	}
}
