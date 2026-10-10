package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestInterceptJudgeUsageEndpoint(t *testing.T) {
	m := backportTestManager(t)
	var err error
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	td := t.TempDir()
	s := New(ctx, m, td, td, td)
	h := s.Handler()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	// Use an isolated ledger for exact, hand-derived totals.
	if _, err = m.pg.Exec(`DELETE FROM llm_usage`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM llm_usage`) })
	fixtures := []db.LLMUsage{
		{Worker: "judge", ProfileName: "judge-old", InputTokens: 10, OutputTokens: 4, CacheRead: 3, CacheWrite: 2, Status: "ok"},
		{Worker: "judge", ProfileName: "judge-new", InputTokens: 20, OutputTokens: 6, CacheRead: 5, CacheWrite: 1, Status: "error"},
		{Worker: "planner", ProfileName: "other", InputTokens: 1000, OutputTokens: 1000, Status: "ok"},
	}
	for _, row := range fixtures {
		if err = m.pg.InsertLLMUsage(&row); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = m.pg.Exec(`UPDATE llm_usage SET ts=now()-interval '40 days' WHERE profile_name='judge-old'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		days string
		want int
	}{{"30", 30}, {"0", 30}, {"invalid", 30}, {"7", 7}, {"100000", 366}} {
		r := httptest.NewRequest("GET", "/api/intercept/judge/usage?days="+tc.days, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
		}
		var got struct {
			Calls      int `json:"calls"`
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_tokens"`
			CacheWrite int `json:"cache_write_tokens"`
			Daily      []struct {
				Date   string `json:"date"`
				Calls  int    `json:"calls"`
				Input  int    `json:"input_tokens"`
				Output int    `json:"output_tokens"`
			} `json:"daily"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Calls != 2 || got.Input != 30 || got.Output != 10 || got.CacheRead != 8 || got.CacheWrite != 3 {
			t.Fatalf("wrong all-time judge totals: %+v", got)
		}
		// Every UTC day in the window is present, oldest first, ending today.
		if len(got.Daily) != tc.want {
			t.Fatalf("days=%s: %d daily buckets want %d", tc.days, len(got.Daily), tc.want)
		}
		today := time.Now().UTC()
		// Fixture usage by UTC day; every other bucket must be zero.
		want := map[string][3]int{
			today.Format("2006-01-02"):                    {1, 20, 6},
			today.AddDate(0, 0, -40).Format("2006-01-02"): {1, 10, 4},
		}
		for i, day := range got.Daily {
			wantDate := today.AddDate(0, 0, i-len(got.Daily)+1).Format("2006-01-02")
			if day.Date != wantDate {
				t.Fatalf("days=%s: bucket %d date=%s want %s", tc.days, i, day.Date, wantDate)
			}
			if usage := want[day.Date]; [3]int{day.Calls, day.Input, day.Output} != usage {
				t.Fatalf("days=%s: day %s usage=%+v want %v", tc.days, day.Date, day, usage)
			}
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/intercept/judge/usage", nil))
	if w.Code != 401 {
		t.Fatalf("unauthenticated status=%d want 401", w.Code)
	}
}
