package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Autumn-27/artex/chatstate"
)

// QueueUndeliveredSteer atomically replaces the warning with one follow-up in
// the correction's original session. Its derived identity survives lost replies.
func (d *DB) QueueUndeliveredSteer(ctx context.Context, ref chatstate.SessionRef, id string, expected int64) (out chatstate.Admission, err error) {
	err = d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		source, e := chatInput(ctx, tx, ref, id)
		if errors.Is(e, sql.ErrNoRows) {
			return chatstate.ErrGone
		}
		if e != nil {
			return e
		}
		clientID := "steer-followup-" + source.ID
		out, e = scanChatAdmission(tx.QueryRowContext(ctx, `SELECT `+admissionCols+` FROM chat_control_inputs i JOIN chat_control_sessions s ON s.session_key=i.session_key WHERE i.session_key=$1 AND i.client_id=$2`, ref.Key(), clientID))
		if e == nil {
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if source.Kind != "steer" || source.Status != "undelivered" || source.Revision != expected {
			return chatstate.ErrConflict
		}
		var closed bool
		if e = tx.QueryRowContext(ctx, `SELECT closed FROM chat_control_gates WHERE gate_key=$1`, ref.GateKey()).Scan(&closed); e != nil {
			return e
		}
		if closed {
			return chatstate.ErrGone
		}
		var ordinal int64
		if e = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal),0)+1 FROM chat_control_inputs WHERE gate_key=$1`, ref.GateKey()).Scan(&ordinal); e != nil {
			return e
		}
		payload, _ := json.Marshal(source.Prompt)
		out = chatstate.Admission{ID: chatID(), ClientID: clientID, Kind: "turn", Status: "queued", Session: ref, Prompt: source.Prompt, Ordinal: ordinal, Revision: 1}
		if _, e = tx.ExecContext(ctx, `INSERT INTO chat_control_inputs(id,session_key,gate_key,client_id,fingerprint,kind,status,prompt,ordinal,language) SELECT $1,session_key,gate_key,$2,$3,'turn','queued',prompt,$4,language FROM chat_control_inputs WHERE id=$5`, out.ID, clientID, chatFingerprint(payload), ordinal, source.ID); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE chat_control_inputs SET status='cancelled',revision=revision+1 WHERE id=$1`, source.ID); e != nil {
			return e
		}
		return bumpChatGate(ctx, tx, ref.GateKey())
	})
	return
}
