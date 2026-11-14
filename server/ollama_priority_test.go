package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
)

func waitGateQueue(t *testing.T, g *endpointGate, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		queued := len(g.urgent) + len(g.background)
		g.mu.Unlock()
		if queued == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("waiter did not enqueue")
}
func TestNativeUrgentFIFOAndBackgroundFIFO(t *testing.T) {
	g := &endpointGate{limit: 1}
	release, err := g.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	type admitted struct {
		name    string
		release func()
	}
	events := make(chan admitted, 6)
	for i, role := range []string{"worker", "planner", "goals", "judge", "mainagent", "sidequestion"} {
		go func(role string) {
			r, e := g.acquire(withNativeRole(t.Context(), role))
			if e == nil {
				events <- admitted{role, r}
			}
		}(role)
		waitGateQueue(t, g, i+1)
	}
	release()
	for _, want := range []string{"judge", "mainagent", "sidequestion", "worker", "planner", "goals"} {
		select {
		case got := <-events:
			if got.name != want {
				t.Errorf("next=%s want %s", got.name, want)
			}
			got.release()
			got.release()
		case <-time.After(time.Second):
			t.Fatal("admission deadlock")
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 {
		t.Fatalf("leaked %d slots", g.active)
	}
}
func TestNativeQueuedAndGrantCancellationAndLoweredCap(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		g := &endpointGate{limit: 1}
		release, _ := g.acquire(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			r, e := g.acquire(withNativeRole(ctx, "judge"))
			if e == nil {
				r()
			}
		}()
		waitGateQueue(t, g, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); release() }()
		wg.Wait()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("cancel/grant deadlock")
		}
		r, e := g.acquire(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		r()
		g.mu.Lock()
		active := g.active
		g.mu.Unlock()
		if active != 0 {
			t.Fatalf("grant cancellation leak %d", active)
		}
	}
	g := &endpointGate{limit: 2}
	r1, _ := g.acquire(t.Context())
	r2, _ := g.acquire(t.Context())
	done := make(chan func(), 1)
	go func() { r, _ := g.acquire(t.Context()); done <- r }()
	waitGateQueue(t, g, 1)
	g.mu.Lock()
	g.limit = 1
	g.dispatch()
	g.mu.Unlock()
	r1()
	select {
	case r := <-done:
		r()
		t.Fatal("lowered cap ignored")
	default:
	}
	r2()
	select {
	case r := <-done:
		r()
	case <-time.After(time.Second):
		t.Fatal("lower cap stuck")
	}
}

