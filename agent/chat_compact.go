package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/google/uuid"
)

// PrepareChatCompaction never writes the active transcript. The caller publishes
// a separately synced candidate under the same durable admission gate.
func PrepareChatCompaction(ctx context.Context, records []transcript.Record, req llm.CompletionRequest, opts chatstate.CompactOptions, summarize compaction.Summarizer, inputBudget int) ([]transcript.Record, chatstate.ContextView, error) {
	var empty chatstate.ContextView
	if opts.KeepRecent < 1 {
		return nil, empty, errors.New("keep_recent must be at least 1")
	}
	if summarize == nil {
		return nil, empty, errors.New("summary provider unavailable")
	}
	births, err := chatstate.CanonicalChatRecords(records)
	if err != nil {
		return nil, empty, err
	}
	if len(opts.TurnStarts) <= opts.KeepRecent {
		return nil, empty, errors.New("not enough completed exchanges to compact")
	}
	cutoff := opts.TurnStarts[len(opts.TurnStarts)-opts.KeepRecent]
	var msgs []llm.Message
	tail := -1
	for _, r := range births {
		if r.UUID == cutoff {
			tail = len(msgs)
		}
		if r.Message != nil {
			msgs = append(msgs, *r.Message)
		}
	}
	if tail <= 0 {
		return nil, empty, chatstate.ErrAmbiguousHistory
	}
	protected := req
	protected.Messages = msgs[tail:]
	if inputBudget <= 0 || EstimatedChatRequestTokens(protected) > inputBudget {
		return nil, empty, errors.New("protected recent exchanges exceed the model input budget")
	}
	sdk := compaction.New(compaction.Config{KeepRecent: len(msgs) - tail}, func(ctx context.Context, head []llm.Message) (string, error) {
		if opts.Focus != "" {
			head = append(append([]llm.Message{}, head...), llm.UserText("Compression focus: "+opts.Focus))
		}
		return summarize(ctx, head)
	})
	out, ok := sdk.Reactive(ctx, msgs)
	if !ok {
		return nil, empty, errors.New("context summary failed or did not reduce context")
	}
	i := llm.LastBoundaryIndex(out)
	if i < 0 || i+1 >= len(out) {
		return nil, empty, chatstate.ErrAmbiguousHistory
	}
	marker := out[i]
	var meta map[string]any
	_ = json.Unmarshal(marker.Content[0].Input, &meta)
	meta["trigger"] = "manual"
	meta["host_working_set_count"] = len(out) - i
	marker.Content = append([]llm.ContentBlock{}, marker.Content...)
	marker.Content[0].Input, _ = json.Marshal(meta)
	out[i] = marker
	nextReq := req
	nextReq.Messages = llm.MessagesForAPI(out)
	if EstimatedChatRequestTokens(nextReq) > inputBudget {
		return nil, empty, errors.New("summary and protected exchanges exceed the model input budget")
	}
	prepared := append([]transcript.Record{}, records...)
	for _, m := range out[i:] {
		copy := m
		prepared = append(prepared, transcript.Record{UUID: uuid.NewString(), Type: "message", Message: &copy})
	}
	if err = chatstate.ValidateChatRecords(prepared); err != nil {
		return nil, empty, err
	}
	view := MeasureChatContext(nextReq, 0, "", inputBudget+req.MaxTokens, req.MaxTokens, ContextProvenance{})
	view.Source = "manual_compaction"
	view.Compactor = "standard"
	view.Summary = strings.TrimSpace(out[i+1].Text())
	view.CompressionCount = 1
	view.BeforeTokens = EstimatedChatRequestTokens(req)
	view.AfterTokens = view.Used
	return prepared, view, nil
}
