package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestNativeToolArgumentShapes(t *testing.T) {
	for _, tc := range []struct {
		name, args, want string
		invalid          bool
	}{
		{"object", `{"n":1}`, `{"n":1}`, false},
		{"encoded-object", `"{\"n\":1}"`, `{"n":1}`, false},
		{"empty", ``, `{}`, false},
		{"array", `[]`, ``, true},
		{"null", `null`, ``, true},
		{"number", `42`, ``, true},
		{"encoded-array", `"[]"`, ``, true},
		{"encoded-malformed", `"{broken"`, ``, true},
		{"malformed", `{broken`, ``, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := NewClient("http://localhost:11434", "", http.DefaultClient)
			p := NewProvider(client, "test", Options{})
			req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, Name: "lookup", Input: json.RawMessage(tc.args)}}}}}
			wire, err := p.wire(context.Background(), req, false)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "invalid arguments for tool lookup") {
					t.Fatalf("outbound accepted %s: err=%v", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := string(wire.Messages[0].ToolCalls[0].Function.Arguments); got != tc.want {
				t.Fatalf("outbound=%s want=%s", got, tc.want)
			}
		})
	}
	for _, stream := range []bool{false, true} {
		for _, args := range []string{`{"n":1}`, `"{\"n\":1}"`, `[]`, `null`, `"{broken"`} {
			t.Run(fmt.Sprintf("inbound/%t/%s", stream, args), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, `{"done":true,"message":{"tool_calls":[{"function":{"name":"lookup","arguments":%s}}]}}`, args)
				}))
				defer srv.Close()
				client, _ := NewClient(srv.URL, "", srv.Client())
				p := NewProvider(client, "test", Options{})
				valid := args == `{"n":1}` || args == `"{\"n\":1}"`
				var got string
				var err error
				if stream {
					for ev, e := range p.Stream(context.Background(), llm.CompletionRequest{}) {
						if e != nil {
							err = e
						}
						if ev.Type == llm.SEToolInputJSON {
							got = ev.Text
						}
					}
				} else {
					msg, _, _, e := p.Complete(context.Background(), llm.CompletionRequest{})
					err = e
					if calls := msg.ToolUses(); len(calls) > 0 {
						got = string(calls[0].Input)
					}
				}
				if !valid {
					if err == nil || !strings.Contains(err.Error(), "invalid arguments for tool lookup") {
						t.Fatalf("inbound accepted %s: got=%s err=%v", args, got, err)
					}
					return
				}
				if err != nil || got != `{"n":1}` {
					t.Fatalf("got=%s err=%v", got, err)
				}
			})
		}
	}
}

func TestNativeSchemaAndJSONFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"schema", Options{JSON: true, Schema: map[string]any{"type": "object", "required": []string{"decision", "comment"}}}, `{"required":["decision","comment"],"type":"object"}`},
		{"json", Options{JSON: true}, `"json"`},
		{"ordinary", Options{}, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Format json.RawMessage `json:"format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if string(req.Format) != tc.want {
					t.Errorf("format=%s want=%s", req.Format, tc.want)
				}
				fmt.Fprintln(w, `{"done":true,"message":{"content":"OK"}}`)
			}))
			defer srv.Close()
			client, _ := NewClient(srv.URL, "", srv.Client())
			p := NewProvider(client, "test", tc.opts)
			if _, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{}); err != nil {
				t.Fatal(err)
			}
			for _, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestNativeLargeToolArguments(t *testing.T) {
	for _, size := range []int{(64 << 10) + 1, (4 << 20) + 1} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("bytes-%d/stream-%t", size, stream), func(t *testing.T) {
				args, _ := json.Marshal(map[string]string{"payload": strings.Repeat("x", size)})
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, `{"done":true,"message":{"tool_calls":[{"function":{"name":"large","arguments":%s}}]},"prompt_eval_count":90,"prompt_eval_cached_count":40,"eval_count":12}`, args)
				}))
				defer srv.Close()
				client, _ := NewClient(srv.URL, "", srv.Client())
				p := NewProvider(client, "test", Options{})
				var got string
				var id string
				var cached int
				if stream {
					for ev, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
						if err != nil {
							t.Fatal(err)
						}
						switch ev.Type {
						case llm.SEToolInputJSON:
							got = ev.Text
						case llm.SEToolUseStart:
							id = ev.ToolID
						case llm.SEMessageDelta:
							cached = ev.Usage.CacheReadTokens
						}
					}
				} else {
					msg, _, usage, err := p.Complete(context.Background(), llm.CompletionRequest{})
					if err != nil {
						t.Fatal(err)
					}
					calls := msg.ToolUses()
					if len(calls) != 1 {
						t.Fatalf("calls=%+v", calls)
					}
					got = string(calls[0].Input)
					id = calls[0].ID
					cached = usage.CacheReadTokens
				}
				if got != string(args) || id == "" || cached != 40 {
					t.Fatalf("length=%d want=%d id=%s cached=%d", len(got), len(args), id, cached)
				}
			})
		}
	}
}

func TestNativeTextNeverBecomesToolCall(t *testing.T) {
	text := `{"name":"side_effect","arguments":{"execute":true}}`
	quoted, _ := json.Marshal(text)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"done":true,"message":{"content":%s}}`, quoted)
	}))
	defer srv.Close()
	client, _ := NewClient(srv.URL, "", srv.Client())
	p := NewProvider(client, "test", Options{})
	msg, stop, _, err := p.Complete(context.Background(), llm.CompletionRequest{})
	if err != nil || len(msg.ToolUses()) != 0 || msg.Text() != text || stop != "end_turn" {
		t.Fatalf("msg=%+v stop=%s err=%v", msg, stop, err)
	}
	var got string
	for ev, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == llm.SEToolUseStart {
			t.Fatal("text promoted to tool")
		}
		if ev.Type == llm.SETextDelta {
			got += ev.Text
		}
	}
	if got != text {
		t.Fatalf("text=%s", got)
	}
}

