package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
)

// Provenance is assigned while assembling the request, never inferred from
// arbitrary user text. A serialized block (including nested results) has one owner.
type ContextProvenance struct {
	SystemSegments map[int]string
	ToolCategories map[string]string
	MessageBlocks  map[string]string
}

func contextTokens(v any) int { raw, _ := json.Marshal(v); return (len(raw) + 3) / 4 }
func requestContributions(req llm.CompletionRequest, p ContextProvenance) map[string]int {
	out := map[string]int{"other": 8}
	add := func(category string, n int) {
		switch category {
		case "system", "tools", "skills", "mcp", "conversation", "documents", "memory", "other":
		default:
			category = "other"
		}
		out[category] += n
	}
	for i, s := range req.System {
		add(p.SystemSegments[i], contextTokens(s)+2)
	}
	for _, t := range req.Tools {
		add(p.ToolCategories[t.Name], contextTokens(t)+2)
	}
	for i, m := range req.Messages {
		add("other", contextTokens(m.Role)+4)
		for j, b := range m.Content {
			add(p.MessageBlocks[fmt.Sprintf("%d:%d", i, j)], contextTokens(b))
		}
	}
	return out
}
func EstimatedChatRequestTokens(req llm.CompletionRequest) int {
	n := 0
	for _, v := range requestContributions(req, ContextProvenance{}) {
		n += v
	}
	return n
}
func contextReserve(window, reserve int) (int, bool) {
	if reserve > 0 {
		return reserve, false
	}
	return min(4096, max(0, window/8)), true
}
func MeasureChatContext(req llm.CompletionRequest, profileID int64, model string, window, reserve int, p ContextProvenance) chatstate.ContextView {
	effectiveReserve, estimatedReserve := contextReserve(window, reserve)
	v := chatstate.ContextView{ProfileID: profileID, Model: model, Window: window, OutputReserve: effectiveReserve, ReserveEstimated: estimatedReserve, Estimated: true, UpdatedAt: time.Now().UTC(), Source: "request", Request: req}
	v.Attribution, _ = json.Marshal(p)
	counts := requestContributions(req, p)
	names := make([]string, 0, len(counts))
	for k := range counts {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		v.Categories = append(v.Categories, chatstate.ContextCategory{Name: name, Tokens: counts[name], Estimated: true})
		v.Used += counts[name]
	}
	v.Headroom = window - v.Used - v.OutputReserve
	return v
}
func RebudgetChatContext(v chatstate.ContextView, profileID int64, model string, window, reserve int, identity string) chatstate.ContextView {
	if v.ProfileID != profileID || v.Model != model || v.ProfileIdentity != identity {
		v.MeasuredInput = 0
		v.Estimated = true
		v.Used = 0
		for _, c := range v.Categories {
			v.Used += c.Tokens
		}
	}
	v.ProfileID, v.Model, v.Window, v.OutputReserve, v.ProfileIdentity = profileID, model, window, reserve, identity
	v.OutputReserve, v.ReserveEstimated = contextReserve(window, reserve)
	v.Headroom = window - v.Used - v.OutputReserve
	return v
}

type ChatContextObserver func(context.Context, chatstate.SessionRef, string, chatstate.ContextView) error
type chatContextObserverKey struct{}
type chatContextObserver struct {
	mu       sync.Mutex
	sequence int64
	save     ChatContextObserver
}

