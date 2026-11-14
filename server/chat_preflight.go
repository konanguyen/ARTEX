package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// A durable admission must remain visible even if attachment/profile validation
// fails before the agent starts. Persist a real, safely retryable user boundary.
func (s *Server) displayChatPreflightFailure(request context.Context, run chatstate.Run, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	table, ownerColumn, owner := "activity", "exploration_id", run.Session.ExplorationID
	if run.Session.ConversationID > 0 {
		table, ownerColumn, owner = "conversation_activities", "conversation_id", run.Session.ConversationID
	}
	var users, results int
	if err := s.m.pg.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE kind='user'),count(*) FILTER(WHERE kind='result') FROM `+table+` WHERE `+ownerColumn+`=$1 AND run_id=$2`, owner, run.ID).Scan(&users, &results); err != nil {
		return err
	}
	if results > 0 {
		return nil
	}
	emit := func(a db.Activity) error {
		a = chatRunActivity(run, a)
		if run.Session.ConversationID > 0 {
			_, err := s.m.pg.AppendConvActivity(run.Session.ConversationID, a)
			return err
		}
		seg := run.Session.MainSeg
		a.MainSeg = &seg
		_, err := s.m.pg.Exploration(run.Session.ExplorationID).AppendActivity(a)
		return err
	}
	worker := "mainagent"
	if run.Session.ConversationID > 0 {
		c, err := s.m.pg.GetConversation(run.Session.ConversationID)
		if err != nil {
			return err
		}
		if c == nil {
			return chatstate.ErrGone
		}
		worker = c.AgentKey
	}
	if users == 0 {
		if err := emit(userActivityWithAttachments(worker, run.Prompt.Text, run.Prompt.Attachments)); err != nil {
			return err
		}
	}
	current, err := s.m.pg.ChatRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if current.StartRecordUUID == "" {
		store := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
		records, err := store.Load(store.MainPath(run.TranscriptID))
		if err != nil {
			return err
		}
		if err = chatstate.ValidateChatRecords(records); err != nil {
			return err
		}
		last := ""
		if len(records) > 0 {
			last = records[len(records)-1].UUID
		}
		if err = s.m.pg.RecordChatTranscriptBaseline(ctx, run.ID, last); err != nil {
			return err
		}
		writer := store.ResumeWriter(run.TranscriptID, last)
		writer.RecordMessage(llm.UserText(composeAgentMessage(run.Prompt.Text, run.Prompt.Attachments, s.chatPromptDir(run.Session))), llm.Usage{})
		if err = writer.Err(); err != nil {
			return err
		}
		file, err := os.OpenFile(store.MainPath(run.TranscriptID), os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = file.Sync()
		file.Close()
		if err != nil {
			return err
		}
		saved, err := store.Load(store.MainPath(run.TranscriptID))
		if err != nil || len(saved) != len(records)+1 {
			return fmt.Errorf("preflight prompt persistence: %v", err)
		}
		if err = s.m.pg.RecordBoundary(ctx, run.ID, saved[len(saved)-1].UUID, ""); err != nil {
			return err
		}
	}
	message := "Chat could not start: " + cause.Error()
	if err = emit(db.Activity{Worker: worker, Kind: "result", IsError: true, Summary: message, Detail: message}); err != nil {
		return err
	}
	return nil
}
