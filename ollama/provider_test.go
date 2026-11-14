package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestNativeCompleteAndToolReplay(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "cloud-secret-must-not-leak")
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/chat" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected authorization: %s", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, body)
		fmt.Fprint(w, `{"message":{"role":"assistant","thinking":"reason","content":"answer","tool_calls":[{"function":{"name":"lookup","arguments":{"host":"example.test"}}}]},"done":true,"done_reason":"stop","prompt_eval_count":80,"prompt_eval_cached_count":30,"eval_count":12}`)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL+"/prefix/v1/chat/completions", "", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(client, "local-model", Options{ContextWindow: 16384, KeepAlive: "45m", MaxTokens: 321, ThinkingType: "enabled", JSON: true})
	temp := 0.2
	req := llm.CompletionRequest{System: []string{"system", "rules"}, Messages: []llm.Message{llm.UserText("hello")}, Tools: []llm.ToolSchema{{Name: "lookup", Description: "lookup host", InputSchema: map[string]any{"type": "object"}}}, Temperature: &temp}
	msg, stop, usage, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if stop != "tool_use" || usage.InputTokens != 80 || usage.CacheReadTokens != 30 || usage.OutputTokens != 12 || msg.Text() != "answer" {
		t.Fatalf("result=%+v %s %+v", msg, stop, usage)
	}
	calls := msg.ToolUses()
	if len(calls) != 1 || calls[0].ID == "" {
		t.Fatalf("calls=%+v", calls)
	}
	body := requests[0]
	opts := body["options"].(map[string]any)
	if opts["num_ctx"] != float64(16384) || opts["num_predict"] != float64(321) || body["keep_alive"] != "45m" || body["think"] != true || body["format"] != "json" || body["stream"] != false {
		t.Fatalf("wire=%+v", body)
	}
	req.Messages = append(req.Messages, msg, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(calls[0].ID, "found", false)}})
	req.Thinking = "disabled"
	if _, _, _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body = requests[1]
	messages := body["messages"].([]any)
	tool := messages[len(messages)-1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_name"] != "lookup" || tool["tool_call_id"] != calls[0].ID || tool["content"] != "found" || body["think"] != false {
		t.Fatalf("replay=%+v", body)
	}
}

func TestNativeNDJSONStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"thinking":"reason"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"content":"hi"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"tool_calls":[{"function":{"name":"one","arguments":{"n":1}}},{"function":{"name":"two","arguments":{}}}]},"done":false}`)
		fmt.Fprintln(w, `{"message":{},"done":true,"prompt_eval_count":50,"prompt_eval_cached_count":20,"eval_count":6}`)
	}))
	defer srv.Close()
	client, _ := NewClient(srv.URL, "token", srv.Client())
	p := NewProvider(client, "test", Options{})
	var events []llm.StreamEvent
	for event, err := range p.Stream(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}}) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	want := []llm.StreamEventType{llm.SEMessageStart, llm.SEThinkingDelta, llm.SETextDelta, llm.SEToolUseStart, llm.SEToolInputJSON, llm.SEToolUseStart, llm.SEToolInputJSON, llm.SEMessageDelta, llm.SEMessageStop}
	if len(events) != len(want) {
		t.Fatalf("events=%+v", events)
	}
	for i, typ := range want {
		if events[i].Type != typ {
			t.Fatalf("event %d=%+v", i, events[i])
		}
	}
	if events[3].ToolID == events[5].ToolID || events[7].Usage.CacheReadTokens != 20 || events[7].StopReason != "tool_use" {
		t.Fatalf("events=%+v", events)
	}
}

func TestNativeFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		stream     bool
		want       string
	}{
		{"http", `{"error":"missing"}`, 404, false, "choose the OpenAI format"},
		{"error", `{"error":"out of memory"}`, 200, false, "out of memory"},
		{"bad-json", `{broken`, 200, false, "invalid response"},
		{"incomplete", `{"message":{"content":"x"}}`, 200, false, "missing done"},
		{"stream-error", `{"error":"runner stopped"}`, 200, true, "runner stopped"},
		{"stream-truncated", `{"message":{"content":"x"}}`, 200, true, "missing done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			client, _ := NewClient(srv.URL, "", srv.Client())
			p := NewProvider(client, "x", Options{})
			var err error
			if tc.stream {
				for _, e := range p.Stream(context.Background(), llm.CompletionRequest{}) {
					if e != nil {
						err = e
					}
				}
			} else {
				_, _, _, err = p.Complete(context.Background(), llm.CompletionRequest{})
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	client, _ := NewClient(srv.URL, "", srv.Client())
	p := NewProvider(client, "x", Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := p.Complete(ctx, llm.CompletionRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestMetadataWarmAndDiagnostics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer optional-token" {
			t.Error("missing explicit key")
		}
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"local:tag","details":{"parameter_size":"14B","quantization_level":"Q4_K_M"}}]}`)
		case "/api/show":
			fmt.Fprint(w, `{"capabilities":["tools","thinking"],"model_info":{"general.architecture":"qwen2"}}`)
		case "/api/ps":
			fmt.Fprint(w, `{"models":[{"name":"local:tag","context_length":32768}]}`)
		case "/api/chat":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["keep_alive"] != "30m" || body["options"].(map[string]any)["num_ctx"] != float64(32768) {
				t.Errorf("warm/test=%+v", body)
			}
			fmt.Fprint(w, `{"done":true,"message":{"tool_calls":[{"function":{"name":"artex_ping","arguments":{"value":"OK"}}}]},"load_duration":2000000,"eval_count":12,"eval_duration":1000000000}`)
		default:
			t.Errorf("path=%s", r.URL.Path)
		}
	}))
	defer srv.Close()
	client, _ := NewClient(srv.URL+"/api", "optional-token", srv.Client())
	models, err := client.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].Details.ParameterSize != "14B" {
		t.Fatalf("models=%+v %v", models, err)
	}
	if err := client.Warm(context.Background(), "local:tag", Options{}); err != nil {
		t.Fatal(err)
	}
	diag, err := client.Test(context.Background(), "local:tag", Options{})
	if err != nil || !diag.ToolsSupported || !diag.ToolCallVerified || !diag.ContextVerified || diag.LoadDurationMS != 2 || diag.TokensPerSecond != 12 {
		t.Fatalf("diagnostics=%+v %v", diag, err)
	}
}

func TestUnsupportedContentFailsExplicitly(t *testing.T) {
	client, _ := NewClient("http://localhost:11434", "", http.DefaultClient)
	p := NewProvider(client, "test", Options{})
	_, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockType("image")}}}}})
	if err == nil || !strings.Contains(err.Error(), "unsupported message block") {
		t.Fatalf("unsupported content=%v", err)
	}
}

func TestNativeRetrySettingsAndNoReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch calls {
				case 1:
					w.WriteHeader(503)
					fmt.Fprint(w, `{"error":"busy"}`)
				case 2:
					fmt.Fprintln(w, `{"done":true,"message":{}}`)
				default:
					fmt.Fprintln(w, `{"done":true,"message":{"content":"OK"}}`)
				}
			}))
			defer srv.Close()
			client, _ := NewClient(srv.URL, "", srv.Client())
			p := NewProvider(client, "test", Options{ConnectAttempts: 1, ConnectInterval: time.Millisecond, EmptyAttempts: 1, EmptyInterval: time.Millisecond})
			if stream {
				var text string
				starts := 0
				for ev, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
					if err != nil {
						t.Fatal(err)
					}
					if ev.Type == llm.SEMessageStart {
						starts++
					}
					if ev.Type == llm.SETextDelta {
						text += ev.Text
					}
				}
				if text != "OK" || starts != 1 {
					t.Fatalf("text=%s starts=%d", text, starts)
				}
			} else {
				msg, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{})
				if err != nil || msg.Text() != "OK" {
					t.Fatalf("msg=%+v err=%v", msg, err)
				}
			}
			if calls != 3 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
	t.Run("never-replay-delivered-tool", func(t *testing.T) {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			fmt.Fprintln(w, `{"message":{"tool_calls":[{"function":{"name":"side_effect","arguments":{}}}]}}`)
			fmt.Fprintln(w, `{"error":"disconnected"}`)
		}))
		defer srv.Close()
		client, _ := NewClient(srv.URL, "", srv.Client())
		p := NewProvider(client, "test", Options{ConnectInterval: time.Millisecond, EmptyInterval: time.Millisecond})
		var failed bool
		for _, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
			failed = failed || err != nil
		}
		if !failed || calls != 1 {
			t.Fatalf("failed=%v calls=%d", failed, calls)
		}
	})
	t.Run("auth-no-retry", func(t *testing.T) {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(401) }))
		defer srv.Close()
		client, _ := NewClient(srv.URL, "", srv.Client())
		p := NewProvider(client, "test", Options{ConnectInterval: time.Millisecond})
		_, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{})
		if err == nil || calls != 1 {
			t.Fatalf("auth=%v calls=%d", err, calls)
		}
	})
	t.Run("cancel-backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503); cancel() }))
		defer srv.Close()
		client, _ := NewClient(srv.URL, "", srv.Client())
		p := NewProvider(client, "test", Options{ConnectInterval: time.Hour})
		_, _, _, err := p.Complete(ctx, llm.CompletionRequest{})
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("cancel=%v calls=%d", err, calls)
		}
	})
}

func TestDiagnosticsRejectFalseReadiness(t *testing.T) {
	for _, tc := range []struct {
		name               string
		capabilities, tool string
		context            int
		want               string
	}{
		{"no-tools", `["completion"]`, `"tool_calls":[{"function":{"name":"artex_ping","arguments":{"value":"OK"}}}]`, 32768, "does not declare tools"},
		{"no-tool-call", `["tools"]`, `"content":"OK"`, 32768, "did not execute"},
		{"context-truncated", `["tools"]`, `"tool_calls":[{"function":{"name":"artex_ping","arguments":{"value":"OK"}}}]`, 2048, "requested context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/show":
					fmt.Fprintf(w, `{"capabilities":%s}`, tc.capabilities)
				case "/api/chat":
					fmt.Fprintf(w, `{"done":true,"message":{%s}}`, tc.tool)
				case "/api/ps":
					fmt.Fprintf(w, `{"models":[{"name":"test","context_length":%d}]}`, tc.context)
				}
			}))
			defer srv.Close()
			client, _ := NewClient(srv.URL, "", srv.Client())
			_, err := client.Test(context.Background(), "test", Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("diagnostic=%v", err)
			}
		})
	}
}

func TestAutoThinkingUsesCapabilitiesAndCaches(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(fmt.Sprint(supported), func(t *testing.T) {
			shows := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/show" {
					shows++
					if supported {
						fmt.Fprint(w, `{"capabilities":["thinking"]}`)
					} else {
						fmt.Fprint(w, `{"capabilities":["tools"]}`)
					}
					return
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				think, exists := body["think"]
				if supported {
					if think != true {
						t.Errorf("thinking=%v", think)
					}
				} else if exists {
					t.Error("unsupported thinking sent")
				}
				fmt.Fprint(w, `{"done":true,"message":{"content":"OK"}}`)
			}))
			defer srv.Close()
			client, _ := NewClient(srv.URL, "", srv.Client())
			p := NewProvider(client, "test", Options{ThinkingType: "auto"})
			for range 2 {
				if _, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{}); err != nil {
					t.Fatal(err)
				}
			}
			if shows != 1 {
				t.Fatalf("show calls=%d", shows)
			}
		})
	}
}

