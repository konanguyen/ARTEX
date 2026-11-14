package db

import "testing"

func TestOllamaProfileRoundTripAndKeylessPool(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	p := &LLMProfile{Name: "t-native-ollama", Format: "ollama", Model: "qwen3:4b", BaseURL: "http://127.0.0.1:11434", KeepAlive: "45m", MaxConcurrent: 2, Streaming: true, ContextWindowK: 32}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	p.ID = id
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	check := func() {
		t.Helper()
		got, err := d.ProfileByID(id)
		if err != nil || got == nil {
			t.Fatalf("read=%+v err=%v", got, err)
		}
		if got.KeepAlive != p.KeepAlive || got.MaxConcurrent != p.MaxConcurrent || got.APIKey != p.APIKey {
			t.Fatalf("profile=%+v", got)
		}
	}
	check()
	p.KeepAlive = "1h"
	p.MaxConcurrent = 3
	if _, err = d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	check()
	p.APIKey = "bearer-token"
	if _, err = d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	check()
	ps, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range ps {
		if v.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ollama profile %d missing from pool: %+v", id, ps)
	}
	p.Format = "openai"
	p.APIKey = ""
	if _, err = d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	ps, err = d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range ps {
		if v.ID == id {
			t.Fatalf("cloud profile without key entered pool: %+v", v)
		}
	}
}
