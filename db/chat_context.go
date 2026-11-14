package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/llm"
)

func (d *DB) SaveChatContext(ctx context.Context, ref chatstate.SessionRef, runID string, view chatstate.ContextView) error {
	return d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		return saveChatContextTx(ctx, tx, ref, runID, view)
	})
}

// The caller holds the parent, gate and session locks through chatTx.
func saveChatContextTx(ctx context.Context, tx *sql.Tx, ref chatstate.SessionRef, runID string, view chatstate.ContextView) error {
	var closed bool
	if err := tx.QueryRowContext(ctx, `SELECT closed FROM chat_control_gates WHERE gate_key=$1`, ref.GateKey()).Scan(&closed); err != nil {
		return err
	}
	if closed {
		return chatstate.ErrGone
	}
	var history int64
	var oldBody []byte
	if err := tx.QueryRowContext(ctx, `SELECT history_revision,context_snapshot FROM chat_control_sessions WHERE session_key=$1`, ref.Key()).Scan(&history, &oldBody); err != nil {
		return err
	}
	if history != view.HistoryRevision {
		return chatstate.ErrConflict
	}
	var latest string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM chat_control_runs WHERE session_key=$1 AND active ORDER BY turn_ordinal DESC,created_at DESC LIMIT 1`, ref.Key()).Scan(&latest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if runID != "" && latest != runID {
		return chatstate.ErrConflict
	}
	if len(oldBody) > 0 {
		var old persistedChatContext
		if err := json.Unmarshal(oldBody, &old); err != nil {
			return err
		}
		if old.RunID == runID && old.Sequence >= view.Sequence {
			return chatstate.ErrConflict
		}
	}
	view.RunID = runID
	request, err := chatContextRequestJSON(view)
	if err != nil {
		return err
	}
	body, err := json.Marshal(persistedChatContext{RunID: runID, HistoryRevision: view.HistoryRevision, Sequence: view.Sequence, View: view, Request: request, ProfileIdentity: view.ProfileIdentity})
	if err != nil {
		return err
	}
	// Captured tool output and nested RawMessage fields may contain NUL escapes.
	_, err = tx.ExecContext(ctx, `UPDATE chat_control_sessions SET context_snapshot=$2 WHERE session_key=$1`, ref.Key(), jsonbClean(body))
	return err
}

func (d *DB) ChatContext(ctx context.Context, ref chatstate.SessionRef) (view chatstate.ContextView, err error) {
	err = d.chatTx(ctx, ref, func(tx *sql.Tx) error {
		var body []byte
		err := tx.QueryRowContext(ctx, `SELECT context_snapshot FROM chat_control_sessions WHERE session_key=$1 AND (context_snapshot->>'history_revision')::bigint=history_revision`, ref.Key()).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) {
			view.Source = "unobserved"
			view.Estimated = true
			return nil
		}
		if err != nil {
			return err
		}
		var snapshot persistedChatContext
		if err = json.Unmarshal(body, &snapshot); err != nil {
			return err
		}
		view = snapshot.View
		view.ProfileIdentity = snapshot.ProfileIdentity
		return decodeChatContextRequestJSON(snapshot.Request, &view)
	})
	return
}

type persistedChatContext struct {
	RunID           string                `json:"run_id"`
	HistoryRevision int64                 `json:"history_revision"`
	Sequence        int64                 `json:"sequence"`
	View            chatstate.ContextView `json:"view"`
	Request         json.RawMessage       `json:"request"`
	ProfileIdentity string                `json:"profile_identity"`
}

type persistedChatRequest struct {
	llm.CompletionRequest
	Attribution json.RawMessage `json:"_scopeweaver_context_attribution,omitempty"`
}

func chatContextRequestJSON(view chatstate.ContextView) ([]byte, error) {
	return json.Marshal(persistedChatRequest{CompletionRequest: view.Request, Attribution: view.Attribution})
}
func decodeChatContextRequestJSON(raw []byte, view *chatstate.ContextView) error {
	var request persistedChatRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	view.Request = request.CompletionRequest
	view.Attribution = request.Attribution
	return nil
}
