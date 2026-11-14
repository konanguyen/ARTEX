package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func localHistory() []llm.Message {
	var msgs []llm.Message
	for i := 0; i < 20; i++ {
		msgs = append(msgs, llm.UserText(strings.Repeat("history ", 200)))
	}
	return msgs
}

func TestLocalCompactionExactThresholdAndRawHistory(t *testing.T) {
	for _, window := range []int{16 * 1024, 32 * 1024, 64 * 1024} {
		calls := 0
		c := &localCompactor{window: window, summarize: func(context.Context, []llm.Message) (string, error) { calls++; return "preserved facts", nil }}
		msgs := localHistory()
		before := append([]llm.Message(nil), msgs...)
		out := c.Pre(t.Context(), msgs, window*72/100-1)
		if calls != 0 || !reflect.DeepEqual(out, msgs) {
			t.Fatalf("window=%d compacted before 72%%", window)
		}
		out = c.Pre(t.Context(), msgs, window*72/100)
		if calls != 1 || llm.LastBoundaryIndex(out) < 0 {
			t.Fatalf("window=%d no compaction at72%% calls=%d", window, calls)
		}
		if !reflect.DeepEqual(msgs, before) {
			t.Fatal("input transcript mutated")
		}
		boundary := llm.LastBoundaryIndex(out)
		if !reflect.DeepEqual(out[:boundary], msgs[:len(msgs)-8]) {
			t.Fatal("raw prefix lost")
		}
	}
}

func TestLocalCompactionPreservesToolPairsAndFailure(t *testing.T) {
	msgs := localHistory()
	call := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "paired", Name: "Bash"}}}
	result := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "paired", Content: []llm.ContentBlock{llm.TextBlock("ok")}}}}
	// Place the assistant immediately before the default eight-message tail.
	msgs[11], msgs[12] = call, result
	secondCall, secondResult := call, result
	secondCall.Content = append([]llm.ContentBlock(nil), call.Content...)
	secondCall.Content[0].ID = "parallel"
	secondResult.Content = append([]llm.ContentBlock(nil), result.Content...)
	secondResult.Content[0].ToolUseID = "parallel"
	msgs[10], msgs[13] = secondCall, secondResult
	c := &localCompactor{window: 32768, summarize: func(context.Context, []llm.Message) (string, error) { return "facts", nil }}
	out, ok := c.Reactive(t.Context(), msgs)
	if !ok {
		t.Fatal("not compacted")
	}
	view := llm.MessagesForAPI(out)
	seen := false
	seenCalls := map[string]bool{}
	for _, msg := range view {
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolUse {
				seenCalls[b.ID] = true
			}
			if b.Type == llm.BlockToolResult && !seenCalls[b.ToolUseID] {
				t.Fatalf("cross-boundary orphan result %s", b.ToolUseID)
			}
			if b.Type == llm.BlockToolUse && b.ID == "paired" {
				seen = true
			}
			if b.Type == llm.BlockToolResult && b.ToolUseID == "paired" && !seen {
				t.Fatal("orphan tool result")
			}
		}
	}
	if !seen {
		t.Fatal("paired call disappeared")
	}
	c.summarize = func(context.Context, []llm.Message) (string, error) { return "", errors.New("offline") }
	out, ok = c.Reactive(t.Context(), msgs)
	if ok || !reflect.DeepEqual(out, msgs) {
		t.Fatal("failed summary changed transcript")
	}
}

func TestLocalOutputCapSpillsFullBytesAndCloudUnchanged(t *testing.T) {
	for _, tc := range []struct {
		native      bool
		window, cap int
	}{{true, 16384, 16384}, {true, 32768, 16384}, {true, 65536, 32768}, {true, 131072, 0}, {false, 32768, 0}, {false, 200000, 0}} {
		cfg := compactionConfig(tc.window)
		opts := agentcore.Options{Compaction: cfg, ToolOutputDir: t.TempDir()}
		applyRuntimePolicy(&opts, RuntimePolicy{NativeOllama: tc.native, ContextWindow: tc.window})
		if opts.MaxToolOutputChars != tc.cap {
			t.Fatalf("%+v cap=%d", tc, opts.MaxToolOutputChars)
		}
		if !tc.native && (opts.Compactor != nil || opts.Compaction != cfg) {
			t.Fatal("cloud semantics changed")
		}
		if tc.cap == 0 {
			continue
		}
		full := strings.Repeat("long tool output\n", 5000)
		captured := actool.CaptureOnce(&actool.ToolContext{OutputDir: opts.ToolOutputDir, MaxOutputChars: opts.MaxToolOutputChars}, full)
		if !strings.Contains(captured, "<persisted-output>") || len(captured) >= len(full) {
			t.Fatal("tool output not bounded/spilled")
		}
		files, _ := filepath.Glob(filepath.Join(opts.ToolOutputDir, "*"))
		if len(files) != 1 {
			t.Fatalf("spill files=%v", files)
		}
		data, err := os.ReadFile(files[0])
		if err != nil || string(data) != full {
			t.Fatalf("full output changed: %v", err)
		}
	}
}

func TestLocalCompactionFailureIsBoundedAndVisible(t *testing.T) {
	calls, warnings := 0, 0
	c := &localCompactor{window: 32768, summarize: func(context.Context, []llm.Message) (string, error) {
		calls++
		return "", errors.New("fixture summary failure")
	}, warn: func(string) { warnings++ }}
	msgs := localHistory()
	for i := 0; i < 8; i++ {
		out := c.Pre(t.Context(), msgs, 24576)
		if !reflect.DeepEqual(out, msgs) {
			t.Fatal("failure discarded transcript")
		}
	}
	if calls != 3 || warnings != 3 {
		t.Fatalf("unbounded/silent failures calls=%d warnings=%d", calls, warnings)
	}
	if !c.IsOverflow(errors.New("maximum context length exceeded")) {
		t.Fatal("overflow errors not surfaced")
	}
}

func TestLocalCompactionRetainsPolicyWhenNoaSelected(t *testing.T) {
	opts := agentcore.Options{Compaction: compactionConfig(32768)}
	applyRuntimePolicy(&opts, RuntimePolicy{NativeOllama: true, ContextWindow: 32768})
	compactor := opts.Compactor
	warning := ""
	if _, local := opts.Compactor.(*localCompactor); local {
		warning = "Native Ollama contexts up to 64K retain the 72% compaction policy; experimental noa is skipped"
	} else {
		enableNoa(&opts, func() bool { return true }, t.TempDir(), "local", func(msg string) { warning = msg })
	}
	if opts.Compactor != compactor || !strings.Contains(warning, "72%") {
		t.Fatal("local policy overridden without warning")
	}
}
