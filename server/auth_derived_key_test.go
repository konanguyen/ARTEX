package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The JWT signing key is HMAC(jwt.key, password hash): any password change —
// through the API or by editing the DB out of process — invalidates every
// token issued before it (#20).
func TestDerivedJWTKeyRotatesWithPassword(t *testing.T) {
	s := &Server{jwtKey: []byte("test-key")}
	s.setPassHash("hash-v1")

	oldTok, err := signJWT(s.signingKey("hash-v1"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.verifyToken(oldTok) {
		t.Fatal("token signed for hash-v1 must verify while the hash is unchanged")
	}

	s.setPassHash("hash-v2")
	if s.verifyToken(oldTok) {
		t.Fatal("token issued before a password change must stop verifying")
	}
	// The failed check above re-read the "DB" (nil store → ""), which in
	// production would return the new hash; re-seed it to model that.
	s.setPassHash("hash-v2")

	newTok, err := signJWT(s.signingKey("hash-v2"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.verifyToken(newTok) {
		t.Fatal("token signed for hash-v2 must verify")
	}
}

// An out-of-process password reset (reset-password.sh) takes effect within the
// hash-cache window: a stale token re-verified after a forced cache refresh fails.
func TestStaleTokenFailsAfterHashRefresh(t *testing.T) {
	s := &Server{jwtKey: []byte("test-key")}
	s.setPassHash("hash-v1")
	oldTok, err := signJWT(s.signingKey("hash-v1"))
	if err != nil {
		t.Fatal(err)
	}

	// The reset happens outside this process: only the DB changes. Force the
	// cache window to look exhausted so verifyToken re-reads it.
	s.authMu.Lock()
	s.passHashAt = s.passHashAt.Add(-passHashCacheTTL - 1)
	s.authMu.Unlock()
	s.m = nil // readPassHash without a store yields "" — the reset removed it

	if s.verifyToken(oldTok) {
		t.Fatal("token must fail once the (now absent) password hash is re-read")
	}
}

// cors() adds nothing for cross-origin callers unless the origin is listed in
// ARTEX_CORS_ORIGINS: the SPA is same-origin and an open ACAO header let other
// sites read authenticated responses (#20).
func TestCORSSameOriginByDefault(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	probe := func(origin string) string {
		h := cors(handler) // cors reads ARTEX_CORS_ORIGINS once per construction
		req := httptest.NewRequest("GET", "/api/tasks", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Header().Get("Access-Control-Allow-Origin")
	}

	t.Setenv("ARTEX_CORS_ORIGINS", "")
	if acao := probe("https://evil.example"); acao != "" {
		t.Fatal("unlisted origin must not receive an ACAO header")
	}

	t.Setenv("ARTEX_CORS_ORIGINS", "https://console.example, https://other.example")
	if acao := probe("https://console.example"); acao != "https://console.example" {
		t.Fatalf("listed origin ACAO = %q", acao)
	}
	if acao := probe("https://evil.example"); acao != "" {
		t.Fatal("unlisted origin must not receive an ACAO header")
	}
}
