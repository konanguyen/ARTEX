package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func isolateOpenCodeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ARTEX_LLM_PROVIDER",
		"ARTEX_LLM_MODEL",
		"ARTEX_LLM_BASE_URL",
		"ARTEX_LLM_PROXY",
		"ARTEX_LLM_STREAM",
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"OPENCODE_API_KEY",
	} {
		t.Setenv(k, "")
	}
}

func TestConfigFromOpenCodeGo(t *testing.T) {
	for _, prov := range []string{"opencode-go", " OpenCode-Go "} {
		t.Run(prov, func(t *testing.T) {
			c := ConfigFrom(prov, "", "", "", "")
			if c.Provider() != "openai" {
				t.Errorf("resolved provider = %q, want openai", c.Provider())
			}
			if c.Format != llm.FormatOpenAI {
				t.Errorf("resolved format = %q, want %q", c.Format, llm.FormatOpenAI)
			}
			if c.BaseURL != "https://opencode.ai/zen/go/v1" {
				t.Errorf("default base URL = %q, want https://opencode.ai/zen/go/v1", c.BaseURL)
			}
			if c.Model != "glm-5.3-flash" {
				t.Errorf("default model = %q, want glm-5.3-flash", c.Model)
			}
			if c.SessionHeaderKey != "x-opencode-session" {
				t.Errorf("session header = %q, want x-opencode-session", c.SessionHeaderKey)
			}
		})
	}

	c := ConfigFrom("opencode-go", "qwen3.7-plus", "https://opencode.ai/zen/go/v1/chat/completions/", "oc-key", "")
	if c.BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Errorf("normalized base URL = %q, want https://opencode.ai/zen/go/v1", c.BaseURL)
	}
	if c.Model != "qwen3.7-plus" {
		t.Errorf("explicit model = %q, want qwen3.7-plus", c.Model)
	}
	if c.APIKey != "oc-key" {
		t.Errorf("APIKey = %q, want oc-key", c.APIKey)
	}
}

func TestFromEnvOpenCodeGo(t *testing.T) {
	t.Run("explicit provider uses OPENCODE_API_KEY", func(t *testing.T) {
		isolateOpenCodeEnv(t)
		t.Setenv("ARTEX_LLM_PROVIDER", "opencode-go")
		t.Setenv("OPENAI_API_KEY", "oai-secret")
		t.Setenv("OPENCODE_API_KEY", "opencode-subscription-key")
		cfg, ok := FromEnv()
		if !ok {
			t.Fatal("FromEnv returned ok=false")
		}
		if cfg.Provider() != "openai" || cfg.BaseURL != "https://opencode.ai/zen/go/v1" || cfg.Model != "glm-5.3-flash" {
			t.Fatalf("unexpected config: provider=%q base=%q model=%q", cfg.Provider(), cfg.BaseURL, cfg.Model)
		}
		if cfg.APIKey != "opencode-subscription-key" {
			t.Fatalf("APIKey = %q, want OPENCODE_API_KEY (must not fall back to OPENAI_API_KEY)", cfg.APIKey)
		}
		if cfg.SessionHeaderKey != "x-opencode-session" {
			t.Fatalf("SessionHeaderKey = %q, want x-opencode-session", cfg.SessionHeaderKey)
		}
	})

	t.Run("inferred from OPENCODE_API_KEY alone", func(t *testing.T) {
		isolateOpenCodeEnv(t)
		t.Setenv("OPENCODE_API_KEY", "opencode-subscription-key")
		cfg, ok := FromEnv()
		if !ok {
			t.Fatal("OPENCODE_API_KEY alone did not resolve config")
		}
		if cfg.Provider() != "openai" || cfg.BaseURL != "https://opencode.ai/zen/go/v1" || cfg.APIKey != "opencode-subscription-key" {
			t.Fatalf("inferred config = %+v", cfg)
		}
	})

	t.Run("missing OPENCODE_API_KEY never falls back to OPENAI_API_KEY", func(t *testing.T) {
		isolateOpenCodeEnv(t)
		t.Setenv("ARTEX_LLM_PROVIDER", "opencode-go")
		t.Setenv("OPENAI_API_KEY", "oai-secret")
		cfg, ok := FromEnv()
		if ok || cfg.APIKey != "" {
			t.Fatalf("expected ok=false and empty APIKey, got ok=%v key=%q", ok, cfg.APIKey)
		}
	})

	t.Run("explicit base url, model and roundtrip headers", func(t *testing.T) {
		isolateOpenCodeEnv(t)
		var gotAuth, gotUA, gotSession string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotUA = r.Header.Get("User-Agent")
			gotSession = r.Header.Get("x-opencode-session")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
		}))
		defer srv.Close()

		t.Setenv("ARTEX_LLM_PROVIDER", " OpenCode-Go ")
		t.Setenv("ARTEX_LLM_BASE_URL", srv.URL)
		t.Setenv("ARTEX_LLM_MODEL", "qwen3.7-plus")
		t.Setenv("ARTEX_LLM_STREAM", "false")
		t.Setenv("OPENCODE_API_KEY", "opencode-subscription-key")
		cfg, ok := FromEnv()
		if !ok {
			t.Fatal("FromEnv returned ok=false")
		}
		if cfg.Stream {
			t.Fatal("expected Stream=false when ARTEX_LLM_STREAM=false")
		}
		cfg.Retry = RetryConfig{ConnectAttempts: -1, EmptyAttempts: -1}
		prov, err := cfg.NewProvider()
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		ctx := transcript.WithSessionID(t.Context(), "sess-opencode-1")
		msg, _, _, err := prov.Complete(ctx, llm.CompletionRequest{
			Messages:  []llm.Message{llm.UserText("ping")},
			MaxTokens: 64,
		})
		if err != nil || msg.Text() != "OK" {
			t.Fatalf("Complete failed: msg=%q err=%v", msg.Text(), err)
		}
		if gotAuth != "Bearer opencode-subscription-key" {
			t.Errorf("Authorization = %q, want Bearer opencode-subscription-key", gotAuth)
		}
		if gotUA != "artex" {
			t.Errorf("User-Agent = %q, want artex", gotUA)
		}
		if gotSession != "sess-opencode-1" {
			t.Errorf("x-opencode-session = %q, want sess-opencode-1", gotSession)
		}
	})
}
