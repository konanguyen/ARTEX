package server

import (
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func TestNativeJudgeMismatchUsesModelAliasesAndContext(t *testing.T) {
	m := backportTestManager(t)
	s := &Server{m: m}
	old, found, _ := m.pg.GetSetting("llm_judge_profile_id")
	t.Cleanup(func() {
		if found {
			m.pg.SetSetting("llm_judge_profile_id", old)
		} else {
			m.pg.Exec(`DELETE FROM settings WHERE key='llm_judge_profile_id'`)
		}
	})
	profile := &db.LLMProfile{Name: "mismatch-" + strconv.FormatInt(time.Now().UnixNano(), 10), Format: "ollama", Model: "foo:latest", BaseURL: "http://127.0.0.1:11434/v1", ContextWindowK: 32}
	id, err := m.pg.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.ID = id
	t.Cleanup(func() { m.pg.DeleteProfile(id) })
	m.pg.SetSetting("llm_judge_profile_id", strconv.FormatInt(id, 10))
	tested := agent.ConfigFrom("ollama", "foo", "http://127.0.0.1:11434", "", "")
	tested.ContextWindowK = 32
	for _, tc := range []struct {
		model    string
		window   int
		endpoint string
		want     bool
	}{{"foo", 32, tested.BaseURL, false}, {"foo:latest", 32, tested.BaseURL, false}, {"foo:latest", 16, tested.BaseURL, true}, {"different", 32, tested.BaseURL, true}, {"different", 32, "http://127.0.0.1:11435", false}} {
		tested.Model, tested.ContextWindowK, tested.BaseURL = tc.model, tc.window, tc.endpoint
		if got := s.nativeJudgeMismatch(tested); got != tc.want {
			t.Fatalf("model=%s window=%d endpoint=%s mismatch=%v want%v", tc.model, tc.window, tc.endpoint, got, tc.want)
		}
	}
	m.pg.SetSetting("llm_judge_profile_id", "0")
	if s.nativeJudgeMismatch(tested) {
		t.Fatal("default judge reported distinct explicit model")
	}
}
