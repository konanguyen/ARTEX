package server

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func backportTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func nativeFixture(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("unexpected inherited auth=%s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"test:latest","details":{"parameter_size":"4B","quantization_level":"Q4_K_M"}}]}`))
		case "/api/show":
			w.Write([]byte(`{"capabilities":["tools","thinking"],"details":{"parameter_size":"4B","quantization_level":"Q4_K_M"}}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"test:latest","context_length":32768}]}`))
		case "/api/chat":
			w.Write([]byte(`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"scopeweaver_ping","arguments":{"value":"OK"}}}]},"done":true,"load_duration":250000000,"eval_count":10,"eval_duration":500000000}`))
		default:
			t.Errorf("unexpected path=%s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}
func TestOllamaDiscoveryAndDiagnosticsContracts(t *testing.T) {
	ts := nativeFixture(t)
	s := &Server{}
	body := map[string]any{"provider": "ollama", "base_url": ts.URL + "/v1", "model": "test:latest", "context_window_k": 32}
	call := func(fn http.HandlerFunc) map[string]any {
		raw, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		fn(w, httptest.NewRequest("POST", "/", bytes.NewReader(raw)))
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["ok"] != true {
			t.Fatalf("response=%s", w.Body)
		}
		return got
	}
	got := call(s.pgListModels)
	if got["models"].([]any)[0] != "test:latest" || len(got["model_details"].([]any)) != 1 {
		t.Fatalf("discovery=%v", got)
	}
	got = call(s.testLLM)
	if got["load_duration_ms"] != 250.0 || got["tokens_per_second"] != 20.0 || got["context_length"] != 32768.0 || got["tool_call_verified"] != true {
		t.Fatalf("diagnostics=%v", got)
	}
}
func TestOllamaSharedEndpointAdmission(t *testing.T) {
	ts := nativeFixture(t)
	s := &Server{}
	fake := &fakeProvider{hold: 30 * time.Millisecond}
	cfg := agent.ConfigFrom("ollama", "test:latest", ts.URL, "", "")
	p1 := s.profileConcurrencyProvider(fake, cfg)
	cfg.BaseURL = ts.URL + "/v1"
	p2 := s.profileConcurrencyProvider(fake, cfg)
	var wg sync.WaitGroup
	for _, p := range []llm.Provider{p1, p2, p1, p2} {
		wg.Add(1)
		go func(p llm.Provider) {
			defer wg.Done()
			p.Complete(context.Background(), llm.CompletionRequest{})
		}(p)
	}
	wg.Wait()
	if fake.Peak() != 1 {
		t.Fatalf("endpoint inference peak=%d", fake.Peak())
	}
}
func TestOllamaKeylessProfileAvailabilityAndPolicy(t *testing.T) {
	m := backportTestManager(t)
	s := &Server{m: m, provByProfile: map[int64]*provEntry{}}
	ts := nativeFixture(t)
	p := &db.LLMProfile{Name: "t-keyless-native-server", Format: "ollama", Model: "test:latest", BaseURL: ts.URL, ContextWindowK: 32, Streaming: true}
	id, err := m.pg.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id)
	cfg, ok := s.loadProfileConfig(id)
	if !ok || cfg.APIKey != "" || !cfg.Ready() {
		t.Fatalf("native ready=%v cfg=%+v", ok, cfg)
	}
	p.ID = id
	if !s.resolutionFromProfile(p, "task_chain").Available {
		t.Fatal("keyless task profile unavailable")
	}
	client, err := cfg.HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	if err = m.pg.SetBool(settingLocalLLMOnly, true); err != nil {
		t.Fatal(err)
	}
	defer m.pg.SetBool(settingLocalLLMOnly, false)
	if strict, _ := cfg.PolicyResolver(); !strict {
		t.Fatal("cached config policy did not reflect saved toggle")
	}
	// Reuse the already-created client after the policy toggle.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://192.0.2.1/models", nil)
	if _, err = client.Do(req); err == nil {
		t.Fatal("strict cached client reached public IP")
	}
}

func TestOllamaKeylessActivationTaskBindingFailoverAndWarm(t *testing.T) {
	m := backportTestManager(t)
	var warms atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/show" {
			w.Write([]byte(`{"capabilities":["tools"]}`))
			return
		}
		if r.URL.Path == "/api/chat" {
			warms.Add(1)
			w.Write([]byte(`{"done":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	native, err := m.pg.SaveProfile(&db.LLMProfile{Name: "t-native-task-chain", Format: "ollama", Model: "test:latest", BaseURL: ts.URL, Streaming: true, ContextWindowK: 32})
	if err != nil {
		t.Fatal(err)
	}
	cloud, err := m.pg.SaveProfile(&db.LLMProfile{Name: "t-cloud-task-chain", Format: "openai", Model: "cloud", APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.pg.Exec(`DELETE FROM llm_profiles WHERE id=ANY($1)`, []int64{native, cloud})
	if err = m.pg.SetActiveProfile(native); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m, provByProfile: map[int64]*provEntry{}}
	if cfg, ok := s.loadLLMConfig(); !ok || cfg.APIKey != "" {
		t.Fatalf("active native config unavailable: %v", ok)
	}
	task, err := m.CreateTaskWithOptions("native task", "verify routing", db.TaskCreateOptions{LLMProfileIDs: []int64{cloud, native}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	if err = m.pg.SetAgentLLMProfile("worker", &native); err != nil {
		t.Fatal(err)
	}
	defer m.pg.SetAgentLLMProfile("worker", nil)
	got, err := s.resolveTaskRoleLLM(task, "worker")
	if err != nil || !got.Available || got.Source != "agent_binding" {
		t.Fatalf("binding=%+v err=%v", got, err)
	}
	taskID, _ := parseTaskID(task.ID)
	if _, err = m.pg.MarkTaskLLMProfileQuotaExhausted(taskID, cloud, "insufficient_quota"); err != nil {
		t.Fatal(err)
	}
	rt := &taskLLMRuntime{s: s, taskID: task.ID, agentKey: "planner"}
	sel, err := rt.current()
	if err != nil || sel.profileID != native {
		t.Fatalf("failover=%+v err=%v", sel, err)
	}
	if !s.prepareTaskOllama(context.Background(), task) {
		t.Fatal("task warm failed")
	}
	if warms.Load() != 1 {
		t.Fatalf("duplicate per-role warms=%d", warms.Load())
	}
	if !s.prepareTaskOllama(context.Background(), task) {
		t.Fatal("resume warm failed")
	}
	if warms.Load() != 2 {
		t.Fatalf("resume did not refresh warm=%d", warms.Load())
	}
}

type diagnosticAdmissionProbe struct {
	started, release chan struct{}
	active           *atomic.Int32
}

func (p *diagnosticAdmissionProbe) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.active.Add(1)
	close(p.started)
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	p.active.Add(-1)
	return llm.Message{}, "stop", llm.Usage{}, nil
}
func (p *diagnosticAdmissionProbe) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		_, _, _, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
		}
	}
}
func TestOllamaDiagnosticSharesWorkerAdmission(t *testing.T) {
	var active, peak atomic.Int32
	diagEntered := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/show":
			w.Write([]byte(`{"capabilities":["tools"]}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"local","context_length":32768}]}`))
		case "/api/chat":
			var body struct {
				Messages []json.RawMessage `json:"messages"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Messages) == 0 {
				w.Write([]byte(`{"done":true}`))
				return
			}
			n := active.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			diagEntered <- struct{}{}
			w.Write([]byte(`{"done":true,"message":{"tool_calls":[{"function":{"name":"scopeweaver_ping","arguments":{"value":"OK"}}}]}}`))
			active.Add(-1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	s := &Server{}
	cfg := agent.ConfigFrom("ollama", "local", ts.URL, "", "")
	probe := &diagnosticAdmissionProbe{started: make(chan struct{}), release: make(chan struct{}), active: &active}
	worker := s.profileConcurrencyProvider(probe, cfg)
	workerDone := make(chan struct{})
	go func() { worker.Complete(context.Background(), llm.CompletionRequest{}); close(workerDone) }()
	select {
	case <-probe.started:
	case <-time.After(time.Second):
		t.Fatal("worker not admitted")
	}
	diagDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.testOllama(w, httptest.NewRequest("POST", "/", nil), cfg)
		diagDone <- w
	}()
	select {
	case <-diagEntered:
		t.Error("diagnostic inference bypassed busy worker gate")
	case <-time.After(30 * time.Millisecond):
	}
	close(probe.release)
	<-workerDone
	select {
	case w := <-diagDone:
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		if out["ok"] != true {
			t.Fatalf("diagnostic=%s", w.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("diagnostic did not acquire released slot")
	}
	if peak.Load() != 1 {
		t.Fatalf("worker and diagnostic inference peak=%d", peak.Load())
	}
}

func TestOllamaSettingsDoNotPersistEnvironmentCIDRs(t *testing.T) {
	m := backportTestManager(t)
	previous, existed, err := m.pg.GetSetting(settingLocalLLMCIDRs)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if existed {
			m.pg.SetSetting(settingLocalLLMCIDRs, previous)
		} else {
			m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingLocalLLMCIDRs)
		}
	}()
	saved := []string{"203.0.113.0/24"}
	s := &Server{m: m}
	if err = s.saveLLMPolicy(nil, &saved); err != nil {
		t.Fatal(err)
	}
	const envCIDR = "198.51.100.128/25"
	t.Setenv("SCOPEWEAVER_LOCAL_LLM_ALLOWED_CIDRS", envCIDR)
	t.Setenv("SCOPEWEAVER_LOCAL_LLM_ONLY", "true")
	w := httptest.NewRecorder()
	s.getSettings(w, httptest.NewRequest("GET", "/api/settings", nil))
	var settings struct {
		LocalOnly bool     `json:"local_llm_only"`
		Saved     []string `json:"local_llm_allowed_cidrs"`
		Effective []string `json:"local_llm_effective_allowed_cidrs"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.LocalOnly {
		t.Fatal("environment strict floor lost")
	}
	if !slices.Equal(settings.Saved, saved) || !slices.Contains(settings.Effective, envCIDR) || !slices.Contains(settings.Effective, saved[0]) {
		t.Fatalf("settings=%+v", settings)
	}
	// Resubmit the editable GET value exactly as the settings form does.
	raw, _ := json.Marshal(map[string]any{"local_llm_allowed_cidrs": settings.Saved})
	w = httptest.NewRecorder()
	s.putSettings(w, httptest.NewRequest("PUT", "/api/settings", bytes.NewReader(raw)))
	if w.Code != 200 {
		t.Fatalf("save=%d %s", w.Code, w.Body)
	}
	if err = os.Unsetenv("SCOPEWEAVER_LOCAL_LLM_ALLOWED_CIDRS"); err != nil {
		t.Fatal(err)
	}
	_, effective := s.effectiveLLMPolicy()
	if slices.Contains(effective, envCIDR) || !slices.Equal(s.savedLLMCIDRs(), saved) {
		t.Fatalf("environment range persisted: saved=%v effective=%v", s.savedLLMCIDRs(), effective)
	}
}

func TestDeepSeekSearchProbeRespectsLiveLocalOnlyPolicy(t *testing.T) {
	m := backportTestManager(t)
	// No active profile means off-mode validation fails before making an HTTP request.
	previous, err := m.pg.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE is_default`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if previous != nil {
			m.pg.SetActiveProfile(previous.ID)
		}
	}()
	oldStrict, existed, err := m.pg.GetSetting(settingLocalLLMOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if existed {
			m.pg.SetSetting(settingLocalLLMOnly, oldStrict)
		} else {
			m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingLocalLLMOnly)
		}
	}()
	t.Setenv("SCOPEWEAVER_LOCAL_LLM_ONLY", "false")
	s := &Server{m: m}
	call := func(backend string) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		body, _ := json.Marshal(map[string]string{"web_search_backend": backend})
		s.testWebSearch(w, httptest.NewRequest("POST", "/api/settings/test-search", bytes.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("status=%d %s", w.Code, w.Body)
		}
		var out map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err = m.pg.SetBool(settingLocalLLMOnly, false); err != nil {
		t.Fatal(err)
	}
	if strict, _ := s.effectiveLLMPolicy(); strict {
		t.Fatal("strict policy active when disabled")
	}
	off := call("deepseek")
	if off["ok"] != false || !strings.Contains(off["error"].(string), "requires a DeepSeek API key") {
		t.Fatalf("off-mode SDK validation changed: %v", off)
	}
	if err = m.pg.SetBool(settingLocalLLMOnly, true); err != nil {
		t.Fatal(err)
	}
	if strict, _ := s.effectiveLLMPolicy(); !strict {
		t.Fatal("strict toggle not reflected")
	}
	for _, backend := range []string{"deepseek", "DEEPSEEK", " deepseek ", "ddgs", "brave-free", "tavily"} {
		blocked := call(backend)
		if blocked["ok"] != false || blocked["error"] != agent.DeepSeekLocalOnlyMessage {
			t.Fatalf("strict probe not blocked before SDK credentials: %v", blocked)
		}
	}
}