func WithChatContextObserver(ctx context.Context, save ChatContextObserver) context.Context {
	return context.WithValue(ctx, chatContextObserverKey{}, &chatContextObserver{save: save})
}
func ObserveChatContext(ctx context.Context, view chatstate.ContextView) error {
	observer, _ := ctx.Value(chatContextObserverKey{}).(*chatContextObserver)
	b := chatRunFrom(ctx)
	if observer == nil || b == nil {
		return nil
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	b.mu.Lock()
	b.contextSequence++
	view.Sequence = b.contextSequence
	b.mu.Unlock()
	view.HistoryRevision = b.run.HistoryRevision
	view.RunID = b.run.ID
	return observer.save(ctx, b.run.Session, b.run.ID, view)
}

// Capture the final schema catalog, including tools revealed during this run.
func chatContextProvenance(ctx context.Context, req llm.CompletionRequest, opts *agentcore.Options, def DeferredInfo) ContextProvenance {
	p := ContextProvenance{SystemSegments: map[int]string{}, ToolCategories: map[string]string{}, MessageBlocks: map[string]string{}}
	for i := range opts.SystemPrompt {
		p.SystemSegments[i] = "system"
	}
	if len(def.GlobalNames) > 0 {
		p.SystemSegments[1] = "mcp"
	}
	for i := range opts.AppendSystemPrompt {
		p.SystemSegments[len(opts.SystemPrompt)+i] = "memory"
	}
	for _, tool := range opts.Tools {
		p.ToolCategories[tool.Name()] = "tools"
	}
	p.ToolCategories["Skill"] = "skills"
	p.ToolCategories["Compress"] = "memory"
	for _, name := range def.Deferred {
		p.ToolCategories[name] = "mcp"
	}
	calls := map[string]string{}
	for i, m := range req.Messages {
		for j, b := range m.Content {
			category := "conversation"
			if b.Type == llm.BlockToolUse {
				category = p.ToolCategories[b.Name]
				calls[b.ID] = category
				if b.Name == "Read" {
					var in struct {
						FilePath string `json:"file_path"`
					}
					if json.Unmarshal(b.Input, &in) == nil {
						documentPath := in.FilePath
						if !filepath.IsAbs(documentPath) {
							documentPath = filepath.Join(opts.WorkingDir, documentPath)
						}
						if rel, err := filepath.Rel(filepath.Join(opts.WorkingDir, "uploads"), documentPath); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
							category = "documents"
							calls[b.ID] = category
						}
						if run := chatRunFrom(ctx); run != nil {
							for _, a := range run.run.Prompt.Attachments {
								if filepath.Clean(in.FilePath) == filepath.Clean(a.Abs) && a.Abs != "" {
									category = "documents"
									calls[b.ID] = category
								}
							}
						}
					}
				}
			}
			if b.Type == llm.BlockToolResult {
				category = calls[b.ToolUseID]
				if category == "" {
					category = "other"
				}
			}
			p.MessageBlocks[fmt.Sprintf("%d:%d", i, j)] = category
		}
	}
	return p
}
func attachChatContext(ctx context.Context, opts *agentcore.Options, def DeferredInfo, noaSession *noaadapter.Session) context.Context {
	if chatRunFrom(ctx) == nil {
		return ctx
	}
	state := &chatContextCapture{ctx: ctx, opts: opts, def: def, compactor: "standard", noaSession: noaSession}
	if opts.Transcript != nil {
		if records, err := opts.Transcript.Load(opts.Transcript.MainPath(opts.SessionID)); err == nil {
			for i, r := range records {
				if r.Message != nil && llm.IsBoundaryMarker(*r.Message) {
					state.count++
					if i+1 < len(records) && records[i+1].Message != nil {
						state.summary = records[i+1].Message.Text()
					}
				}
			}
		}
	}
	switch opts.Compactor.(type) {
	case *localCompactor:
		state.compactor = "native"
	case *noaadapter.Compactor:
		state.compactor = "noa"
	}
	if opts.Compactor == nil && opts.Compaction != nil {
		opts.Compactor = compaction.New(*opts.Compaction, compaction.ProviderSummarizer(opts.Provider, opts.MaxTokens, opts.NonStreaming))
		opts.Compaction = nil
	}
	if opts.Compactor != nil {
		opts.Compactor = observeChatCompactor(opts.Compactor, state.boundary)
	}
	return sidequestion.WithRequestObserver(ctx, state.observe)
}

type chatContextCapture struct {
	noaSession                      *noaadapter.Session
	mu                              sync.Mutex
	ctx                             context.Context
	opts                            *agentcore.Options
	def                             DeferredInfo
	compactor                       string
	identity                        string
	measured, anchorEstimate, count int
	summary                         string
	before, after                   int
}

