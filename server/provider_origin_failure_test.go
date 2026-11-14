package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/norma/llm"
)

func TestNativeOriginFailurePreservesSelectedFailurePolicy(t *testing.T) {
	m := backportTestManager(t)
	for _, key := range []string{"llm_judge_enabled", "llm_judge_profile_id", "llm_judge_timeout_seconds", "llm_judge_fail_action", "llm_judge_ask_timeout_seconds", "llm_judge_ask_timeout_action", "llm_judge_prompt"} {
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
	cloudID, err := m.pg.SaveProfile(&db.LLMProfile{Name: "failure-global-" + strconv.FormatInt(time.Now().UnixNano(), 10), Format: "openai", Model: "global", BaseURL: "http://127.0.0.1:1/v1", APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	previous, _ := m.pg.ActiveProfile()
	m.pg.SetActiveProfile(cloudID)
	t.Cleanup(func() {
		m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, cloudID)
		if previous != nil {
			m.pg.SetActiveProfile(previous.ID)
		}
		m.pg.DeleteProfile(cloudID)
	})
	s := &Server{m: m}
	s.wireInterceptReviewer()
	nativeCfg := agent.ConfigFrom("ollama", "native-failure", "http://127.0.0.1:2", "", "")
	nativeID := int64(87654)
	pending := []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "pending", ToolName: "Bash"}, {Type: llm.SEToolInputJSON, ToolID: "pending", Text: `{"command":"true"}`}}
	failure := errors.New("native inference failed after pending tool")
	native := &originProvider{inner: &scriptedLLMProvider{events: pending, err: failure}, cfg: nativeCfg, id: nativeID}
	assertNative := func(t *testing.T, ctx context.Context) {
		t.Helper()
		d, ok := m.interceptor.Judge(ctx, "Bash", json.RawMessage(`{"command":"true"}`))
		if !ok || d.Action != "ask" || !d.ModelFallback || d.ProfileID != nativeID || !d.TimeoutEnabled || d.TimeoutSeconds != 300 || d.TimeoutAction != "deny" {
			t.Fatalf("native failure policy lost: %+v", d)
		}
		// The config digest binds the actual native profile and 90s model deadline,
		// distinct from the 300s human approval expiry carried by Decision.
		cfg := intercept.JudgeConfig{Enabled: true, ProfileID: nativeID, Prompt: intercept.EffectiveJudgePrompt(intercept.DefaultJudgePrompt), TimeoutSeconds: 90, FailAction: "ask", AskTimeoutSeconds: 300, AskTimeoutAction: "deny"}
		raw, _ := json.Marshal(cfg)
		var canonical map[string]any
		_ = json.Unmarshal(raw, &canonical)
		raw, _ = json.Marshal(canonical)
		sum := sha256.Sum256(raw)
		if d.ConfigDigest != hex.EncodeToString(sum[:]) {
			t.Fatalf("native 90-second config lost: got%s expected%s ", d.ConfigDigest, hex.EncodeToString(sum[:]))
		}
		// A wired native reviewer would have requested unreachable /api/show; the
		// exact error here proves the origin failure short-circuited inference.
		if !strings.Contains(d.Message, failure.Error()) {
			t.Fatalf("origin failure replaced: %s", d.Message)
		}
	}
	for _, mode := range []string{"late-stream", "complete", "terminal-route", "terminal-stream-route"} {
		t.Run(mode, func(t *testing.T) {
			ctx := intercept.WithProviderOrigin(t.Context())
			if mode == "complete" {
				_, _, _, _ = native.Complete(ctx, llm.CompletionRequest{})
			} else {
				events := 0
				for ev, err := range native.Stream(ctx, llm.CompletionRequest{}) {
					if ev.Type == llm.SEToolUseStart {
						events++
					}
					if err != nil {
						break
					}
				}
				if events != 1 {
					t.Fatal("pending tool absent")
				}
			}
			if mode == "terminal-route" {
				_, _, _, _ = completeTaskLLM(ctx, "1", llm.CompletionRequest{}, taskLLMStreamHooks{current: func(context.Context) (taskLLMSelection, error) { return taskLLMSelection{}, failure }})
			}
			if mode == "terminal-stream-route" {
				for range streamTaskLLM(ctx, "1", llm.CompletionRequest{}, taskLLMStreamHooks{current: func(context.Context) (taskLLMSelection, error) { return taskLLMSelection{}, failure }}) {
				}
			}
			assertNative(t, ctx)
		})
	}
	// A real succeeding cloud pool member clears native preference and its error.
	ctx := intercept.WithProviderOrigin(t.Context())
	cloudCfg := agent.ConfigFrom("openai", "cloud", "http://127.0.0.1:3/v1", "fixture", "")
	cloud := &originProvider{inner: &fakeProvider{}, cfg: cloudCfg, id: 44}
	pool := llmpool.New([]*llmpool.Member{{ID: 1, Rank: llmpool.RankActive, Prov: &originProvider{inner: &scriptedLLMProvider{err: errors.New("HTTP 402: quota exhausted")}, cfg: nativeCfg, id: nativeID}}, {ID: 2, Prov: cloud}}, nil)
	if _, _, _, err := pool.Complete(ctx, llm.CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.interceptor.SetReviewer(func(_ context.Context, id int64, _ string, _ intercept.ReviewInput) (intercept.Decision, error) {
		calls++
		if id != cloudID {
			t.Errorf("cloud fallback profile=%d", id)
		}
		return intercept.Decision{}, errors.New("cloud failure")
	})
	d, _ := m.interceptor.Judge(ctx, "Bash", json.RawMessage(`{"command":"true"}`))
	if calls != 1 || d.Action != "allow" {
		t.Fatalf("cloud fallback retained stale native policy: %+v calls%d", d, calls)
	}
	// A failed cloud member also retains cloud semantics, while preventing review.
	cloud.inner = &scriptedLLMProvider{err: errors.New("cloud inference failed")}
	cloud.Complete(ctx, llm.CompletionRequest{})
	d, _ = m.interceptor.Judge(ctx, "Bash", json.RawMessage(`{"command":"true"}`))
	if calls != 1 || d.Action != "allow" {
		t.Fatalf("failed cloud policy changed %+v calls%d", d, calls)
	}
}
