package db

import (
	"context"
	"database/sql"
	"github.com/Autumn-27/artex/chatstate"
)

func (d *DB) CloseChatGate(ctx context.Context, ref chatstate.SessionRef) error {
	return d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE chat_control_gates SET closed=true,paused=true,revision=revision+1 WHERE gate_key=$1`, ref.GateKey())
		return err
	})
}
func (d *DB) ReopenChatGate(ctx context.Context, ref chatstate.SessionRef) error {
	return d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE chat_control_gates SET closed=false,revision=revision+1 WHERE gate_key=$1 AND closed`, ref.GateKey())
		return err
	})
}
func (d *DB) ChatGateClosed(ctx context.Context, ref chatstate.SessionRef) (closed bool, err error) {
	err = d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT closed FROM chat_control_gates WHERE gate_key=$1`, ref.GateKey()).Scan(&closed)
	})
	return
}
