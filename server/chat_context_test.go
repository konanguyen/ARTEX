package server

import (
	"encoding/json"
	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/db"
	"net/http/httptest"
	"testing"
)

func TestChatContextSelectedModelCapacityBeforeCall(t *testing.T) {
	f := newHistoryFixture(t)
	run := f.turn(t, "first")
	profile := &db.LLMProfile{Name: "context-capacity", Format: "openai", Model: "large", APIKey: "api-key-secret", ContextWindowK: 64, MaxTokens: 4096}
	id, err := f.s.m.pg.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.m.pg.DeleteProfile(id)
	if _, err = f.s.m.pg.Exec(`UPDATE conversations SET llm_profile_id=$2 WHERE id=$1`, f.ref.ConversationID, id); err != nil {
		t.Fatal(err)
	}
	cfg, ok := f.s.loadProfileConfig(id)
	if !ok {
		t.Fatal("config missing")
	}
	view := chatstate.ContextView{ProfileID: id, Model: cfg.Model, ProfileIdentity: sideModel(cfg, id, "").Identity, HistoryRevision: run.HistoryRevision, Sequence: 2, Used: 1000, MeasuredInput: 1000, Categories: []chatstate.ContextCategory{{Name: "conversation", Tokens: 900, Estimated: true}}}
	if err = f.s.m.pg.SaveChatContext(t.Context(), f.ref, run.ID, view); err != nil {
		t.Fatal(err)
	}
	get := func() chatstate.ContextView {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		f.s.chatContextHTTP(rec, req, f.ref)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		var v chatstate.ContextView
		if err = json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	old := get()
	if old.Window != 64000 || old.MeasuredInput != 1000 {
		t.Fatal(old)
	}
	profile.ID = id
	profile.ContextWindowK = 32
	profile.MaxTokens = 2048
	if _, err = f.s.m.pg.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}
	next := get()
	if next.Window != 32000 || next.OutputReserve != 2048 || next.MeasuredInput != 0 || next.Used != 900 || next.Headroom >= old.Headroom {
		t.Fatal(next)
	}
	if f.calls.Load() != 1 {
		t.Fatal("GET context called provider")
	}
}
func TestChatContextModelIdentityIncludesBudget(t *testing.T) {
	a := agent.Config{Model: "same", MaxTokens: 4096}
	b := a
	b.MaxTokens = 2048
	if sideModel(a, 1, "").Identity == sideModel(b, 1, "").Identity {
		t.Fatal("capacity change reused stale anchor")
	}
}
