package agent

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
)

func TestChatPromptPreservesRoleOnMissingOrInvalidTemplate(t *testing.T) {
	previous := PromptOverride()
	t.Cleanup(func() { SetPromptOverride(previous) })

	roles := map[string]string{
		"auto": autoDefaultTmpl, "pentest": pentestDefaultTmpl,
		"reporter": ReporterDefaultPrompt, "retester": RetesterDefaultPrompt,
		"custom": DefaultAssistantPrompt,
	}
	for role, fallback := range roles {
		for _, tc := range []struct {
			name, body string
			exists     bool
		}{
			{name: "missing"},
			{name: "empty", exists: true},
			{name: "unknown variable", body: "{{.UnknownVariable}}", exists: true},
			{name: "invalid syntax", body: "{{if}}", exists: true},
		} {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				SetPromptOverride(func(string) (string, bool) { return tc.body, tc.exists })
				got := chatSystem(role, "/data", "/work")
				// The code-owned untrusted-content tail and the noise-profile block
				// follow the rendered body (F6/F13 tails, this port's egress block).
				want := fallback + untrustedContentTail(fallback) + chatWorkDirSpec("/work") + noiseProfileBlock()
				if got != want {
					t.Fatal("chat lost its role default or file output rules")
				}
			})
		}
		t.Run(role+"/edited template", func(t *testing.T) {
			SetPromptOverride(func(string) (string, bool) { return "EDITED {{.DataDir}} {{.Now}}", true })
			got := chatSystem(role, "/data", "/work")
			if !strings.HasPrefix(got, "EDITED /data ") || strings.Contains(got, "{{") || !strings.HasSuffix(got, chatWorkDirSpec("/work")+noiseProfileBlock()) {
				t.Fatal("valid edited prompt was not rendered intact")
			}
		})
	}
}

func TestRenderSystemReportsInvalidTemplateWithoutLoggingPrompt(t *testing.T) {
	previous := PromptOverride()
	output := log.Writer()
	t.Cleanup(func() {
		SetPromptOverride(previous)
		log.SetOutput(output)
	})
	var logs bytes.Buffer
	log.SetOutput(&logs)
	for _, tc := range []struct {
		name, body, diagnostic string
	}{
		{name: "text", body: "PRIVATE_PROMPT_TEXT {{.UnknownVariable}}", diagnostic: "[prompt] agent=%q: template execution failed; using role default"},
		{name: "parse expression", body: "{{PRIVATE_PROMPT_TEXT}}", diagnostic: "[prompt] agent=%q: template parsing failed; using role default"},
		{name: "execute expression", body: `{{index .DataDir "PRIVATE_PROMPT_TEXT"}}`, diagnostic: "[prompt] agent=%q: template execution failed; using role default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetPromptOverride(func(string) (string, bool) { return tc.body, true })
			logs.Reset()
			if got := chatSystem("pentest", "/data", "/work"); !strings.HasPrefix(got, pentestDefaultTmpl) {
				t.Fatal("invalid prompt lost the pentest role")
			}
			if !strings.Contains(logs.String(), fmt.Sprintf(tc.diagnostic, "pentest")) {
				t.Fatalf("invalid prompt did not produce expected log entry: %s", logs.String())
			}
			if strings.Contains(logs.String(), "PRIVATE_PROMPT_TEXT") {
				t.Fatal("render failure logged the prompt body")
			}
		})
	}
}
