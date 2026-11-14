package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/Autumn-27/artex/chatstate"
)

// Legacy display rows retain their original bytes and billing. Metadata is only
// attached after the server proves a complete chronological transcript mapping.
func (d *DB) ChatLegacyTimeline(ctx context.Context, ref chatstate.SessionRef) ([]Activity, error) {
	table, where, args := "conversation_activities", "conversation_id=$1", []any{ref.ConversationID}
	if ref.ConversationID == 0 {
		table = "activity"
		where = "exploration_id=$1 AND worker='mainagent' AND COALESCE(main_seg,0)=$2"
		args = []any{ref.ExplorationID, ref.MainSeg}
	}
	var exists bool
	if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE `+where+` AND run_id IS NULL AND kind='user')`, args...).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	rows, err := d.QueryContext(ctx, `SELECT id,COALESCE(kind,''),COALESCE(summary,''),COALESCE(detail,''),COALESCE(run_id,''),created_at FROM `+table+` WHERE `+where+` AND active_attempt IS DISTINCT FROM false ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err = rows.Scan(&a.ID, &a.Kind, &a.Summary, &a.Detail, &a.RunID, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type LegacyChatTurn struct {
	FirstActivityID, LastActivityID int64
	StartRecordUUID, EndRecordUUID  string
	Prompt                          chatstate.Prompt
}

func (d *DB) ImportLegacyChatTurns(ctx context.Context, ref chatstate.SessionRef, expected int64, transcriptID string, turns []LegacyChatTurn) error {
	return d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		if err := chatExpected(ctx, tx, ref.GateKey(), expected); err != nil {
			return err
		}
		var busy bool
		if err := tx.QueryRowContext(ctx, `SELECT active_run_id IS NOT NULL FROM chat_control_gates WHERE gate_key=$1`, ref.GateKey()).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return chatstate.ErrBusy
		}
		table := "conversation_activities"
		if ref.ConversationID == 0 {
			table = "activity"
		}
		var ordinal, history int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(turn_ordinal),1) FROM chat_control_runs WHERE session_key=$1`, ref.Key()).Scan(&ordinal); err != nil {
			return err
		}
		ordinal -= int64(len(turns))
		if err := tx.QueryRowContext(ctx, `SELECT history_revision FROM chat_control_sessions WHERE session_key=$1`, ref.Key()).Scan(&history); err != nil {
			return err
		}
		for index, t := range turns {
			var unmapped bool
			if err := tx.QueryRowContext(ctx, `SELECT run_id IS NULL FROM `+table+` WHERE id=$1`, t.FirstActivityID).Scan(&unmapped); err != nil {
				return err
			}
			if !unmapped {
				continue
			}
			inputID, runID, turnID := chatID(), chatID(), chatID()
			payload, _ := json.Marshal(t.Prompt)
			if _, err := tx.ExecContext(ctx, `INSERT INTO chat_control_inputs(id,session_key,gate_key,client_id,fingerprint,kind,status,prompt,ordinal,turn_id) VALUES($1,$2,$3,$4,$5,'turn','completed',$6,$7,$8)`, inputID, ref.Key(), ref.GateKey(), "legacy-"+runID, chatFingerprint(t.Prompt), payload, ordinal+int64(index), turnID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO chat_control_runs(id,input_id,session_key,turn_id,history_revision,transcript_id,start_record_uuid,end_record_uuid,prompt,status,turn_ordinal,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'completed',$10,now())`, runID, inputID, ref.Key(), turnID, history, transcriptID, t.StartRecordUUID, t.EndRecordUUID, payload, ordinal+int64(index)); err != nil {
				return err
			}
			scope := "conversation_id=$5"
			args := []any{t.FirstActivityID, t.LastActivityID, runID, turnID, ref.ConversationID}
			if ref.ConversationID == 0 {
				scope = "exploration_id=$5 AND worker='mainagent' AND COALESCE(main_seg,0)=$6"
				args = []any{t.FirstActivityID, t.LastActivityID, runID, turnID, ref.ExplorationID, ref.MainSeg}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET run_id=$3,turn_id=$4,active_attempt=true,history_mutable=(kind IN ('user','result','text')) WHERE id BETWEEN $1 AND $2 AND run_id IS NULL AND `+scope, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

type chatDisplayImport struct {
	Source string `json:"source"`
	Run    string `json:"run"`
	Turn   string `json:"turn"`
	Detail string `json:"detail"`
}

func cloneChatDisplayPrefixTx(ctx context.Context, tx *sql.Tx, source, target chatstate.SessionRef, mapping []chatDisplayImport) error {
	if len(mapping) == 0 {
		return nil
	}
	payload, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	// Copy one ordered display prefix. Unmapped legacy rows stay read-only, even
	// when their structured boundary cannot safely be reconstructed.
	cte := `WITH mapped AS (SELECT * FROM jsonb_to_recordset($1::jsonb) AS x(source text,run text,turn text,detail text)), cutoff AS (`
	if target.ConversationID > 0 {
		_, err = tx.ExecContext(ctx, cte+`SELECT MAX(a.id) AS id FROM conversation_activities a JOIN mapped m ON m.source=a.run_id WHERE a.conversation_id=$2)
  INSERT INTO conversation_activities(conversation_id,worker,kind,tool,tool_use_id,is_error,summary,detail,turn_id,run_id,active_attempt,history_mutable,created_at)
  SELECT $3,a.worker,a.kind,a.tool,a.tool_use_id,a.is_error,a.summary,CASE WHEN a.kind='user' AND m.run IS NOT NULL THEN m.detail ELSE a.detail END,m.turn,m.run,true,CASE WHEN m.run IS NULL THEN false ELSE a.history_mutable END,a.created_at
  FROM conversation_activities a LEFT JOIN mapped m ON m.source=a.run_id WHERE a.conversation_id=$2 AND a.active_attempt IS DISTINCT FROM false AND (m.run IS NOT NULL OR (a.run_id IS NULL AND a.id<=(SELECT id FROM cutoff))) ORDER BY a.id`, payload, source.ConversationID, target.ConversationID)
	} else {
		_, err = tx.ExecContext(ctx, cte+`SELECT MAX(a.id) AS id FROM activity a JOIN mapped m ON m.source=a.run_id WHERE a.exploration_id=$2 AND COALESCE(a.main_seg,0)=$3)
  INSERT INTO activity(exploration_id,worker,kind,tool,tool_use_id,is_error,summary,detail,metadata,main_seg,turn_id,run_id,active_attempt,history_mutable,created_at)
  SELECT $2,a.worker,a.kind,a.tool,a.tool_use_id,a.is_error,a.summary,CASE WHEN a.kind='user' AND m.run IS NOT NULL THEN m.detail ELSE a.detail END,a.metadata,$4,m.turn,m.run,true,CASE WHEN m.run IS NULL THEN false ELSE a.history_mutable END,a.created_at
  FROM activity a LEFT JOIN mapped m ON m.source=a.run_id WHERE a.exploration_id=$2 AND a.worker='mainagent' AND COALESCE(a.main_seg,0)=$3 AND a.active_attempt IS DISTINCT FROM false AND (m.run IS NOT NULL OR (a.run_id IS NULL AND a.id<=(SELECT id FROM cutoff))) ORDER BY a.id`, payload, source.ExplorationID, source.MainSeg, target.MainSeg)
	}
	return err
}
