package sidequestion

import (
	"context"
	"iter"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestStreamUsageDeltaPerCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []llm.StreamEvent
	}{
		{"anthropic", []llm.StreamEvent{
			{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 100, OutputTokens: 1, CacheReadTokens: 60, CacheWriteTokens: 10}},
			{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 9}},
			{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 10}},
		}},
		{"openai-final", []llm.StreamEvent{{Type: llm.SEMessageDelta, Usage: llm.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60, CacheWriteTokens: 10}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeProvider{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(y func(llm.StreamEvent, error) bool) {
					for _, ev := range tc.events {
						if !y(ev, nil) {
							return
						}
					}
					if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: "answer"}, nil) {
						return
					}
					y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
				}
			}}
			var total llm.Usage
			for range 2 {
				out, err := (SideQuestionService{p}).Answer(t.Context(), llm.CompletionRequest{}, true, nil)
				if err != nil {
					t.Fatal(err)
				}
				want := llm.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60, CacheWriteTokens: 10}
				if out.Usage != want {
					t.Fatalf("per-call usage=%+v want %+v", out.Usage, want)
				}
				total.Add(out.Usage)
			}
			if total.OutputTokens != 40 || total.InputTokens != 200 || total.CacheReadTokens != 120 || total.CacheWriteTokens != 20 {
				t.Fatalf("cross-call usage=%+v", total)
			}
		})
	}
}
