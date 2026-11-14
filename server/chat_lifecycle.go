package server

import (
	"context"
	"crypto/rand"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/sidequestion"
)

func (s *Server) beginChatMutation(ctx context.Context, ref chatstate.SessionRef) (context.Context, func(), error) {
	key := chatBusyKey(ref)
	id := rand.Text()
	ctx, cancel := context.WithCancelCause(ctx)
	s.chatMu.Lock()
	closed, err := s.m.pg.ChatGateClosed(ctx, ref)
	if err != nil || closed {
		s.chatMu.Unlock()
		cancel(chatstate.ErrGone)
		if err == nil {
			err = chatstate.ErrGone
		}
		return ctx, func() {}, err
	}
	if s.chatMutations == nil {
		s.chatMutations = map[string]map[string]context.CancelCauseFunc{}
	}
	if s.chatMutations[key] == nil {
		s.chatMutations[key] = map[string]context.CancelCauseFunc{}
	}
	s.chatMutations[key][id] = cancel
	s.chatMu.Unlock()
	taskID := ""
	if ref.TaskID > 0 {
		taskID = strconv.FormatInt(ref.TaskID, 10)
		if !s.engine.beginTaskOperation(taskID) {
			s.chatMu.Lock()
			delete(s.chatMutations[key], id)
			s.chatMu.Unlock()
			cancel(chatstate.ErrGone)
			return ctx, func() {}, chatstate.ErrGone
		}
	}
	release := func() {
		cancel(agent.AbortChatTurnFinished)
		s.chatMu.Lock()
		delete(s.chatMutations[key], id)
		s.chatMu.Unlock()
		if taskID != "" {
			s.engine.decInflight(taskID)
		}
	}
	return ctx, release, nil
}

func (s *Server) cancelChatMutations(key string) {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	for _, cancel := range s.chatMutations[key] {
		cancel(agent.AbortTaskDeleted)
	}
}

func (s *Server) closeTaskChatGate(ctx context.Context, id string) error {
	t := s.m.ResolveTask(id)
	if t == nil {
		return nil
	}
	n, _ := strconv.ParseInt(id, 10, 64)
	err := s.m.pg.CloseChatGate(ctx, chatstate.SessionRef{TaskID: n, ExplorationID: t.ExpID})
	s.cancelChatMutations(id)
	return err
}

func (s *Server) drainConversationChat(ctx context.Context, id int64) error {
	ref := chatstate.SessionRef{ConversationID: id}
	if err := s.m.pg.CloseChatGate(ctx, ref); err != nil {
		return err
	}
	key := chatBusyKey(ref)
	s.cancelConversation(id)
	s.cancelChatMutations(key)
	done := s.cancelSideWhere(func(p sidequestion.Parent) bool { return p.ConversationID == id })
	for _, ch := range done {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		s.chatMu.Lock()
		busy := s.chatBusy[key] || len(s.chatMutations[key]) > 0
		s.chatMu.Unlock()
		if !busy {
			return nil
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
