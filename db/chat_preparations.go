package db

import (
	"context"
	"github.com/Autumn-27/artex/chatstate"
)

// Called before accepting HTTP requests after process recovery. A published
// branch clears preparing in its publication transaction and is never removed.
func (d *DB) RecoverChatPreparations(ctx context.Context) (abandoned []chatstate.SessionRef, err error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(conversation_id,0),COALESCE(task_id,0),COALESCE(exploration_id,0),main_seg FROM chat_control_sessions WHERE preparing FOR UPDATE`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref chatstate.SessionRef
		if err = rows.Scan(&ref.ConversationID, &ref.TaskID, &ref.ExplorationID, &ref.MainSeg); err != nil {
			rows.Close()
			return nil, err
		}
		abandoned = append(abandoned, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, ref := range abandoned {
		if ref.ConversationID > 0 {
			_, err = tx.ExecContext(ctx, `DELETE FROM conversations WHERE id=$1 AND preparing`, ref.ConversationID)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM main_sessions WHERE exploration_id=$1 AND seq=$2 AND preparing`, ref.ExplorationID, ref.MainSeg)
			if err == nil {
				_, err = tx.ExecContext(ctx, `DELETE FROM chat_control_sessions WHERE session_key=$1 AND preparing`, ref.Key())
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE chat_history_changes SET status='failed',target_session_key=NULL WHERE status='preparing'`); err != nil {
		return nil, err
	}
	return abandoned, tx.Commit()
}