func TestNativeStreamOOMAfterContent(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintln(w, `{"message":{"content":"partial"}}`)
		fmt.Fprintln(w, `{"error":"runner out of memory"}`)
	}))
	defer srv.Close()
	client, _ := NewClient(srv.URL, "", srv.Client())
	p := NewProvider(client, "test", Options{})
	var failure error
	var text string
	var stopped bool
	for ev, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
		if err != nil {
			failure = err
		}
		if ev.Type == llm.SETextDelta {
			text += ev.Text
		}
		stopped = stopped || ev.Type == llm.SEMessageStop
	}
	if failure == nil || !strings.Contains(failure.Error(), "out of memory") || text != "partial" || stopped || calls != 1 {
		t.Fatalf("failure=%v text=%s stop=%t calls=%d", failure, text, stopped, calls)
	}
}

func TestDiagnosticsMeasuredOffloadAndAliases(t *testing.T) {
	for _, tc := range []struct {
		name, selected string
		percent        *float64
		size, vram     int64
	}{
		{"gpu", `{"name":"local:latest","model":"selected-alias","context_length":32768,"size":1000,"size_vram":1000}`, floatPointer(100), 1000, 1000},
		{"cpu", `{"name":"local:latest","model":"selected-alias","context_length":32768,"size":1000,"size_vram":0}`, floatPointer(0), 1000, 0},
		{"partial", `{"name":"selected-alias","model":"local:latest","context_length":32768,"size":1000,"size_vram":250}`, floatPointer(25), 1000, 250},
		{"clamp", `{"name":"local:latest","model":"selected-alias","context_length":32768,"size":1000,"size_vram":1500}`, floatPointer(100), 1000, 1500},
		{"zero-total", `{"name":"local:latest","model":"selected-alias","context_length":32768,"size":0,"size_vram":200}`, nil, 0, 200},
		{"missing-total", `{"name":"local:latest","model":"selected-alias","context_length":32768,"size_vram":200}`, nil, 0, 200},
		{"missing-vram", `{"name":"local:latest","model":"selected-alias","context_length":32768,"size":1000}`, nil, 1000, 0},
		{"negative-vram", `{"name":"local:latest","model":"selected-alias","context_length":32768,"size":1000,"size_vram":-1}`, nil, 1000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			methods := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				methods[r.Method+" "+r.URL.Path]++
				switch r.URL.Path {
				case "/api/show":
					fmt.Fprint(w, `{"capabilities":["tools"]}`)
				case "/api/chat":
					fmt.Fprint(w, `{"done":true,"message":{"tool_calls":[{"function":{"name":"scopeweaver_ping","arguments":"{\"value\":\"OK\"}"}}]},"load_duration":2000000,"eval_count":12,"eval_duration":1000000000}`)
				case "/api/ps":
					fmt.Fprintf(w, `{"models":[%s,{"name":"alias","model":"local","context_length":32768},{"name":"selected-alias:latest"},{"name":"other:latest"},{"model":"other"},{"name":"second:tag"}]}`, tc.selected)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			client, _ := NewClient(srv.URL, "", srv.Client())
			diag, err := client.Test(context.Background(), "local", Options{})
			if err != nil {
				t.Fatal(err)
			}
			// The extra alias entry must not override measured selected-model metadata.
			if diag.SizeBytes != tc.size || diag.SizeVRAMBytes != tc.vram {
				t.Fatalf("size=%d vram=%d diag=%+v", diag.SizeBytes, diag.SizeVRAMBytes, diag)
			}
			if (diag.GPUOffloadPercent == nil) != (tc.percent == nil) || (tc.percent != nil && *diag.GPUOffloadPercent != *tc.percent) {
				t.Fatalf("offload=%v want=%v", diag.GPUOffloadPercent, tc.percent)
			}
			if strings.Join(diag.OtherLoadedModels, ",") != "other:latest,second:tag" {
				t.Fatalf("other=%v", diag.OtherLoadedModels)
			}
			raw, _ := json.Marshal(diag)
			var fields map[string]json.RawMessage
			json.Unmarshal(raw, &fields)
			if _, present := fields["gpu_offload_percent"]; present != (tc.percent != nil) {
				t.Fatalf("optional percentage=%s", raw)
			}
			if len(methods) != 3 || methods["POST /api/show"] != 1 || methods["POST /api/chat"] != 1 || methods["GET /api/ps"] != 1 {
				t.Fatalf("methods=%v", methods)
			}
			if !diag.ContextVerified || !diag.ToolCallVerified || diag.LoadDurationMS != 2 || diag.TokensPerSecond != 12 {
				t.Fatalf("existing diagnostics lost: %+v", diag)
			}
		})
	}
}
func floatPointer(n float64) *float64 { return &n }

