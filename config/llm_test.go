package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLLMFileAndEnvironmentPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"llm":{"provider":"ollama","model":"file-model","base_url":"http://localhost:11434","context_window_k":16,"keep_alive":"15m","llm_max_concurrent":2,"max_tokens":2048,"stream":false},"local_llm_only":true,"local_llm_allowed_cidrs":["203.0.113.0/24"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOPEWEAVER_CONFIG", path)
	t.Setenv("ARTEX_LLM_MODEL", "legacy-model")
	t.Setenv("SCOPEWEAVER_LLM_MODEL", "modern-model")
	t.Setenv("ARTEX_LLM_CONTEXT_WINDOW_K", "32")
	t.Setenv("SCOPEWEAVER_LLM_CONTEXT_WINDOW_K", "64")
	c := ResolveLLM()
	if c.Provider != "ollama" || c.Model != "modern-model" || c.ContextWindowK != 64 || c.Stream == nil || *c.Stream || c.KeepAlive != "15m" || c.MaxConcurrent != 2 || c.MaxTokens != 2048 {
		t.Fatalf("resolved=%+v", c)
	}
	strict, cidrs := LocalLLMPolicy()
	if !strict || len(cidrs) != 1 {
		t.Fatalf("file policy=%v %v", strict, cidrs)
	}
	t.Setenv("ARTEX_LOCAL_LLM_ONLY", "true")
	t.Setenv("SCOPEWEAVER_LOCAL_LLM_ONLY", "false")
	t.Setenv("SCOPEWEAVER_LOCAL_LLM_ALLOWED_CIDRS", "10.0.0.0/8, fd00::/8")
	strict, cidrs = LocalLLMPolicy()
	if strict || len(cidrs) != 2 {
		t.Fatalf("env policy=%v %v", strict, cidrs)
	}
	t.Setenv("SCOPEWEAVER_LLM_API_KEY", "")
	t.Setenv("ARTEX_LLM_API_KEY", "legacy-secret")
	if v, _ := LLMEnv("API_KEY"); v != "" {
		t.Fatal("explicit empty modern value ignored")
	}
}

func TestLocalOnlyInvalidEnvironmentFailsClosed(t *testing.T) {
	t.Setenv("SCOPEWEAVER_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	for _, tc := range []struct {
		value string
		want  bool
	}{{"tru", true}, {"", true}, {"true", true}, {"1", true}, {"false", false}, {"0", false}} {
		t.Setenv("SCOPEWEAVER_LOCAL_LLM_ONLY", tc.value)
		strict, _ := LocalLLMPolicy()
		if strict != tc.want {
			t.Errorf("%q strict=%v want=%v", tc.value, strict, tc.want)
		}
	}
}
