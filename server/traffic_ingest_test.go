package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/traffic"
)

// ingestServer wires a Server with a live traffic store, like
// trafficEvidenceServer but without the finding plumbing.
func ingestServer(t *testing.T) (*Server, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	m.traffic, err = traffic.Open(filepath.Join(m.dir, "traffic"), ":0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	s.archiveWG.Wait()
	<-s.side.done
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	return s, func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
}

func TestTrafficIngestHandler(t *testing.T) {
	// Unavailable traffic store returns 503.
	noTraffic := &Server{m: &Manager{}}
	w := httptest.NewRecorder()
	noTraffic.ingestTraffic(w, httptest.NewRequest(http.MethodPost, "/api/traffic/ingest", strings.NewReader(`{"exchanges":[]}`)))
	if w.Code != 503 {
		t.Fatalf("nil traffic store status = %d, want 503", w.Code)
	}

	tr, err := traffic.Open(t.TempDir(), ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	s := &Server{m: &Manager{traffic: tr}}
	call := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/traffic/ingest", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.ingestTraffic(rec, r)
		return rec
	}

	if w = call(`{bad`); w.Code != 400 {
		t.Fatalf("bad json status = %d, want 400", w.Code)
	}
	if w = call(`{"exchanges":[]}`); w.Code != 400 {
		t.Fatalf("empty batch status = %d, want 400", w.Code)
	}
	big := `{"exchanges":[` + strings.Repeat(`{"method":"GET","url":"https://x.example.com/"},`, maxIngestBatch) + `{}]}`
	if w = call(big); w.Code != 400 {
		t.Fatalf("oversized batch status = %d, want 400", w.Code)
	}

	mixed := `{"exchanges":[
		{"method":"POST","url":"https://good.example.com/ok","status":201,"requestHeaders":{"content-type":"application/json"},"requestBody":"{\"a\":1}","responseHeaders":{"Content-Type":"text/plain"},"responseBody":"ok"},
		{"method":"GET","url":"::::","status":200,"responseBody":"bad"}
	]}`
	w = call(mixed)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ingested":1`) || !strings.Contains(w.Body.String(), `"failed":1`) {
		t.Fatalf("mixed batch: %d %s", w.Code, w.Body)
	}

	trunc := `{"exchanges":[{"method":"GET","url":"https://big.example.com/","status":200,"responseBody":"` + strings.Repeat("x", maxIngestBodySize+1024) + `"}]}`
	w = call(trunc)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ingested":1`) {
		t.Fatalf("truncating batch: %d %s", w.Code, w.Body)
	}

	if n, _ := tr.Count(); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	rows, total, err := tr.Page(traffic.PageQuery{Host: "big.example.com", RespMin: -1, RespMax: -1}, 0, 10)
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("page big.example.com: total=%d len=%d err=%v", total, len(rows), err)
	}
	_, resp, err := tr.Get(rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	wantLen := fmt.Sprintf("(len=%d)", maxIngestBodySize+len("\n... [truncated by ingest]"))
	if !strings.Contains(resp, wantLen) {
		t.Fatalf("expected truncated body len %s in stored response: %q", wantLen, resp)
	}
	const blobPrefix = "@blob sha256:"
	_, after, ok := strings.Cut(resp, blobPrefix)
	if !ok {
		t.Fatalf("expected spilled blob marker in stored response: %q", resp)
	}
	hash, _, _ := strings.Cut(after, " ")
	tail, _, err := tr.BlobRange(hash, maxIngestBodySize-16, 128)
	if err != nil {
		t.Fatalf("read spilled blob: %v", err)
	}
	if !strings.Contains(string(tail), "... [truncated by ingest]") {
		t.Fatalf("expected truncated marker in stored blob tail: %q", tail)
	}
}

func TestTrafficIngestFlow(t *testing.T) {
	s, request := ingestServer(t)

	body := fmt.Sprintf(`{"exchanges":[
		{"method":"POST","url":"https://api.example.com/v1/login","status":200,
		 "requestHeaders":{"content-type":"application/json"},"requestBody":"{\"u\":\"u1\"}",
		 "responseHeaders":{"content-type":"application/json"},"responseBody":"{\"token\":\"t\"}"},
		{"method":"GET","url":"https://api.example.com/v1/profile","status":200,
		 "requestHeaders":{},"requestBody":"",
		 "responseHeaders":{},"responseBody":"{\"name\":\"n\"}"}
	]}`)
	w := request("POST", "/api/traffic/ingest", body)
	if w.Code != 200 {
		t.Fatalf("ingest status = %d, body = %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"ingested":2`) || !strings.Contains(w.Body.String(), `"failed":0`) {
		t.Fatalf("unexpected counts: %s", w.Body)
	}

	// Both entries are visible on the normal traffic API, like proxy traffic.
	w = request("GET", "/api/traffic", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "api.example.com") {
		t.Fatalf("traffic page after ingest: %d %s", w.Code, w.Body)
	}

	// One bad URL among good ones: the batch reports per-entry failure.
	mixed := `{"exchanges":[
		{"method":"GET","url":"https://good.example.com/ok","status":200,"responseBody":"ok"},
		{"method":"GET","url":"::::","status":200,"responseBody":"bad"}
	]}`
	w = request("POST", "/api/traffic/ingest", mixed)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ingested":1`) || !strings.Contains(w.Body.String(), `"failed":1`) {
		t.Fatalf("mixed batch: %d %s", w.Code, w.Body)
	}

	// Guards: empty batch, oversized batch, oversized body truncation.
	if w = request("POST", "/api/traffic/ingest", `{"exchanges":[]}`); w.Code != 400 {
		t.Fatalf("empty batch status = %d", w.Code)
	}
	big := `{"exchanges":[` + strings.Repeat(`{"method":"GET","url":"https://x.example.com/"},`, maxIngestBatch) + `{}]}`
	if w = request("POST", "/api/traffic/ingest", big); w.Code != 400 {
		t.Fatalf("oversized batch status = %d", w.Code)
	}
	trunc := `{"exchanges":[{"method":"GET","url":"https://big.example.com/","status":200,"responseBody":"` + strings.Repeat("x", maxIngestBodySize+1024) + `"}]}`
	w = request("POST", "/api/traffic/ingest", trunc)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ingested":1`) {
		t.Fatalf("truncating batch: %d %s", w.Code, w.Body)
	}
	if n, _ := s.m.Traffic().Count(); n != 4 { // 2 + 1 + 1
		t.Fatalf("final count = %d, want 4", n)
	}
}

func TestTrafficIngestRequiresAuth(t *testing.T) {
	s, _ := ingestServer(t)
	r := httptest.NewRequest("POST", "/api/traffic/ingest", strings.NewReader(`{"exchanges":[]}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauthenticated ingest status = %d, want 401", w.Code)
	}
}
