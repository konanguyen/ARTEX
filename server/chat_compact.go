package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type chatCompactRuntime struct {
	Model     sidequestion.Model
	MaxTokens int
	Provider  llm.Provider
}

func (s *Server) compactChatContext(ctx context.Context, ref chatstate.SessionRef, client string, expected int64, options chatstate.CompactOptions, runtime chatCompactRuntime) (out ChatHistoryResult, err error) {
	operationCtx, release, beginErr := s.beginChatMutation(ctx, ref)
	if beginErr != nil {
		return out, beginErr
	}
	defer release()
	ctx = operationCtx
	if options.KeepRecent < 1 {
		return out, fmt.Errorf("keep_recent must be at least 1")
	}
	token, prior, reserved, err := s.m.pg.BeginChatContextChange(ctx, ref, client, expected, "compact", options)
	if err != nil {
		return out, err
	}
	if prior != nil {
		return s.chatHistoryResult(ctx, *prior), nil
	}
	defer func() {
		if err != nil {
			_ = s.m.pg.AbortChatContextChange(context.Background(), ref, token)
		}
		s.dispatchChatGate(ref.GateKey())
	}()
	view, err := s.m.pg.ChatContext(ctx, ref)
	if err != nil {
		return out, err
	}
	if view.Source == "unobserved" || len(view.Request.System) == 0 {
		return out, fmt.Errorf("run the main agent before compressing context")
	}
	active, err := s.m.pg.ActiveChatTranscript(ctx, ref)
	if err != nil {
		return out, err
	}
	store := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	if active.ID != "" {
		if err = chatstate.ValidateTranscriptRevision(store, *active); err != nil {
			return out, err
		}
	}
	records, err := store.Load(store.MainPath(active.TranscriptID))
	if err != nil {
		return out, err
	}
	runs, err := s.m.pg.ChatHistoryRuns(ctx, ref)
	if err != nil {
		return out, err
	}
	for _, run := range runs {
		if run.Status == "completed" {
			options.TurnStarts = append(options.TurnStarts, run.StartRecordUUID)
		}
	}
	request := view.Request
	request.MaxTokens = runtime.MaxTokens
	reserve := runtime.MaxTokens
	if reserve <= 0 {
		reserve = min(4096, runtime.Model.WindowTokens/8)
	}
	maxSummary := min(2048, reserve)
	candidate, next, err := agent.PrepareChatCompaction(ctx, records, request, options, compaction.ProviderSummarizer(runtime.Provider, maxSummary, !runtime.Model.Streaming), runtime.Model.WindowTokens-reserve)
	if err != nil {
		return out, err
	}
	revision, err := chatstate.PrepareTranscript(ctx, store, ref, candidate)
	if err != nil {
		return out, err
	}
	defer s.cleanupPreparedChatTranscript(revision)
	// Profile selection may change while the summary is in flight.
	if cfg, id, ok := s.chatContextBudget(ref); ok && sideModel(cfg, id, "").Identity != runtime.Model.Identity {
		return out, chatstate.ErrConflict
	}
	measured := agent.MeasureChatContext(next.Request, runtime.Model.ProfileID, runtime.Model.Model, runtime.Model.WindowTokens, reserve, agent.RemapChatContextProvenance(view, next.Request))
	next.Categories = measured.Categories
	next.Attribution = measured.Attribution
	next.ProfileID = runtime.Model.ProfileID
	next.Model = runtime.Model.Model
	next.ProfileIdentity = runtime.Model.Identity
	next.Window = runtime.Model.WindowTokens
	next.OutputReserve = reserve
	next.ReserveEstimated = runtime.MaxTokens <= 0
	next.Headroom = next.Window - next.Used - reserve
	next.Compactor = view.Compactor
	next.CompressionCount = view.CompressionCount + 1
	next.Sequence = view.Sequence
	state, err := s.m.pg.CommitChatContextChange(ctx, ref, token, reserved, revision, &next, "")
	if err != nil {
		return out, err
	}
	return s.chatHistoryResult(ctx, state), nil
}

func (s *Server) chatCompactHTTP(w http.ResponseWriter, r *http.Request, ref chatstate.SessionRef) {
	var in struct {
		Client   string `json:"client_request_id"`
		Expected int64  `json:"expected_revision,string"`
		Focus    string `json:"focus"`
		Keep     *int   `json:"keep_recent"`
	}
	if !decodeConversationRequest(w, r, &in) {
		return
	}
	keep := 2
	if in.Keep != nil {
		keep = *in.Keep
	}
	if keep < 1 {
		chatControlError(w, fmt.Errorf("keep_recent must be at least 1"))
		return
	}
	cfg, id, ok := s.chatContextBudget(ref)
	if !ok {
		chatControlError(w, fmt.Errorf("model unavailable"))
		return
	}
	var provider llm.Provider
	if ref.TaskID > 0 {
		provider = &taskLLMRuntime{s: s, taskID: strconv.FormatInt(ref.TaskID, 10), agentKey: "mainagent"}
	} else if id > 0 {
		provider, _, _ = s.providerForProfile(id)
	} else {
		s.cfgMu.Lock()
		provider = s.llmProv
		s.cfgMu.Unlock()
	}
	if provider == nil {
		chatControlError(w, fmt.Errorf("model unavailable"))
		return
	}
	_, err := s.compactChatContext(r.Context(), ref, in.Client, in.Expected, chatstate.CompactOptions{KeepRecent: keep, Focus: in.Focus}, chatCompactRuntime{Model: sideModel(cfg, id, ""), MaxTokens: cfg.MaxTokens, Provider: provider})
	if err != nil {
		chatControlError(w, err)
		return
	}
	view, err := s.m.pg.ChatContext(r.Context(), ref)
	if err != nil {
		chatControlError(w, err)
		return
	}
	writeJSON(w, 200, view)
}