func TestWarmRequiresDoneAndDiagnosticArgumentsExact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			fmt.Fprint(w, `{"capabilities":["tools"]}`)
		case "/api/chat":
			fmt.Fprint(w, `{"done":false,"message":{"tool_calls":[{"function":{"name":"artex_ping","arguments":{"value":"WRONG"}}}]}}`)
		case "/api/ps":
			fmt.Fprint(w, `{"models":[{"name":"test","context_length":32768}]}`)
		}
	}))
	defer srv.Close()
	client, _ := NewClient(srv.URL, "", srv.Client())
	if err := client.Warm(context.Background(), "test", Options{}); err == nil {
		t.Fatal("incomplete warm accepted")
	}
	diag, err := client.Test(context.Background(), "test", Options{})
	if err == nil || diag.ToolCallVerified {
		t.Fatalf("incorrect diagnostic args accepted: %+v %v", diag, err)
	}
}

func TestModelDiscoveryCompatibilityFallback(t *testing.T) {
	for _, status := range []int{404, 405, 401, 403, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			tagsCalls, compatibleCalls := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/tags":
					tagsCalls++
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":"unsupported"}`)
				case "/v1/models":
					compatibleCalls++
					fmt.Fprint(w, `{"data":[{"id":"local-first"},{"id":"local-second"}]}`)
				default:
					t.Errorf("unexpected path=%s", r.URL.Path)
				}
			}))
			defer srv.Close()
			client, _ := NewClient(srv.URL+"/v1", "", srv.Client())
			client.connectAttempts = -1
			models, err := client.ListModels(context.Background())
			fallback := status == 404 || status == 405
			if fallback {
				if err != nil || compatibleCalls != 1 || len(models) != 2 || models[0].Name != "local-first" || models[0].Model != "local-first" || models[1].Name != "local-second" {
					t.Fatalf("models=%+v err=%v compatibleCalls=%d", models, err, compatibleCalls)
				}
			} else if err == nil || compatibleCalls != 0 {
				t.Fatalf("unexpected fallback: models=%+v err=%v compatibleCalls=%d", models, err, compatibleCalls)
			}
			if tagsCalls != 1 {
				t.Fatalf("tagsCalls=%d", tagsCalls)
			}
		})
	}
}

func TestNativeHTTPFailureClassification(t *testing.T) {
	const model = "artex-missing:fixture"
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
		pull   bool
	}{
		{"missing-model", 404, `{"error":"model 'artex-missing:fixture' not found"}`, "ollama pull " + model, true},
		{"endpoint-json", 404, `{"error":"Not Found"}`, "choose the OpenAI format", false},
		{"endpoint-text", 404, "404 page not found", "check the base URL", false},
		{"unsupported-method", 405, `{"error":"Method not allowed"}`, "choose the OpenAI format", false},
		{"different-model", 404, `{"error":"model 'different' not found"}`, "choose the OpenAI format", false},
		{"authentication", 401, `{"error":"unauthorized"}`, "HTTP 401", false},
		{"forbidden", 403, `{"error":"forbidden"}`, "HTTP 403", false},
		{"server-error", 500, `{"error":"runner failed"}`, "HTTP 500", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			client, _ := NewClient(srv.URL, "", srv.Client())
			client.connectAttempts = -1
			diag, err := client.Test(context.Background(), model, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "ollama pull") != tc.pull {
				t.Fatalf("error=%v", err)
			}
			if diag.Model != model || diag.ToolCallVerified || diag.ContextVerified {
				t.Fatalf("partial diagnostics=%+v", diag)
			}
			var status *HTTPError
			if !errors.As(err, &status) || status.StatusCode != tc.status {
				t.Fatalf("typed status lost: %v", err)
			}
		})
	}
	t.Run("network-no-pull", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		address := srv.URL
		srv.Close()
		client, _ := NewClient(address, "", http.DefaultClient)
		client.connectAttempts = -1
		_, err := client.Test(context.Background(), model, Options{})
		if err == nil || strings.Contains(err.Error(), "ollama pull") {
			t.Fatalf("network error=%v", err)
		}
	})
}
