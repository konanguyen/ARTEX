package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestChatContextPartition(t *testing.T) {
	req := llm.CompletionRequest{System: []string{"system api-key-secret", strings.Repeat("MCP catalog ", 50)}, Tools: []llm.ToolSchema{{Name: "Read", Description: "read", InputSchema: map[string]any{"type": "object"}}, {Name: "mcp_lookup", Description: strings.Repeat("schema ", 80)}}, Messages: []llm.Message{llm.UserText("review"), {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("read", strings.Repeat("document ", 90), false)}}}}
	provenance := ContextProvenance{SystemSegments: map[int]string{0: "system", 1: "mcp"}, ToolCategories: map[string]string{"Read": "tools", "mcp_lookup": "mcp"}, MessageBlocks: map[string]string{"0:0": "conversation", "1:0": "documents"}}
	view := MeasureChatContext(req, 1, "fixture", 32000, 4096, provenance)
	total := 0
	for _, category := range view.Categories {
		total += category.Tokens
		if !category.Estimated {
			t.Fatal("local category presented as measured")
		}
	}
	if total != EstimatedChatRequestTokens(req) || view.Used != total || !view.Estimated {
		t.Fatal(view, total)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), "api-key-secret") {
		t.Fatal("context telemetry exposed prompt/config bytes")
	}
	if view.Headroom != 32000-view.Used-4096 {
		t.Fatal(view)
	}
}
func TestChatContextMeasuredAnchor(t *testing.T) {
	dir := t.TempDir()
	store := transcript.NewStore(dir)
	var views []chatstate.ContextView
	ctx := WithChatRun(t.Context(), chatstate.Run{ID: "run", Session: chatstate.SessionRef{ConversationID: 1}, HistoryRevision: 2}, &fakeChatControl{})
	ctx = WithChatContextObserver(ctx, func(_ context.Context, _ chatstate.SessionRef, _ string, view chatstate.ContextView) error {
		views = append(views, view)
		return nil
	})
	p := sidequestion.Bind(captureUsageProvider{stream: func(_ context.Context, y func(llm.StreamEvent, error) bool) {
		for _, event := range []llm.StreamEvent{{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 77, CacheReadTokens: 3}}, {Type: llm.SETextDelta, Text: "answer"}, {Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{OutputTokens: 9}}, {Type: llm.SEMessageStop}} {
			if !y(event, nil) {
				return
			}
		}
	}}, sidequestion.Model{ProfileID: 4, Model: "fixture", Identity: "profile-a", WindowTokens: 32000})
	ca := NewChatAgent(p, "fixture", dir, store, 32000)
	ca.SetMaxTokens(func() int { return 4096 })
	if _, err := ca.Chat(ctx, "auto", "conv-1", "review", 2, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(views) < 2 {
		t.Fatal("actual provider capture missing", views)
	}
	view := views[len(views)-1]
	if view.MeasuredInput != 80 || view.Used < 80 || view.Used == 9999 || view.HistoryRevision != 2 || view.Sequence < 2 {
		t.Fatal(view)
	}
	if view.ProfileID != 4 || view.OutputReserve != 4096 || view.Model != "fixture" {
		t.Fatal(view)
	}
	if !view.Estimated {
		t.Fatal("post-response growth presented as exact")
	}
	sparse := MeasureChatContext(llm.CompletionRequest{Messages: []llm.Message{llm.UserText("sparse")}}, 4, "fixture", 32000, 0, ContextProvenance{})
	if !sparse.Estimated {
		t.Fatal("sparse provider fabricated measurement")
	}
}
func TestChatContextModelSwitch(t *testing.T) {
	old := chatstate.ContextView{ProfileID: 1, Model: "large", Window: 64000, Used: 1000, MeasuredInput: 1000, OutputReserve: 4096, Headroom: 58904, ProfileIdentity: "large-v1", Categories: []chatstate.ContextCategory{{Name: "conversation", Tokens: 900, Estimated: true}}}
	next := RebudgetChatContext(old, 2, "small", 32000, 2048, "small-v1")
	if next.Window != 32000 || next.Headroom != 32000-next.Used-2048 || next.Headroom >= old.Headroom || next.MeasuredInput != 0 || !next.Estimated || next.Used != 900 {
		t.Fatal(next)
	}
	same := RebudgetChatContext(old, 1, "large", 64000, 4096, "large-v1")
	if same.MeasuredInput != 1000 {
		t.Fatal("valid anchor lost", same)
	}
}
func TestChatContextCompactorPolicy(t *testing.T) {
	for _, window := range []int{32768, 65536} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			calls := 0
			opts := agentcore.Options{Provider: captureUsageProvider{}, Compaction: compactionConfig(window), MaxTokens: 4096}
			applyRuntimePolicy(&opts, RuntimePolicy{NativeOllama: true, ContextWindow: window})
			native := opts.Compactor.(*localCompactor)
			native.summarize = func(context.Context, []llm.Message) (string, error) { calls++; return "summary", nil }
			expectedLimit := 16 * 1024
			if window > 32768 {
				expectedLimit = 32 * 1024
			}
			if opts.MaxToolOutputChars != expectedLimit {
				t.Fatal(opts.MaxToolOutputChars)
			}
			observed := observeChatCompactor(opts.Compactor, nil)
			msgs := localHistory()
			observed.Pre(t.Context(), msgs, window*71/100)
			if calls != 0 {
				t.Fatal("compaction policy changed below threshold")
			}
			observed.Pre(t.Context(), msgs, window*73/100)
			if calls != 1 {
				t.Fatal("native 72% policy lost", calls)
			}
			if native.maxOutput > 2048 {
				t.Fatal(native.maxOutput)
			}
		})
	}
}
func TestChatContextLogicalHeaderWithSideCapture(t *testing.T) {
	dir := t.TempDir()
	store := transcript.NewStore(dir)
	ref := chatstate.SessionRef{ConversationID: 42}
	revision, err := chatstate.PrepareTranscript(t.Context(), store, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	logical := ""
	ctx := sidequestion.WithPublisher(t.Context(), func(sidequestion.Snapshot) {})
	ctx = WithChatRun(ctx, chatstate.Run{ID: "run", Session: ref, TranscriptID: revision.TranscriptID, HistoryRevision: 1}, &fakeChatControl{})
	p := captureUsageProvider{stream: func(ctx context.Context, y func(llm.StreamEvent, error) bool) {
		logical = transcript.SessionIDFrom(ctx)
		for _, event := range []llm.StreamEvent{{Type: llm.SETextDelta, Text: "answer"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}} {
			if !y(event, nil) {
				return
			}
		}
	}}
	ca := NewChatAgent(p, "fixture", dir, store, 32000)
	if _, err = ca.Chat(ctx, "auto", "conv-42", "review", 2, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if logical != "conv-42" {
		t.Fatalf("side capture bypassed logical identity: %s", logical)
	}
}
func TestChatContextObserverFailureDoesNotAbort(t *testing.T) {
	ctx := WithChatRun(t.Context(), chatstate.Run{ID: "run", Session: chatstate.SessionRef{ConversationID: 1}}, nil)
	ctx = WithChatContextObserver(ctx, func(context.Context, chatstate.SessionRef, string, chatstate.ContextView) error {
		return fmt.Errorf("telemetry storage offline")
	})
	p := sidequestion.Bind(captureUsageProvider{stream: func(_ context.Context, y func(llm.StreamEvent, error) bool) {
		for _, event := range []llm.StreamEvent{{Type: llm.SETextDelta, Text: "answer"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}} {
			if !y(event, nil) {
				return
			}
		}
	}}, sidequestion.Model{Model: "fixture", WindowTokens: 32000})
	ca := NewChatAgent(p, "fixture", t.TempDir(), nil, 32000)
	if _, err := ca.Chat(ctx, "auto", "conv-1", "review", 2, 0, false, nil); err != nil {
		t.Fatal("telemetry aborted valid inference", err)
	}
}

func TestChatContextSteerAcknowledgedBeforeNextCallWithPublisher(t *testing.T) {
	control := &fakeChatControl{steer: &chatstate.Admission{ID: "steer", TargetRunID: "run", Prompt: chatstate.Prompt{Text: "focus the correction"}}}
	ctx := sidequestion.WithPublisher(t.Context(), func(sidequestion.Snapshot) {})
	ctx = WithChatRun(ctx, chatstate.Run{ID: "run", Session: chatstate.SessionRef{ConversationID: 1}}, control)
	calls := 0
	p := captureUsageProvider{stream: func(_ context.Context, y func(llm.StreamEvent, error) bool) {
		calls++
		if calls == 1 {
			y(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "read", ToolName: "Read"}, nil)
			y(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"file_path":"unneeded"}`}, nil)
			y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"}, nil)
		} else {
			if control.delivered != 1 {
				t.Errorf("correction not durably acknowledged before inference: %d", control.delivered)
			}
			y(llm.StreamEvent{Type: llm.SETextDelta, Text: "corrected"}, nil)
			y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}}
	dir := t.TempDir()
	ca := NewChatAgent(p, "fixture", dir, transcript.NewStore(dir), 32000)
	if _, err := ca.Chat(ctx, "auto", "conv-1", "review", 3, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestChatContextExternalBackgroundPromptHasBoundary(t *testing.T) {
	for _, kind := range []string{"user", "external"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			store := transcript.NewStore(dir)
			control := &fakeChatControl{}
			p := captureUsageProvider{stream: func(_ context.Context, y func(llm.StreamEvent, error) bool) {
				y(llm.StreamEvent{Type: llm.SETextDelta, Text: "generated response"}, nil)
				y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
				y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
			}}
			ca := NewChatAgent(p, "fixture", dir, store, 32000)
			ctx := WithChatRun(t.Context(), chatstate.Run{ID: "generated", SourceKind: kind, Session: chatstate.SessionRef{ConversationID: 1}}, control)
			if _, err := ca.Chat(ctx, "auto", "conv-1", "[Background task fixture] generated trigger", 2, 0, false, nil); err != nil {
				t.Fatal("external trigger cannot establish its own raw boundary", err)
			}
			if control.start == "" {
				t.Fatal("missing external boundary")
			}
		})
	}
}

func TestChatContextRelativeDocumentReadProvenance(t *testing.T) {
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "read", Name: "Read", Input: []byte(`{"file_path":"uploads/spec.pdf"}`)}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("read", "document contents", false)}}}}
	provenance := chatContextProvenance(t.Context(), req, &agentcore.Options{WorkingDir: "/owned/chat"}, DeferredInfo{})
	if provenance.MessageBlocks["1:0"] != "documents" {
		t.Fatal("relative uploaded document result lost its provenance", provenance)
	}
}

func TestChatContextCompactionAttribution(t *testing.T) {
	old := llm.CompletionRequest{System: []string{"system"}, Tools: []llm.ToolSchema{{Name: "Read", Description: "read file"}}, Messages: []llm.Message{llm.UserText("old prompt"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "read", Name: "Read", Input: []byte(`{"file_path":"uploads/spec.pdf"}`)}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("read", "document text", false)}}}}
	previous := MeasureChatContext(old, 1, "fixture", 32000, 100, ContextProvenance{SystemSegments: map[int]string{0: "system"}, ToolCategories: map[string]string{"Read": "tools"}, MessageBlocks: map[string]string{"0:0": "conversation", "1:0": "documents", "2:0": "documents"}})
	next := old
	next.Messages = append([]llm.Message{llm.UserText("compressed summary")}, old.Messages[1:]...)
	v := MeasureChatContext(next, 1, "fixture", 32000, 100, RemapChatContextProvenance(previous, next))
	found := map[string]int{}
	for _, c := range v.Categories {
		found[c.Name] = c.Tokens
	}
	if found["system"] == 0 || found["tools"] == 0 || found["conversation"] == 0 || found["documents"] == 0 {
		t.Fatal("compaction collapsed proven categories into other", v)
	}
}
