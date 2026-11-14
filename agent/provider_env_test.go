package agent

import (
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// ARTEX_LLM_PROVIDER is matched regardless of case and surrounding spaces. A
// miss used to fall through to anthropic: "OpenAI" with mixed casing then sent
// ANTHROPIC_API_KEY instead of OPENAI_API_KEY.
func TestFromEnvProviderIsCaseInsensitive(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")
	t.Setenv("OPENAI_API_KEY", "sk-oai")
	t.Setenv("ARTEX_LLM_BASE_URL", "")
	t.Setenv("ARTEX_LLM_MODEL", "")
	for _, tc := range []struct {
		env  string
		want llm.Format
		key  string
	}{
		{"OpenAI", llm.FormatOpenAI, "sk-oai"},
		{" OPENAI ", llm.FormatOpenAI, "sk-oai"},
		{"OpenAI-Responses", llm.FormatOpenAIResponses, "sk-oai"},
		{" OPENAI-RESPONSES ", llm.FormatOpenAIResponses, "sk-oai"},
		{"Anthropic", llm.FormatAnthropic, "sk-ant-secret"},
		{" ANTHROPIC ", llm.FormatAnthropic, "sk-ant-secret"},
	} {
		t.Setenv("ARTEX_LLM_PROVIDER", tc.env)
		c, ok := FromEnv()
		if !ok {
			t.Fatalf("%q: FromEnv returned ok=false", tc.env)
		}
		if c.Format != tc.want || c.APIKey != tc.key {
			t.Fatalf("%q: format=%v key=%q, want format=%v key=%q", tc.env, c.Format, c.APIKey, tc.want, tc.key)
		}
	}
}
