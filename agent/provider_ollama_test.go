package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestOllamaConfigAndEnvironment(t *testing.T) {
	t.Setenv("SCOPEWEAVER_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("OPENAI_API_KEY", "cloud-secret")
	t.Setenv("OLLAMA_API_KEY", "")
	t.Setenv("ARTEX_LLM_PROVIDER", "openai")
	t.Setenv("SCOPEWEAVER_LLM_PROVIDER", "ollama")
	t.Setenv("SCOPEWEAVER_LLM_MODEL", "")
	t.Setenv("SCOPEWEAVER_LLM_BASE_URL", "")
	t.Setenv("SCOPEWEAVER_LLM_API_KEY", "")
	c, ok := FromEnv()
	if !ok || !c.Ready() || c.APIKey != "" || c.Provider() != "ollama" || c.Model != "qwen2.5-coder:32b" || c.BaseURL != "http://127.0.0.1:11434" || c.MaxTokens != 4096 || c.KeepAlive != "30m" || c.CompactionWindow() != 32768 {
		t.Fatalf("native config=%+v ok=%v", c, ok)
	}
	c.ContextWindowK = 16
	if c.CompactionWindow() != 16384 {
		t.Fatal("16K native window lost")
	}
	c = ConfigFrom("openai", "", "", "key", "")
	if c.CompactionWindow() != 200000 {
		t.Fatal("cloud default changed")
	}
	c.ContextWindowK = 16
	if c.CompactionWindow() != 32000 {
		t.Fatal("cloud clamp changed")
	}
}

func TestNativeProviderWireDefaults(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Header.Get("Authorization") != "" {
			t.Errorf("native endpoint/header: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["options"].(map[string]any)["num_ctx"] != float64(32768) || body["keep_alive"] != "30m" {
			t.Errorf("native controls=%+v", body)
		}
		fmt.Fprint(w, `{"done":true,"message":{"content":"OK"}}`)
	}))
	defer srv.Close()
	c := ConfigFrom("ollama", "model", srv.URL+"/v1", "", "")
	p, err := c.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("ping")}})
	if err != nil || msg.Text() != "OK" {
		t.Fatalf("native=%+v %v", msg, err)
	}
}

func TestKeylessLocalCompatibilityAndCloudWire(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	for _, format := range []string{"openai", "openai-responses", "anthropic"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if _, ok := body["keep_alive"]; ok {
					t.Error("native control leaked to cloud")
				}
				if _, ok := body["options"]; ok {
					t.Error("native options leaked to cloud")
				}
				if r.Header.Get("Authorization") == "Bearer must-not-leak" || r.Header.Get("X-Api-Key") == "must-not-leak" {
					t.Error("cloud environment key inherited")
				}
				switch format {
				case "openai":
					if r.URL.Path != "/custom/chat/completions" {
						t.Errorf("path=%s", r.URL.Path)
					}
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
				case "openai-responses":
					if r.URL.Path != "/custom/responses" {
						t.Errorf("path=%s", r.URL.Path)
					}
					fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`)
				case "anthropic":
					if r.URL.Path != "/custom/v1/messages" {
						t.Errorf("path=%s", r.URL.Path)
					}
					fmt.Fprint(w, `{"role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
				}
			}))
			defer srv.Close()
			c := ConfigFrom(format, "test-model", srv.URL+"/custom", "", "")
			c.Retry.ConnectAttempts = -1
			c.Retry.EmptyAttempts = -1
			if !c.Ready() {
				t.Fatal("explicit local endpoint not ready")
			}
			p, err := c.NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			msg, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hello")}, MaxTokens: 256})
			if err != nil || msg.Text() != "OK" {
				t.Fatalf("compatible result=%+v %v", msg, err)
			}
		})
	}
	for _, url := range []string{"", "https://203.0.113.4/v1"} {
		cfg := ConfigFrom("openai", "", url, "", "")
		if cfg.Ready() {
			t.Errorf("keyless external ready: %s", url)
		}
		if _, err := cfg.NewProvider(); err == nil {
			t.Errorf("keyless external SDK construction accepted: %s", url)
		}
	}
}

func TestConfigHTTPClientStrictGuard(t *testing.T) {
	c := ConfigFrom("openai", "test", "https://203.0.113.4/v1", "key", "")
	c.LocalLLMOnly = true
	c.Retry.ConnectAttempts = -1
	p, err := c.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, _, err = p.Complete(ctx, llm.CompletionRequest{})
	if err == nil || !strings.Contains(err.Error(), "local_llm_only") {
		t.Fatalf("strict=%v", err)
	}
}
