package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/sidequestion"
	"strings"
)

func chatSideParent(ref chatstate.SessionRef) sidequestion.Parent {
	p := sidequestion.Parent{ConversationID: ref.ConversationID, TaskID: ref.TaskID, ExplorationID: ref.ExplorationID}
	if ref.TaskID > 0 {
		seg := ref.MainSeg
		p.MainSeg = &seg
	}
	return p
}
func sideControlKey(key string) string {
	if strings.Contains(key, "-main") && !strings.Contains(key, "-worker-") && !strings.Contains(key, "-main-s") {
		return key + "-s0"
	}
	return key
}
func validateSideSnapshotTx(ctx context.Context, tx *sql.Tx, s sidequestion.Snapshot) error {
	var closed bool
	err := tx.QueryRowContext(ctx, `SELECT closed FROM chat_control_gates WHERE gate_key=$1`, func() string {
		if s.Parent.ConversationID > 0 {
			return s.Parent.Key()
		}
		return fmt.Sprintf("task-%d-main", s.Parent.TaskID)
	}()).Scan(&closed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if closed {
		return ErrSideParentGone
	}
	if s.Parent.IntentID > 0 {
		return nil
	}
	if s.Parent.MainSeg != nil && *s.Parent.MainSeg > 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main_sessions WHERE exploration_id=$1 AND seq=$2 AND NOT preparing)`, s.Parent.ExplorationID, *s.Parent.MainSeg).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrSideParentGone
		}
	}
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT history_revision FROM chat_control_sessions WHERE session_key=$1`, sideControlKey(s.Parent.Key())).Scan(&revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if revision != s.HistoryRevision {
		return chatstate.ErrConflict
	}
	if s.SourceRunID != "" {
		var active bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chat_control_runs WHERE id=$1 AND session_key=$2 AND active)`, s.SourceRunID, sideControlKey(s.Parent.Key())).Scan(&active); err != nil {
			return err
		}
		if !active {
			return chatstate.ErrConflict
		}
	}
	return nil
}
func (d *DB) ValidateSideSnapshot(ctx context.Context, s sidequestion.Snapshot) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockSideParent(ctx, tx, s.Parent); err != nil {
		return err
	}
	return validateSideSnapshotTx(ctx, tx, s)
}
func invalidateSideSnapshotTx(ctx context.Context, tx *sql.Tx, parent sidequestion.Parent, history int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE side_question_sessions SET snapshot='{}',memory='{}',history_revision=$2 WHERE session_key=$1`, parent.Key(), history)
	return err
}
func (d *DB) InvalidateSideSnapshot(ctx context.Context, parent sidequestion.Parent, history int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockSideParent(ctx, tx, parent); err != nil {
		return err
	}
	if err = invalidateSideSnapshotTx(ctx, tx, parent, history); err != nil {
		return fmt.Errorf("invalidate side context: %w", err)
	}
	return tx.Commit()
}
