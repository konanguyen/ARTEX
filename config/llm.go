package config

import (
	"os"
	"strconv"
	"strings"
)

// LLM is the optional config.json bootstrap profile. Environment values win over file values.
type LLM struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
	Proxy           string `json:"proxy"`
	ContextWindowK  int    `json:"context_window_k"`
	KeepAlive       string `json:"keep_alive"`
	MaxConcurrent   int    `json:"llm_max_concurrent"`
	MaxTokens       int    `json:"max_tokens"`
	ThinkingType    string `json:"thinking_type"`
	ReasoningEffort string `json:"reasoning_effort"`
	Stream          *bool  `json:"stream"`
}

// LLMEnv recognizes an explicitly empty modern value as an override too.
func LLMEnv(suffix string) (string, bool) {
	if v, ok := os.LookupEnv("SCOPEWEAVER_LLM_" + suffix); ok {
		return v, true
	}
	return os.LookupEnv("ARTEX_LLM_" + suffix)
}
func ResolveLLM() LLM {
	c := Load().LLM
	for suffix, dest := range map[string]*string{"PROVIDER": &c.Provider, "MODEL": &c.Model, "BASE_URL": &c.BaseURL, "API_KEY": &c.APIKey, "PROXY": &c.Proxy, "KEEP_ALIVE": &c.KeepAlive, "THINKING_TYPE": &c.ThinkingType, "REASONING_EFFORT": &c.ReasoningEffort} {
		if value, ok := LLMEnv(suffix); ok {
			*dest = strings.TrimSpace(value)
		}
	}
	for suffix, dest := range map[string]*int{"CONTEXT_WINDOW_K": &c.ContextWindowK, "MAX_CONCURRENT": &c.MaxConcurrent, "MAX_TOKENS": &c.MaxTokens} {
		if value, ok := LLMEnv(suffix); ok {
			*dest, _ = strconv.Atoi(value)
		}
	}
	if value, ok := LLMEnv("STREAM"); ok {
		v := true
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "0", "false", "off", "no":
			v = false
		}
		c.Stream = &v
	}
	return c
}

// LocalLLMPolicy resolves file and modern/legacy environment settings. It is opt-in.
func LocalLLMPolicy() (bool, []string) {
	c := Load()
	strict := c.LocalLLMOnly
	cidrs := c.LocalLLMAllowedCIDRs
	for _, prefix := range []string{"SCOPEWEAVER_", "ARTEX_"} {
		if value, ok := os.LookupEnv(prefix + "LOCAL_LLM_ONLY"); ok {
			parsed, err := strconv.ParseBool(strings.TrimSpace(value))
			strict = parsed || err != nil // Malformed explicit security settings fail closed.
			break
		}
	}
	for _, prefix := range []string{"SCOPEWEAVER_", "ARTEX_"} {
		if value, ok := os.LookupEnv(prefix + "LOCAL_LLM_ALLOWED_CIDRS"); ok {
			cidrs = nil
			for _, part := range strings.Split(value, ",") {
				if part = strings.TrimSpace(part); part != "" {
					cidrs = append(cidrs, part)
				}
			}
			break
		}
	}
	return strict, cidrs
}
