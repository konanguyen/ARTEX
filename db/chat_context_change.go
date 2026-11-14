package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Autumn-27/artex/chatstate"
)

// Reserve a short durable operation token; no transaction spans summarization.
func (d *DB) BeginChatContextChange(ctx context.Context, ref chatstate.SessionRef, client string, expected int64, kind string, payload any) (token string, result *chatstate.ControlState, reserved int64, err error) {
	if client == "" || len(client) > 200 {
		return "", nil, 0, errors.New("client request ID is required")
	}
	fingerprint := chatFingerprint(struct {
		Kind    string
		Payload any
	}{kind, payload})
	err = d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		var oldFingerprint, status string
		var body []byte
		e := tx.QueryRowContext(ctx, `SELECT fingerprint,status,result FROM chat_context_changes WHERE session_key=$1 AND client_id=$2`, ref.Key(), client).Scan(&oldFingerprint, &status, &body)
		if e == nil {
			if oldFingerprint != fingerprint {
				return chatstate.ErrConflict
			}
			if status == "published" {
				var state chatstate.ControlState
				if e = json.Unmarshal(body, &state); e != nil {
					return e
				}
				result = &state
				return nil
			}
			if status == "preparing" {
				return chatstate.ErrBusy
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e = chatExpected(ctx, tx, ref.GateKey(), expected); e != nil {
			return e
		}
		var active sql.NullString
		if e = tx.QueryRowContext(ctx, `SELECT active_run_id FROM chat_control_gates WHERE gate_key=$1`, ref.GateKey()).Scan(&active); e != nil {
			return e
		}
		if active.Valid {
			return chatstate.ErrBusy
		}
		if kind == "compact" {
			var unknown bool
			if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chat_control_inputs WHERE gate_key=$1 AND status='unknown')`, ref.GateKey()).Scan(&unknown); e != nil {
				return e
			}
			if unknown {
				return chatstate.ErrUnknownRun
			}
		}
		token = chatID()
		reserved = expected + 1
		if _, e = tx.ExecContext(ctx, `INSERT INTO chat_context_changes(session_key,client_id,token,fingerprint,status) VALUES($1,$2,$3,$4,'preparing') ON CONFLICT(session_key,client_id) DO UPDATE SET token=EXCLUDED.token,status='preparing'`, ref.Key(), client, token, fingerprint); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `UPDATE chat_control_gates SET operation_id=$2,revision=revision+1 WHERE gate_key=$1`, ref.GateKey(), token)
		return e
	})
	return
}
func (d *DB) AbortChatContextChange(ctx context.Context, ref chatstate.SessionRef, token string) error {
	return d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE chat_context_changes SET status='aborted' WHERE token=$1 AND status='preparing'`, token); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE chat_control_gates SET operation_id='',revision=revision+1 WHERE gate_key=$1 AND operation_id=$2`, ref.GateKey(), token)
		return err
	})
}
func (d *DB) CommitChatContextChange(ctx context.Context, ref chatstate.SessionRef, token string, expected int64, revision chatstate.TranscriptRevision, view *chatstate.ContextView, discardInput string) (out chatstate.ControlState, err error) {
	if err = validatePreparedChatRevision(ref, revision); err != nil {
		return
	}
	err = d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		var current int64
		var operation string
		var active sql.NullString
		var closed bool
		if e := tx.QueryRowContext(ctx, `SELECT revision,operation_id,active_run_id,closed FROM chat_control_gates WHERE gate_key=$1`, ref.GateKey()).Scan(&current, &operation, &active, &closed); e != nil {
			return e
		}
		if closed {
			return chatstate.ErrGone
		}
		if current != expected || operation != token {
			return chatstate.ErrConflict
		}
		if active.Valid {
			return chatstate.ErrBusy
		}
		if discardInput != "" {
			result, e := tx.ExecContext(ctx, `UPDATE chat_control_inputs SET status='cancelled',revision=revision+1 WHERE id=$1 AND session_key=$2 AND status='unknown'`, discardInput, ref.Key())
			if e != nil {
				return e
			}
			n, _ := result.RowsAffected()
			if n != 1 {
				return chatstate.ErrConflict
			}
			if _, e = tx.ExecContext(ctx, `UPDATE chat_control_runs SET active=false WHERE input_id=$1`, discardInput); e != nil {
				return e
			}
		}
		if discardInput != "" {
			for _, table := range []string{"activity", "conversation_activities"} {
				if _, e := tx.ExecContext(ctx, `UPDATE `+table+` SET active_attempt=false,history_mutable=false WHERE run_id IN (SELECT id FROM chat_control_runs WHERE input_id=$1)`, discardInput); e != nil {
					return e
				}
			}
		}
		if e := publishChatRevision(ctx, tx, ref, revision); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `UPDATE chat_control_gates SET operation_id='',revision=revision+1 WHERE gate_key=$1`, ref.GateKey()); e != nil {
			return e
		}
		var e error
		out, e = chatControlStateTx(ctx, tx, ref)
		if e != nil {
			return e
		}
		if view != nil {
			view.HistoryRevision = out.HistoryRevision
			view.Sequence++
			view.RunID = ""
			if e = saveChatContextTx(ctx, tx, ref, "", *view); e != nil {
				return e
			}
		}
		body, e := json.Marshal(out)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `UPDATE chat_context_changes SET status='published',result=$2 WHERE token=$1`, token, body)
		return e
	})
	return
}

func (d *DB) RecordChatTranscriptBaseline(ctx context.Context, runID, before string) error {
	run, err := d.ChatRun(ctx, runID)
	if err != nil {
		return err
	}
	return d.chatTx(ctx, run.Session, func(tx *sql.Tx) error {
		var closed bool
		if err := tx.QueryRowContext(ctx, `SELECT closed FROM chat_control_gates WHERE gate_key=$1`, run.Session.GateKey()).Scan(&closed); err != nil {
			return err
		}
		if closed {
			return chatstate.ErrGone
		}
		current, err := chatRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if current.Status != "running" {
			return chatstate.ErrConflict
		}
		if current.BaselineRecorded {
			if current.BeforeRecordUUID == before {
				return nil
			}
			return chatstate.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE chat_control_runs SET baseline_recorded=true,before_record_uuid=$2 WHERE id=$1`, runID, before)
		return err
	})
}
