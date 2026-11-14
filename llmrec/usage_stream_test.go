package llmrec

import (
	"context"
	"iter"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/google/uuid"
)

type cumulativeUsageProvider struct {
	completeProvider
	finalOnly bool
}

func (p cumulativeUsageProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		if !p.finalOnly {
			if !y(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 100, OutputTokens: 1, CacheReadTokens: 60, CacheWriteTokens: 10}}, nil) {
				return
			}
			if !y(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 10}}, nil) {
				return
			}
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: llm.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60, CacheWriteTokens: 10}}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func TestStreamUsageCumulativePerCall(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if err := pg.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	if err := pg.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	for _, finalOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "anthropic", true: "openai-final"}[finalOnly], func(t *testing.T) {
			profile := "cumulative-" + uuid.NewString()
			defer pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
			defer pg.Exec(`DELETE FROM llm_records WHERE profile_name=$1`, profile)
			recorder := Wrap(cumulativeUsageProvider{finalOnly: finalOnly}, pg, "fixture", profile, "", "", nil)
			for range 2 {
				for _, err := range recorder.Stream(t.Context(), llm.CompletionRequest{}) {
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			var count, input, output, read, write int
			if err := pg.QueryRow(`SELECT count(*),sum(input_tokens),sum(output_tokens),sum(cache_read),sum(cache_write) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&count, &input, &output, &read, &write); err != nil {
				t.Fatal(err)
			}
			if count != 2 || input != 200 || output != 40 || read != 120 || write != 20 {
				t.Fatalf("ledger calls=%d input/output/cache-read/cache-write=%d/%d/%d/%d want 2 calls, 200/40/120/20", count, input, output, read, write)
			}
			if err := pg.QueryRow(`SELECT sum(output_tokens) FROM llm_records WHERE profile_name=$1`, profile).Scan(&output); err != nil {
				t.Fatal(err)
			}
			if output != 40 {
				t.Fatalf("body-record output=%d want 40", output)
			}
		})
	}
}
