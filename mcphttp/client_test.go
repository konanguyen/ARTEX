package mcphttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// initRequest captures what the client sent for one MCP request.
type initRequest struct {
	method, header, bodyVersion string
}

func recordRequest(r *http.Request) (rpcRequest, initRequest) {
	var req rpcRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	rec := initRequest{method: req.Method, header: r.Header.Get("MCP-Protocol-Version")}
	if p, ok := req.Params.(map[string]any); ok {
		rec.bodyVersion, _ = p["protocolVersion"].(string)
	}
	return req, rec
}

// TestStreamableClientProtocolVersions uses spec-shaped servers that each support
// one version: they answer initialize with their own version, accept a request
// without MCP-Protocol-Version, and reject any other header value. The client
// must send no version header on initialize and the negotiated one afterwards.
func TestStreamableClientProtocolVersions(t *testing.T) {
	for _, supported := range []string{"2025-11-25", "2025-06-18", "2025-03-26"} {
		t.Run(supported, func(t *testing.T) {
			var seen []initRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req, rec := recordRequest(r)
				seen = append(seen, rec)
				if rec.header != "" && rec.header != supported {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"code":"UNSUPPORTED_PROTOCOL_VERSION"}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case "initialize":
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`, req.ID, supported)
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				case "tools/list":
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, req.ID)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			c, err := New(context.Background(), "t", srv.URL, nil, false)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close()
			if _, err := c.Tools(context.Background()); err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			if c.protocol != supported {
				t.Fatalf("negotiated protocol = %q", c.protocol)
			}
			var inits int
			for _, rec := range seen {
				if rec.method == "initialize" {
					inits++
					if rec.header != "" {
						t.Fatalf("initialize carried MCP-Protocol-Version %q", rec.header)
					}
					if rec.bodyVersion != protocolVersion {
						t.Fatalf("initialize offered %q, want %q", rec.bodyVersion, protocolVersion)
					}
					continue
				}
				if rec.header != supported {
					t.Fatalf("%s carried MCP-Protocol-Version %q, want %q", rec.method, rec.header, supported)
				}
			}
			if inits != 1 {
				t.Fatalf("initialize attempts = %d, want 1", inits)
			}
		})
	}
}

// TestStreamableClientVersionRejectionForms keeps the fallback loop honest for
// servers that reject the initialize body version instead of negotiating: an
// HTTP 400 "Unsupported protocol version", the JSON-RPC -32602 error from the
// spec's example, and an unrelated failure that must not trigger a retry.
func TestStreamableClientVersionRejectionForms(t *testing.T) {
	cases := []struct {
		name     string
		reject   func(w http.ResponseWriter, id int)
		fallback bool
	}{
		{"http 400", func(w http.ResponseWriter, _ int) {
			http.Error(w, "Bad Request: Unsupported protocol version", http.StatusBadRequest)
		}, true},
		{"json-rpc -32602", func(w http.ResponseWriter, id int) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"Unsupported protocol version","data":{"supported":["2025-06-18"],"requested":"2025-11-25"}}}`, id)
		}, true},
		{"unrelated 401", func(w http.ResponseWriter, _ int) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var inits []initRequest
			var after []initRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req, rec := recordRequest(r)
				if req.Method == "initialize" {
					inits = append(inits, rec)
					if rec.bodyVersion != protocolVersionFallback {
						tc.reject(w, req.ID)
						return
					}
				} else {
					after = append(after, rec)
				}
				w.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case "initialize":
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`, req.ID)
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			c, err := New(context.Background(), "t", srv.URL, nil, false)
			for _, rec := range inits {
				if rec.header != "" {
					t.Fatalf("initialize carried MCP-Protocol-Version %q", rec.header)
				}
			}
			if !tc.fallback {
				if err == nil {
					_ = c.Close()
					t.Fatal("expected connect error")
				}
				if len(inits) != 1 {
					t.Fatalf("unrelated error retried: %d initialize attempts", len(inits))
				}
				return
			}
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close()
			if len(inits) != 2 || inits[0].bodyVersion != protocolVersion || inits[1].bodyVersion != protocolVersionFallback {
				t.Fatalf("fallback attempts = %+v", inits)
			}
			if c.protocol != protocolVersionFallback {
				t.Fatalf("negotiated protocol = %q", c.protocol)
			}
			if len(after) == 0 || after[0].method != "notifications/initialized" || after[0].header != protocolVersionFallback {
				t.Fatalf("requests after initialize = %+v", after)
			}
		})
	}
}
