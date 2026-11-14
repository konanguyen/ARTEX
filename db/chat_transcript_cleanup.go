package db

import (
	"context"
	"database/sql"

	"github.com/Autumn-27/artex/chatstate"
)

// WithUnpublishedChatTranscript reconciles even an ambiguous publication error
// before removing bytes. The callback runs under the publication lock; errors
// or unknown owners must preserve files.
func (d *DB) WithUnpublishedChatTranscript(ctx context.Context, sessionKey, transcriptID string, cleanup func() error) error {
	var ref chatstate.SessionRef
	err := d.QueryRowContext(ctx, `SELECT COALESCE(conversation_id,0),COALESCE(task_id,0),COALESCE(exploration_id,0),main_seg FROM chat_control_sessions WHERE session_key=$1`, sessionKey).Scan(&ref.ConversationID, &ref.TaskID, &ref.ExplorationID, &ref.MainSeg)
	if err != nil {
		return err
	}
	if ref.Key() != sessionKey {
		return chatstate.ErrConflict
	}
	return d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		var referenced bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chat_transcript_revisions WHERE transcript_id=$1) OR EXISTS(SELECT 1 FROM chat_control_sessions WHERE transcript_id=$1) OR EXISTS(SELECT 1 FROM chat_control_runs WHERE transcript_id=$1)`, transcriptID).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return nil
		}
		return cleanup()
	})
}
