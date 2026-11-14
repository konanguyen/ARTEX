package server

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/norma/llm"
)

func TestNativeJudgeActualConcreteOriginAndIsolation(t *testing.T) {
	m := backportTestManager(t)
	for _, key := range []string{"llm_judge_enabled", "llm_judge_profile_id", "llm_judge_timeout_seconds", "llm_judge_fail_action"} {
		old, found, _ := m.pg.GetSetting(key)
		m.pg.Exec(`DELETE FROM settings WHERE key=$1`, key)
		t.Cleanup(func() {
			if found {
				m.pg.SetSetting(key, old)
			} else {
				m.pg.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		})
	}
	m.pg.SetBool("llm_judge_enabled", true)
	globalID, err := m.pg.SaveProfile(&db.LLMProfile{Name: "origin-global-" + strconv.FormatInt(time.Now().UnixNano(), 10), Format: "openai", BaseURL: "http://127.0.0.1:1/v1", Model: "global", APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	previous, _ := m.pg.ActiveProfile()
	m.pg.SetActiveProfile(globalID)
	t.Cleanup(func() {
		m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, globalID)
		if previous != nil {
			m.pg.SetActiveProfile(previous.ID)
		}
		m.pg.DeleteProfile(globalID)
	})
	native := agent.ConfigFrom("ollama", "actual-bound-model", "http://127.0.0.1:2", "", "")
	native.ContextWindowK = 16
	cloud := agent.ConfigFrom("openai", "cloud-failover", "http://127.0.0.1:3/v1", "fixture", "")
	nativeProvider := &originProvider{inner: &fakeProvider{}, cfg: native, id: 0}
	cloudProvider := &originProvider{inner: &fakeProvider{}, cfg: cloud, id: 44}
	var gotID int64
	var gotNative bool
	var timeout time.Duration
	m.interceptor.SetReviewer(func(ctx context.Context, id int64, _ string, _ intercept.ReviewInput) (intercept.Decision, error) {
		gotID = id
		origin, ok := intercept.ReviewProviderOrigin(ctx)
		gotNative = ok
		if ok {
			cfg := origin.Data.(agent.Config)
			if cfg.Model != native.Model || cfg.CompactionWindow() != 16384 {
				t.Errorf("wrong captured concrete cfg %+v", cfg)
			}
		}
		deadline, _ := ctx.Deadline()
		timeout = time.Until(deadline)
		return intercept.Decision{Action: "allow"}, nil
	})
	review := func(ctx context.Context, wantID int64, wantNative bool) {
		t.Helper()
		_, ok := m.interceptor.Judge(ctx, "Bash", json.RawMessage(`{"command":"true"}`))
		if !ok || gotID != wantID || gotNative != wantNative {
			t.Fatalf("review id=%d native=%v want %d/%v", gotID, gotNative, wantID, wantNative)
		}
		want := 15 * time.Second
		if wantNative {
			want = 90 * time.Second
		}
		if timeout < want-time.Second || timeout > want {
			t.Fatalf("wrong pre-resolved deadline %v", timeout)
		}
	}
	first := intercept.WithProviderOrigin(t.Context())
	other := intercept.WithProviderOrigin(first)
	nativeProvider.Complete(first, llm.CompletionRequest{})
	review(first, 0, true)
	review(other, globalID, false)
	// A cloud switch clears the previous native preference in the same run.
	cloudProvider.Complete(first, llm.CompletionRequest{})
	review(first, globalID, false)
	nativeProvider.Complete(first, llm.CompletionRequest{})
	m.pg.SetSetting("llm_judge_profile_id", strconv.FormatInt(globalID, 10))
	review(first, globalID, false)
	m.pg.SetSetting("llm_judge_profile_id", "0")
	// Concrete members set identity even when a pool hides the routing choice.
	pool := llmpool.New([]*llmpool.Member{{ID: 1, Prov: nativeProvider, Rank: llmpool.RankActive}}, nil)
	pool.Complete(first, llm.CompletionRequest{})
	review(first, 0, true)
	// Failover origin follows the actual succeeding member, not the pool head.
	failingNative := &originProvider{inner: &scriptedLLMProvider{err: errors.New("HTTP 402: quota exhausted")}, cfg: native, id: 11}
	for _, tc := range []struct {
		next   llm.Provider
		native bool
	}{{nativeProvider, true}, {cloudProvider, false}} {
		routed := llmpool.New([]*llmpool.Member{{ID: 11, Prov: failingNative, Rank: llmpool.RankActive}, {ID: 12, Prov: tc.next}}, nil)
		if _, _, _, err := routed.Complete(first, llm.CompletionRequest{}); err != nil {
			t.Fatal(err)
		}
		wantID := globalID
		if tc.native {
			wantID = 0
		}
		review(first, wantID, tc.native)
	}

	// Exhausted/unavailable routing is an explicit review failure, not global fallback.
	intercept.SetProviderOrigin(first, intercept.ProviderOrigin{Err: errors.New("selected route exhausted")})
	calls := 0
	m.interceptor.SetReviewer(func(context.Context, int64, string, intercept.ReviewInput) (intercept.Decision, error) {
		calls++
		return intercept.Decision{Action: "allow"}, nil
	})
	m.pg.SetSetting("llm_judge_fail_action", "deny")
	d, _ := m.interceptor.Judge(first, "Bash", json.RawMessage(`{"command":"true"}`))
	if calls != 0 || d.Action != "deny" {
		t.Fatalf("route failure silently fell back: %+v calls=%d", d, calls)
	}
}
