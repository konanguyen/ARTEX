package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

type policyRoundTrip func(*http.Request) (*http.Response, error)

func (f policyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type toggleRead struct{ read func([]byte) (int, error) }

func (b toggleRead) Read(p []byte) (int, error) { return b.read(p) }
func (b toggleRead) Close() error               { return nil }
func strictTestServer(t *testing.T) *Server {
	t.Helper()
	m := backportTestManager(t)
	old, found, _ := m.pg.GetSetting(settingLocalLLMOnly)
	t.Cleanup(func() {
		if found {
			m.pg.SetSetting(settingLocalLLMOnly, old)
		} else {
			m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingLocalLLMOnly)
		}
	})
	t.Setenv("SCOPEWEAVER_LOCAL_LLM_ONLY", "false")
	m.pg.SetBool(settingLocalLLMOnly, false)
	return &Server{m: m}
}
func TestUpdateStrictBlocksBeforeFetchAndCachedResult(t *testing.T) {
	s := strictTestServer(t)
	previous := relCache
	t.Cleanup(func() { relCache = previous })
	calls := 0
	relCache = &releaseCache{rel: &selfupdate.Release{TagName: "v99.0.0", Body: "cached-secret"}, at: time.Now(), fetch: func(context.Context, *http.Client) (*selfupdate.Release, error) { calls++; return nil, nil }}
	s.m.pg.SetBool(settingLocalLLMOnly, true)
	for _, handler := range []http.HandlerFunc{s.guardedUpdateCheck, s.guardedUpdateApply} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("POST", "/api/update/check?force=1", nil))
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out["local_llm_only"] != true || out["error"] == nil || out["latest"] != nil || strings.Contains(w.Body.String(), "cached-secret") || calls != 0 {
			t.Fatalf("strict cache/fetch leaked: calls=%d %s", calls, w.Body)
		}
	}
}
func TestUpdateLiveRoundTripRedirectAndReadGuards(t *testing.T) {
	s := strictTestServer(t)
	calls := 0
	inner := policyRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data"))}, nil
	})
	client := s.guardedUpdateClient(&http.Client{Transport: inner})
	s.m.pg.SetBool(settingLocalLLMOnly, true)
	if _, err := client.Get("https://example.invalid"); err == nil || calls != 0 {
		t.Fatal("strict round trip admitted")
	}
	s.m.pg.SetBool(settingLocalLLMOnly, false)
	resp, err := client.Get("https://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	s.m.pg.SetBool(settingLocalLLMOnly, true)
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("stale body bypassed live toggle")
	}
	resp.Body.Close()
	redirects := 0
	client = s.guardedUpdateClient(&http.Client{Transport: inner, CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil }})
	req, _ := http.NewRequest("GET", "https://example.invalid", nil)
	if err := client.CheckRedirect(req, nil); err == nil || redirects != 0 {
		t.Fatal("strict redirect admitted")
	}
	s.m.pg.SetBool(settingLocalLLMOnly, false)
	if err := client.CheckRedirect(req, nil); err != nil || redirects != 1 {
		t.Fatal("normal redirect contract changed")
	}
	// A toggle during an in-flight read must discard bytes and EOF alike.
	body := &updatePolicyBody{s: s, ReadCloser: toggleRead{read: func(p []byte) (int, error) {
		copy(p, "late")
		s.m.pg.SetBool(settingLocalLLMOnly, true)
		return 4, io.EOF
	}}}
	if n, err := body.Read(make([]byte, 8)); n != 0 || err == nil || err == io.EOF {
		t.Fatalf("in-flight bytes escaped %d %v", n, err)
	}
	h := &updateHub{subs: map[chan updateProgress]struct{}{}}
	h.begin("v99")
	h.finish(s.updatePolicyError())
	current, running := h.snapshot()
	if running || current.Phase != selfupdate.PhaseFailed {
		t.Fatal("policy failure left hub running")
	}
}

// The admitted asynchronous apply path uses the guarded client all the way
// through Stage. A later strict toggle aborts and cleans its .part ZIP.
func TestUpdateAsyncStageStopsAndCleansAfterToggle(t *testing.T) {
	s := strictTestServer(t)
	s.ctx = t.Context()
	oldCache, oldHub, oldVersion := relCache, updHub, BuildVersion
	t.Cleanup(func() { relCache, updHub, BuildVersion = oldCache, oldHub, oldVersion })
	BuildVersion = "v0.1.0"
	updHub = &updateHub{subs: map[chan updateProgress]struct{}{}}
	name := selfupdate.AssetName("v99.0.0", runtime.GOOS, runtime.GOARCH)
	assetURL := "https://github.com/cskwork/scopeweaver/releases/download/v99.0.0/" + name
	sumsURL := "https://github.com/cskwork/scopeweaver/releases/download/v99.0.0/SHA256SUMS"
	entered, allowRead := make(chan struct{}), make(chan struct{})
	calls := 0
	relCache = &releaseCache{fetch: func(_ context.Context, client *http.Client) (*selfupdate.Release, error) {
		client.Transport.(*updatePolicyTransport).inner = policyRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.String() == sumsURL {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("0", 64) + "  " + name + "\n"))}, nil
			}
			if r.URL.String() != assetURL {
				t.Errorf("unexpected fixture URL %s", r.URL)
				return nil, errors.New("unexpected fixture URL")
			}
			first := true
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: toggleRead{read: func(p []byte) (int, error) {
				if first {
					first = false
					close(entered)
					<-allowRead
					copy(p, "partial")
					return 7, nil
				}
				return 0, io.EOF
			}}}, nil
		})
		return &selfupdate.Release{TagName: "v99.0.0", Assets: []selfupdate.Asset{{Name: name, URL: assetURL, Size: 100}, {Name: "SHA256SUMS", URL: sumsURL}}}, nil
	}}
	w := httptest.NewRecorder()
	s.guardedUpdateApply(w, httptest.NewRequest("POST", "/api/update/apply", nil))
	if w.Code != 202 {
		t.Fatalf("not admitted: %d %s", w.Code, w.Body)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("download did not start")
	}
	s.m.pg.SetBool(settingLocalLLMOnly, true)
	close(allowRead)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, running := updHub.snapshot()
		if !running {
			if current.Phase != selfupdate.PhaseFailed || !strings.Contains(current.Error, "local_llm_only") {
				t.Fatalf("hub did not fail closed %+v", current)
			}
			paths, err := selfupdate.ResolvePaths()
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{paths.New, paths.New + ".part", paths.New + ".zip.part"} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("partial staging remains %s: %v", path, err)
				}
			}
			if calls != 2 {
				t.Fatalf("unexpected external round trips %d", calls)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("update hub remained running")
}
