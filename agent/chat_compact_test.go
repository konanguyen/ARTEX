package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func compactFixture() ([]transcript.Record, []string) {
	var records []transcript.Record
	var starts []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("u%d", i)
		starts = append(starts, id)
		u := llm.UserText(fmt.Sprintf("user-%d ", i) + strings.Repeat("old detail ", 300))
		a := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "reason", Signature: "preserve-signature"}, llm.TextBlock(fmt.Sprintf("answer-%d", i))}}
		records = append(records, transcript.Record{UUID: id, Type: "message", Message: &u}, transcript.Record{UUID: fmt.Sprintf("a%d", i), Type: "message", Message: &a})
	}
	return records, starts
}
func TestChatManualCompactRetainedTail(t *testing.T) {
	records, starts := compactFixture()
	calls := 0
	summarize := func(_ context.Context, m []llm.Message) (string, error) {
		calls++
		if !strings.Contains(fmt.Sprint(m), "preserve URLs") {
			t.Error("focus absent")
		}
		return "concise summary", nil
	}
	out, view, err := PrepareChatCompaction(t.Context(), records, llm.CompletionRequest{MaxTokens: 100}, chatstate.CompactOptions{KeepRecent: 2, Focus: "preserve URLs", TurnStarts: starts}, summarize, 20000)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || view.Summary == "" || len(out) <= len(records) {
		t.Fatal(view, len(out))
	}
	var msgs []llm.Message
	for _, r := range out {
		if r.Message != nil {
			msgs = append(msgs, *r.Message)
		}
	}
	working := llm.MessagesForAPI(msgs)
	raw := fmt.Sprint(working)
	if strings.Contains(raw, "user-2") || !strings.Contains(raw, "user-3") || !strings.Contains(raw, "user-4") || !strings.Contains(raw, "preserve-signature") {
		t.Fatal("protected tail changed", raw)
	}
	if _, _, err = PrepareChatCompaction(t.Context(), records, llm.CompletionRequest{}, chatstate.CompactOptions{KeepRecent: 0, TurnStarts: starts}, summarize, 20000); err == nil {
		t.Fatal("zero retention accepted")
	}
	if _, _, err = PrepareChatCompaction(t.Context(), records, llm.CompletionRequest{}, chatstate.CompactOptions{KeepRecent: 2, TurnStarts: starts}, summarize, 100); err == nil {
		t.Fatal("oversized protected tail silently shortened")
	}
	// Repeated snapshots must never be counted as additional real exchanges.
	again, _, err := PrepareChatCompaction(t.Context(), out, llm.CompletionRequest{}, chatstate.CompactOptions{KeepRecent: 2, Focus: "preserve URLs", TurnStarts: starts}, summarize, 20000)
	if err != nil {
		t.Fatal(err)
	}
	births, err := chatstate.CanonicalChatRecords(again)
	if err != nil || len(births) != len(records) {
		t.Fatal("snapshot copies became turns", len(births), err)
	}
}
