package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestLLMProfileDTOAPIKeyOptionalMatchesEndpointReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, format, endpoint string
		optional               bool
	}{
		{"native", "ollama", "", true},
		{"native-remote", "ollama", "https://ollama.example.invalid", true},
		{"local-openai", "openai", "http://127.0.0.1:1234/v1", true},
		{"local-anthropic", "anthropic", "http://127.0.0.1:1234/v1/messages", true},
		{"local-responses", "openai-responses", "http://[::1]:1234/v1", true},
		{"private-openai", "openai", "http://192.168.1.4:1234/v1", true},
		{"public-openai", "openai", "https://192.0.2.1/v1", false},
		{"public-anthropic", "anthropic", "https://192.0.2.1/anthropic", false},
		{"public-responses", "openai-responses", "https://192.0.2.1/v1", false},
		{"default-openai", "openai", "", false},
		{"default-anthropic", "anthropic", "", false},
		{"default-responses", "openai-responses", "", false},
	} {
		for _, key := range []string{"", "stored-secret-1234"} {
			name := tc.name + "/keyless"
			if key != "" {
				name = tc.name + "/keyed"
			}
			t.Run(name, func(t *testing.T) {
				p := &db.LLMProfile{ID: 17, Name: tc.name, Format: tc.format, Model: "m", BaseURL: tc.endpoint, APIKey: key}
				if key != "" {
					p.APIKeyHint = "…1234"
				}
				dto := llmProfileDTO(p)
				if dto.APIKeyOptional != tc.optional {
					t.Fatalf("optional=%v want=%v", dto.APIKeyOptional, tc.optional)
				}
				raw, err := json.Marshal(dto)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err = json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if got, ok := fields["api_key_optional"]; !ok || got != tc.optional {
					t.Fatalf("serialized capability=%v present=%v", got, ok)
				}
				if _, exists := fields["api_key"]; exists || strings.Contains(string(raw), "stored-secret-1234") {
					t.Fatalf("DTO leaked API key: %s", raw)
				}
				if key != "" && fields["api_key_hint"] != "…1234" {
					t.Fatalf("key hint changed: %s", raw)
				}
			})
		}
	}
}
