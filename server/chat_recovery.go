package server

import (
	"context"
	"path/filepath"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/transcript"
)

func (s *Server) discardUnknownChatRun(ctx context.Context, ref chatstate.SessionRef, inputID string, expected int64) (err error) {
	operationCtx, release, beginErr := s.beginChatMutation(ctx, ref)
	if beginErr != nil {
		return beginErr
	}
	defer release()
	ctx = operationCtx
	run, err := s.m.pg.ChatRunForInput(ctx, inputID)
	if err != nil {
		return err
	}
	if run.Session.GateKey() != ref.GateKey() {
		return chatstate.ErrGone
	}
	ref = run.Session
	token, prior, reserved, err := s.m.pg.BeginChatContextChange(ctx, ref, "discard-"+inputID, expected, "discard", inputID)
	if err != nil {
		return err
	}
	if prior != nil {
		return nil
	}
	defer func() {
		if err != nil {
			_ = s.m.pg.AbortChatContextChange(context.Background(), ref, token)
		}
	}()
	if run.Status != "unknown" {
		return chatstate.ErrConflict
	}
	active, err := s.m.pg.ActiveChatTranscript(ctx, ref)
	if err != nil {
		return err
	}
	store := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	if active.ID != "" {
		if err = chatstate.ValidateTranscriptPrefix(store, *active); err != nil {
			return err
		}
	}
	var records []transcript.Record
	if run.BaselineRecorded {
		records, err = chatstate.LoadChatTranscriptThrough(store, active.TranscriptID, run.BeforeRecordUUID)
	} else {
		records, err = store.Load(store.MainPath(active.TranscriptID))
		if err == nil && len(records) > 0 {
			end := -1
			for i, r := range records {
				if r.UUID == run.StartRecordUUID && run.StartRecordUUID != "" {
					end = i
					break
				}
			}
			if end < 0 {
				return chatstate.ErrAmbiguousHistory
			}
			records = records[:end]
		}
	}
	if err != nil {
		return err
	}
	revision, err := chatstate.PrepareTranscript(ctx, store, ref, records)
	if err != nil {
		return err
	}
	defer s.cleanupPreparedChatTranscript(revision)
	_, err = s.m.pg.CommitChatContextChange(ctx, ref, token, reserved, revision, nil, inputID)
	return err
}
