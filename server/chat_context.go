package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
)

type pendingChatContext struct {
	ref   chatstate.SessionRef
	runID string
	view  chatstate.ContextView
}

func (s *Server) captureChatContext(_ context.Context, ref chatstate.SessionRef, runID string, view chatstate.ContextView) error {
	s.side.mu.Lock()
	defer s.side.mu.Unlock()
	old, ok := s.side.contextPending[ref.Key()]
	if !ok || old.runID != runID || old.view.Sequence < view.Sequence {
		s.side.contextPending[ref.Key()] = pendingChatContext{ref, runID, view}
	}
	return nil
}

func (s *Server) flushChatContexts() error {
	s.side.mu.Lock()
	pending := make([]pendingChatContext, 0, len(s.side.contextPending))
	for _, v := range s.side.contextPending {
		pending = append(pending, v)
	}
	s.side.mu.Unlock()
	var flushErr error
	for _, v := range pending {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.m.pg.SaveChatContext(ctx, v.ref, v.runID, v.view)
		cancel()
		if err == nil || errors.Is(err, chatstate.ErrGone) || errors.Is(err, chatstate.ErrConflict) {
			s.side.mu.Lock()
			if old, ok := s.side.contextPending[v.ref.Key()]; ok && old.runID == v.runID && old.view.Sequence == v.view.Sequence {
				delete(s.side.contextPending, v.ref.Key())
			}
			s.side.mu.Unlock()
		} else {
			flushErr = errors.Join(flushErr, fmt.Errorf("chat context %s: %w", v.ref.Key(), err))
		}
	}
	return flushErr
}

// Resolve capacity from the current selection, even before its next completion.
func (s *Server) chatContextBudget(ref chatstate.SessionRef) (agent.Config, int64, bool) {
	if ref.ConversationID > 0 {
		c, err := s.m.pg.GetConversation(ref.ConversationID)
		if err != nil || c == nil {
			return agent.Config{}, 0, false
		}
		if id := s.effectiveProfileForAgent(c.AgentKey, c.LLMProfileID); id != nil {
			cfg, ok := s.loadProfileConfig(*id)
			return cfg, *id, ok
		}
		return s.globalChatContextBudget()
	}
	runtime := taskLLMRuntime{s: s, taskID: strconv.FormatInt(ref.TaskID, 10), agentKey: "mainagent"}
	if id := s.effectiveProfileForAgent("mainagent", nil); id != nil {
		cfg, ok := s.loadProfileConfig(*id)
		return cfg, *id, ok
	}
	t, err := s.m.pg.GetTask(ref.TaskID)
	if err == nil && t != nil && t.ActiveLLMProfileID != nil {
		cfg, ok := s.loadProfileConfig(*t.ActiveLLMProfileID)
		return cfg, *t.ActiveLLMProfileID, ok
	}
	if cfg, id, ok := s.globalChatContextBudget(); ok {
		return cfg, id, ok
	}
	cfg, ok := runtime.activeCfg()
	return cfg, 0, ok
}

func (s *Server) chatContextHTTP(w http.ResponseWriter, r *http.Request, ref chatstate.SessionRef) {
	v, err := s.m.pg.ChatContext(r.Context(), ref)
	if err != nil {
		chatControlError(w, err)
		return
	}
	if s.side != nil {
		s.side.mu.Lock()
		p, ok := s.side.contextPending[ref.Key()]
		s.side.mu.Unlock()
		if ok {
			state, e := s.m.pg.ChatControlState(r.Context(), ref)
			if e == nil && p.view.HistoryRevision == state.HistoryRevision && state.LatestRunID == p.runID {
				v = p.view
			}
		}
	}
	if cfg, id, ok := s.chatContextBudget(ref); ok {
		model := sideModel(cfg, id, "")
		v = agent.RebudgetChatContext(v, id, cfg.Model, cfg.CompactionWindow(), cfg.MaxTokens, model.Identity)
	}
	writeJSON(w, 200, v)
}

func (s *Server) globalChatContextBudget() (agent.Config, int64, bool) {
	s.cfgMu.Lock()
	cfg, on := s.llmCfg, s.llmOn
	s.cfgMu.Unlock()
	if active, _ := s.m.pg.ActiveProfile(); active != nil {
		if current, ok := s.loadProfileConfig(active.ID); ok && sameLLMConfig(current, cfg) {
			return current, active.ID, on
		}
	}
	return cfg, 0, on
}