func (c *chatContextCapture) observe(ctx context.Context, event sidequestion.RequestEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	req := event.Request
	if c.identity != event.Model.Identity {
		c.measured, c.anchorEstimate = 0, 0
		c.identity = event.Model.Identity
	}
	measured := event.Usage.InputTokens + event.Usage.CacheReadTokens + event.Usage.CacheWriteTokens
	if measured > 0 {
		c.measured = measured
		c.anchorEstimate = EstimatedChatRequestTokens(event.AnchorRequest)
	}
	v := MeasureChatContext(req, event.Model.ProfileID, event.Model.Model, event.Model.WindowTokens, req.MaxTokens, chatContextProvenance(c.ctx, req, c.opts, c.def))
	v.ProfileIdentity = event.Model.Identity
	v.MeasuredInput = c.measured
	if c.measured > 0 {
		v.Used = max(0, c.measured+v.Used-c.anchorEstimate)
		v.Estimated = event.Source != "request" || EstimatedChatRequestTokens(req) != c.anchorEstimate
	}
	if c.noaSession != nil {
		state := c.noaSession.State()
		c.count = state.Stats.CompressionCount
		latest := int64(0)
		for _, b := range state.Blocks {
			if b.Active && b.CreatedAt >= latest {
				latest = b.CreatedAt
				c.summary = b.Summary
			}
		}
	}
	v.Source = event.Source
	v.Compactor = c.compactor
	v.CompressionCount = c.count
	v.Summary = c.summary
	v.BeforeTokens = c.before
	v.AfterTokens = c.after
	v.Headroom = v.Window - v.Used - v.OutputReserve
	_ = ObserveChatContext(c.ctx, v) // Telemetry may not change inference semantics.
}
func (c *chatContextCapture) boundary(before, after []llm.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	c.before = compaction.EstimateTokens(llm.MessagesForAPI(before))
	c.after = compaction.EstimateTokens(llm.MessagesForAPI(after))
	i := llm.LastBoundaryIndex(after)
	if i >= 0 && i+1 < len(after) {
		c.summary = after[i+1].Text()
	}
}

type observedChatCompactor struct {
	inner  harness.Compactor
	notify func([]llm.Message, []llm.Message)
}

func observeChatCompactor(inner harness.Compactor, notify func([]llm.Message, []llm.Message)) harness.Compactor {
	return &observedChatCompactor{inner: inner, notify: notify}
}
func (c *observedChatCompactor) track(before, after []llm.Message) []llm.Message {
	i := llm.LastBoundaryIndex(after)
	if i > llm.LastBoundaryIndex(before) {
		// The raw recorder repeats the complete new working set after the marker.
		// Persist its length so history rewrites can recover birth-order records.
		after = append([]llm.Message{}, after...)
		marker := after[i]
		marker.Content = append([]llm.ContentBlock{}, marker.Content...)
		var meta map[string]any
		_ = json.Unmarshal(marker.Content[0].Input, &meta)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["host_working_set_count"] = len(after) - i
		marker.Content[0].Input, _ = json.Marshal(meta)
		after[i] = marker
		if c.notify != nil {
			c.notify(before, after)
		}
	}
	return after
}
func (c *observedChatCompactor) Pre(ctx context.Context, m []llm.Message, n int) []llm.Message {
	return c.track(m, c.inner.Pre(ctx, m, n))
}
func (c *observedChatCompactor) Reactive(ctx context.Context, m []llm.Message) ([]llm.Message, bool) {
	out, ok := c.inner.Reactive(ctx, m)
	if ok {
		out = c.track(m, out)
	}
	return out, ok
}
func (c *observedChatCompactor) IsOverflow(err error) bool { return c.inner.IsOverflow(err) }
func (c *observedChatCompactor) View(ctx context.Context, m []llm.Message) []llm.Message {
	if v, ok := c.inner.(harness.ContextView); ok {
		return v.View(ctx, m)
	}
	return llm.MessagesForAPI(m)
}

// Rebase only attribution metadata. Unchanged retained blocks retain their sole
// category; newly synthesized prose is conversation, never raw prompt text.
func RemapChatContextProvenance(previous chatstate.ContextView, req llm.CompletionRequest) ContextProvenance {
	var old ContextProvenance
	_ = json.Unmarshal(previous.Attribution, &old)
	next := ContextProvenance{SystemSegments: old.SystemSegments, ToolCategories: old.ToolCategories, MessageBlocks: map[string]string{}}
	known := map[string]string{}
	for i, m := range previous.Request.Messages {
		for j, b := range m.Content {
			raw, _ := json.Marshal([]any{m.Role, b})
			known[string(raw)] = old.MessageBlocks[fmt.Sprintf("%d:%d", i, j)]
		}
	}
	for i, m := range req.Messages {
		for j, b := range m.Content {
			raw, _ := json.Marshal([]any{m.Role, b})
			category := known[string(raw)]
			if category == "" {
				switch b.Type {
				case llm.BlockText, llm.BlockThinking:
					category = "conversation"
				case llm.BlockToolUse:
					category = old.ToolCategories[b.Name]
				default:
					category = "other"
				}
			}
			next.MessageBlocks[fmt.Sprintf("%d:%d", i, j)] = category
		}
	}
	return next
}