func TestNativeNearContextLimitLog(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, count := range []int{0, 32703, 32704, 32768, 32800} {
			t.Run(fmt.Sprintf("stream-%t/prompt-%d", stream, count), func(t *testing.T) {
				var captured bytes.Buffer
				originalOutput := log.Writer()
				log.SetOutput(&captured)
				defer log.SetOutput(originalOutput)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, `{"done":true,"message":{"content":"OK"},"prompt_eval_count":%d,"prompt_eval_cached_count":25,"eval_count":4}`, count)
				}))
				defer srv.Close()
				client, _ := NewClient(srv.URL, "", srv.Client())
				p := NewProvider(client, "fixture", Options{})
				if stream {
					for _, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
						if err != nil {
							t.Fatal(err)
						}
					}
				} else {
					if _, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{}); err != nil {
						t.Fatal(err)
					}
				}
				got := captured.String()
				want := count >= 32704
				if (got != "") != want {
					t.Fatalf("log=%q wantWarning=%t", got, want)
				}
				if want && (!strings.Contains(got, fmt.Sprintf("prompt_eval_count %d, num_ctx 32768", count)) || !strings.Contains(got, "does not prove truncation") || strings.Count(got, "near the configured context limit") != 1) {
					t.Fatalf("log=%q", got)
				}
			})
		}
	}
}

func TestDiagnosticToolProbeDisablesThinking(t *testing.T) {
	for _, thinking := range []string{"", "enabled", "auto", "low", "disabled"} {
		t.Run(thinking, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/show":
					// Tool-capable models need not advertise thinking support.
					fmt.Fprint(w, `{"capabilities":["tools"]}`)
				case "/api/chat":
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if think, present := request["think"]; !present || think != false {
						t.Errorf("diagnostic think=%v present=%t; want explicit false", think, present)
					}
					if request["options"].(map[string]any)["num_predict"] != float64(256) {
						t.Errorf("diagnostic changed prediction budget: %+v", request)
					}
					fmt.Fprint(w, `{"done":true,"message":{"tool_calls":[{"function":{"name":"scopeweaver_ping","arguments":{"value":"OK"}}}]}}`)
				case "/api/ps":
					fmt.Fprint(w, `{"models":[{"name":"fixture","context_length":32768}]}`)
				default:
					t.Errorf("unexpected diagnostic path: %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			client, _ := NewClient(srv.URL, "", srv.Client())
			diag, err := client.Test(context.Background(), "fixture", Options{ThinkingType: thinking, MaxTokens: 256})
			if err != nil || !diag.ToolCallVerified {
				t.Fatalf("diagnostic=%+v err=%v", diag, err)
			}
		})
	}
}