// A real native endpoint is shared by cached worker providers and the separate
// schema-constrained reviewer. The reviewer must mark its own request urgent.
func TestNativeHTTPJudgeTakesNextSlotBeforeThreeWorkers(t *testing.T) {
	m := backportTestManager(t)
	for _, key := range []string{"llm_judge_enabled", "llm_judge_profile_id", "llm_judge_timeout_seconds", "llm_judge_fail_action"} {
		old, found, err := m.pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.pg.Exec(`DELETE FROM settings WHERE key=$1`, key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if found {
				m.pg.SetSetting(key, old)
			} else {
				m.pg.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		})
	}
	held := make(chan struct{})
	var releaseHeld sync.Once
	t.Cleanup(func() { releaseHeld.Do(func() { close(held) }) })
	requests := make(chan string, 4)
	var mu sync.Mutex
	active, peak, warmCalls := 0, 0, 0
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/show":
			io.WriteString(w, `{"capabilities":["tools"],"model_info":{}}`)
		case "/api/chat":
			mu.Lock()
			active++
			peak = max(peak, active)
			mu.Unlock()
			defer func() { mu.Lock(); active--; mu.Unlock() }()
			var req struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
				Format  map[string]any `json:"format"`
				Options map[string]any `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if len(req.Messages) == 0 {
				mu.Lock()
				warmCalls++
				mu.Unlock()
				io.WriteString(w, `{"done":true}`)
				return
			}
			label := req.Messages[len(req.Messages)-1].Content
			text := "worker finished"
			if req.Format != nil {
				label = "judge"
				properties, _ := req.Format["properties"].(map[string]any)
				decision, _ := properties["decision"].(map[string]any)
				comment, _ := properties["comment"].(map[string]any)
				required, _ := req.Format["required"].([]any)
				choices, _ := decision["enum"].([]any)
				if req.Format["type"] != "object" || req.Format["additionalProperties"] != false || decision["type"] != "string" || comment["type"] != "string" || comment["minLength"] != float64(10) || len(required) != 2 || required[0] != "decision" || required[1] != "comment" || len(choices) != 3 || choices[0] != "allow" || choices[1] != "ask" || choices[2] != "deny" {
					t.Errorf("invalid native judge schema: %+v", req.Format)
				}
				text = `{"decision":"allow","comment":"Operation: read a file; Effect if successful: return content; Rule: A5"}`
			}
			if req.Options["num_ctx"] != float64(32768) {
				t.Errorf("native context changed: %+v", req.Options)
			}
			requests <- label
			if label == "worker-1" {
				select {
				case <-held:
				case <-r.Context().Done():
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": text}, "done": true})
		default:
			t.Errorf("unexpected native route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	previous, err := m.pg.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "priority-http-" + strconv.FormatInt(time.Now().UnixNano(), 10), Format: "ollama", Model: "priority-fixture", BaseURL: fixture.URL, Streaming: true, ContextWindowK: 32, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
		if previous != nil {
			m.pg.SetActiveProfile(previous.ID)
		}
		m.pg.DeleteProfile(id)
	})
	if err := m.pg.SetActiveProfile(id); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetBool("llm_judge_enabled", true); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m, provByProfile: map[int64]*provEntry{}}
	s.wireInterceptReviewer()
	provider, cfg, ok := s.providerForProfile(id)
	if !ok {
		t.Fatal("native profile provider unavailable")
	}
	if _, ok := provider.(*nativeAdmissionProvider); !ok {
		t.Fatalf("worker bypassed native admission: %T", provider)
	}
	gate := s.nativeGate(cfg)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	workers := make(chan error, 3)
	startWorker := func(label string) {
		go func() {
			_, _, _, err := provider.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText(label)}})
			workers <- err
		}()
	}
	startWorker("worker-1")
	select {
	case first := <-requests:
		if first != "worker-1" {
			t.Fatalf("first request=%s", first)
		}
	case <-ctx.Done():
		t.Fatal("first worker never reached native HTTP")
	}
	startWorker("worker-2")
	waitGateQueue(t, gate, 1)
	startWorker("worker-3")
	waitGateQueue(t, gate, 2)
	type reviewResult struct {
		decision intercept.Decision
		enabled  bool
	}
	judge := make(chan reviewResult, 1)
	// No urgency marker here: the actual reviewer must add it itself.
	go func() {
		d, ok := m.interceptor.Judge(ctx, "Bash", json.RawMessage(`{"command":"cat fixture.txt"}`))
		judge <- reviewResult{d, ok}
	}()
	waitGateQueue(t, gate, 3)
	gate.mu.Lock()
	urgent, background := len(gate.urgent), len(gate.background)
	gate.mu.Unlock()
	if urgent != 1 || background != 2 {
		t.Fatalf("wired reviewer lanes urgent=%d background=%d", urgent, background)
	}
	releaseHeld.Do(func() { close(held) })
	for _, want := range []string{"judge", "worker-2", "worker-3"} {
		select {
		case got := <-requests:
			if got != want {
				t.Errorf("next HTTP request=%s want %s", got, want)
			}
		case <-ctx.Done():
			t.Fatal("native HTTP request stalled")
		}
	}
	for range 3 {
		select {
		case err := <-workers:
			if err != nil {
				t.Errorf("worker inference: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("worker did not finish")
		}
	}
	select {
	case result := <-judge:
		if !result.enabled || result.decision.Action != "allow" || result.decision.ModelFallback || result.decision.ProfileID != id {
			t.Fatalf("judge did not return native verdict: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("judge timed out")
	}
	gate.mu.Lock()
	remaining, queued := gate.active, len(gate.urgent)+len(gate.background)
	gate.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if peak != 1 || active != 0 || warmCalls != 1 || remaining != 0 || queued != 0 {
		t.Fatalf("native endpoint peak=%d active=%d warm=%d gate active=%d queued=%d", peak, active, warmCalls, remaining, queued)
	}
	t.Log("native HTTP order: worker-1, judge, worker-2, worker-3; high-water=1; all complete; gate empty")
}
