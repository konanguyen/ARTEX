package chatstate

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func revisionRecords() []transcript.Record {
	user := llm.UserText("same prompt")
	tool := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Text: "reasoning", Signature: "signature"}, {Type: llm.BlockToolUse, ID: "skill", Name: "Skill", Input: []byte(`{"skill":"review"}`)}}}
	result := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("skill", "instructions", false)}}
	assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}
	return []transcript.Record{{UUID: "u1", Type: "message", Message: &user}, {UUID: "t1", Type: "message", Message: &tool}, {UUID: "r1", Type: "message", Message: &result}, {UUID: "a1", Type: "message", Message: &assistant, Usage: &llm.Usage{InputTokens: 10}}}
}
func TestChatTranscriptResumeStructured(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	records := revisionRecords()
	boundary := llm.BoundaryMessage(llm.BoundaryMeta{Trigger: "manual", PreTokens: 100, MessagesSummarized: 2})
	records = append(records, transcript.Record{UUID: "b1", Type: "message", Message: &boundary})
	records = append(records, transcript.Record{UUID: "s1", Type: "message", Message: ptrMessage(llm.UserText("summary"))})
	r, err := PrepareTranscript(t.Context(), store, SessionRef{ConversationID: 1}, records)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateTranscriptRevision(store, r); err != nil {
		t.Fatal(err)
	}
	s := agentcore.NewSession(agentcore.Options{Transcript: store, SessionID: r.TranscriptID})
	defer s.Close()
	if err = s.Resume(r.TranscriptID); err != nil {
		t.Fatal(err)
	}
	messages, _ := transcript.Messages(records)
	if !reflect.DeepEqual(s.Messages(), messages) {
		t.Fatalf("structured history changed %+v", s.Messages())
	}
	loaded, _ := store.Load(store.MainPath(r.TranscriptID))
	if loaded[1].Message.Content[0].Signature != "signature" || loaded[0].SessionID != r.TranscriptID || loaded[1].ParentUUID != "u1" {
		t.Fatal("metadata/parent normalization lost")
	}
}
func ptrMessage(m llm.Message) *llm.Message { return &m }
func TestChatTranscriptPublishCrashWindows(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	r, err := PrepareTranscript(t.Context(), store, SessionRef{ConversationID: 1}, revisionRecords())
	if err != nil {
		t.Fatal(err)
	}
	path := store.MainPath(r.TranscriptID)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, original[:len(original)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if ValidateTranscriptRevision(store, r) == nil {
		t.Fatal("partial file validated")
	}
	if err = os.WriteFile(path, append([]byte(" "), original...), 0600); err != nil {
		t.Fatal(err)
	}
	if ValidateTranscriptRevision(store, r) == nil {
		t.Fatal("checksum mismatch validated")
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	writer := store.ResumeWriter(r.TranscriptID, r.EndRecordUUID)
	writer.RecordMessage(llm.UserText("new run"), llm.Usage{})
	writer.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("new answer")}}, llm.Usage{InputTokens: 20})
	if err = ValidateTranscriptRevision(store, r); err != nil {
		t.Fatalf("normal appends invalidated committed prefix: %v", err)
	}
	if _, err = os.Stat(filepath.Clean(path) + ".manifest.json"); err != nil {
		t.Fatal("durable manifest missing", err)
	}
}
func TestChatTranscriptLegacyMapping(t *testing.T) {
	records := revisionRecords()
	records = append(records, transcript.Record{UUID: "u2", Type: "message", Message: ptrMessage(llm.UserText("same prompt"))})
	mapped, err := MapLegacyChatTurns(records, []string{"same prompt", "same prompt"})
	if err != nil || len(mapped) != 2 || mapped[0] != "u1" || mapped[1] != "u2" {
		t.Fatal(mapped, err)
	}
	if _, err = MapLegacyChatTurns(records, []string{"truncated summary"}); !errors.Is(err, ErrAmbiguousHistory) {
		t.Fatalf("guessed legacy mapping %v", err)
	}
}

func TestChatTranscriptRejectsSplitToolExchange(t *testing.T) {
	records := revisionRecords()
	records[2], records[3] = records[3], records[2]
	if _, err := PrepareTranscript(t.Context(), transcript.NewStore(t.TempDir()), SessionRef{ConversationID: 1}, records); err == nil {
		t.Fatal("published an assistant reply between tool use and result")
	}
}
