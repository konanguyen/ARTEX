package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestModelListCandidatesOpenCodeGo(t *testing.T) {
	t.Run("default opencode-go root", func(t *testing.T) {
		got := modelListCandidates("opencode-go", "", "oc-key")
		if len(got) != 1 {
			t.Fatalf("candidates = %d, want 1", len(got))
		}
		if got[0].url != "https://opencode.ai/zen/go/v1/models" {
			t.Errorf("url = %q, want https://opencode.ai/zen/go/v1/models", got[0].url)
		}
		if got[0].hdr.Get("Authorization") != "Bearer oc-key" {
			t.Errorf("Authorization = %q, want Bearer oc-key", got[0].hdr.Get("Authorization"))
		}
	})

	t.Run("padded opencode-go trims chat completions suffix", func(t *testing.T) {
		got := modelListCandidates(" OpenCode-Go ", "https://gateway.example/zen/go/v1/chat/completions/", "oc-key")
		if len(got) != 1 || got[0].url != "https://gateway.example/zen/go/v1/models" {
			t.Fatalf("unexpected candidates: %+v", got)
		}
	})
}

func TestPgListModelsOpenCodeGoHTTP(t *testing.T) {
	var gotPath, gotAuth, gotUA string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"glm-5.3-flash"},{"id":"qwen3.7-plus"}]}`)
	}))
	defer gateway.Close()

	s := &Server{}
	raw, _ := json.Marshal(map[string]any{
		"provider": "opencode-go",
		"base_url": gateway.URL + "/zen/go/v1/chat/completions",
		"api_key":  "oc-secret",
	})
	rr := httptest.NewRecorder()
	s.pgListModels(rr, httptest.NewRequest(http.MethodPost, "/api/llm/models", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
		Error  string   `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || !slices.Equal(out.Models, []string{"glm-5.3-flash", "qwen3.7-plus"}) {
		t.Fatalf("unexpected response: %+v", out)
	}
	if gotPath != "/zen/go/v1/models" {
		t.Errorf("path = %q, want /zen/go/v1/models", gotPath)
	}
	if gotAuth != "Bearer oc-secret" {
		t.Errorf("Authorization = %q, want Bearer oc-secret", gotAuth)
	}
	if gotUA != "artex" {
		t.Errorf("User-Agent = %q, want artex", gotUA)
	}
}

func TestTestLLMOpenCodeGoSessionHeaderPrecedence(t *testing.T) {
	s := &Server{}
	for _, format := range []string{"openai", "opencode-go"} {
		for _, field := range []string{"omitted", "null", "empty", "custom"} {
			t.Run(format+"/"+field, func(t *testing.T) {
				headers := make(chan http.Header, 1)
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					headers <- r.Header.Clone()
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
				}))
				defer gateway.Close()

				body := map[string]any{
					"provider":  format,
					"model":     "fixture-model",
					"base_url":  gateway.URL + "/v1",
					"api_key":   "fixture-form-key",
					"streaming": false,
				}
				switch field {
				case "null":
					body["session_header_key"] = nil
				case "empty":
					body["session_header_key"] = ""
				case "custom":
					body["session_header_key"] = "X-Requested-Session"
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				rr := httptest.NewRecorder()
				s.testLLM(rr, httptest.NewRequest(http.MethodPost, "/api/llm/test", bytes.NewReader(raw)))
				var result map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if rr.Code != http.StatusOK || result["ok"] != true || result["reply"] != "OK" {
					t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
				}
				got := <-headers
				if got.Get("User-Agent") != "artex" {
					t.Errorf("User-Agent = %q, want artex", got.Get("User-Agent"))
				}
				want := ""
				if format == "opencode-go" {
					want = "x-opencode-session"
				}
				if field == "custom" {
					want = "X-Requested-Session"
				}
				for _, header := range []string{"x-opencode-session", "X-Requested-Session"} {
					if strings.EqualFold(header, want) {
						if !strings.HasPrefix(got.Get(header), "conntest-") {
							t.Errorf("%s = %q, want conntest- prefix", header, got.Get(header))
						}
					} else if got.Get(header) != "" {
						t.Errorf("unexpected header %s = %q", header, got.Get(header))
					}
				}
			})
		}
	}
}
