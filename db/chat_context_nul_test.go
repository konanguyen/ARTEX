package db

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestChatContextSnapshotStripsNUL(t *testing.T) {
	d, ref := controlFixture(t)
	view := chatContextSnapshotFixture()
	view.Summary = "before\x00after"
	view.Request.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
		llm.ToolResultText("tool", "PK\x03\x04\x00binary 中文", false),
		llm.TextBlock(`literal \u0000`),
		{Type: llm.BlockToolUse, ID: "nested", Name: "fixture", Input: json.RawMessage(`{"actual":"a\u0000b","literal":"\\u0000"}`)},
	}}}
	view.Attribution = json.RawMessage(`{"note":"a\u0000b","literal":"\\u0000"}`)
	before, err := json.Marshal(view.Request)
	if err != nil {
		t.Fatal(err)
	}
	for seq := int64(1); seq <= 3; seq++ {
		view.Sequence = seq
		if err := d.SaveChatContext(t.Context(), ref, "", view); err != nil {
			t.Fatal(err)
		}
		got, err := d.ChatContext(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if got.Sequence != seq || got.Summary != "beforeafter" {
			t.Fatalf("snapshot metadata: %+v", got)
		}
		blocks := got.Request.Messages[0].Content
		if blocks[0].Content[0].Text != "PK\x03\x04binary 中文" || blocks[1].Text != `literal \u0000` {
			t.Fatalf("tool output or literal changed incorrectly: %+v", blocks)
		}
		for _, raw := range []json.RawMessage{blocks[2].Input, got.Attribution} {
			var fields map[string]string
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["literal"] != `\u0000` || (fields["actual"] != "ab" && fields["note"] != "ab") {
				t.Fatalf("nested JSON not cleaned correctly: %s", raw)
			}
		}
	}
	after, err := json.Marshal(view.Request)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || view.Summary != "before\x00after" {
		t.Fatal("saving mutated the live request")
	}
}
