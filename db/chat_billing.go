package db

import (
	"context"
	"database/sql"
	"github.com/Autumn-27/artex/chatstate"
)

// One SQL snapshot avoids double-counting a live update as its result arrives.
// Superseded attempts remain billable; imported copies have NULL token columns.
func chatConversationUsageTx(ctx context.Context, tx *sql.Tx, id int64) (*chatstate.BilledUsage, error) {
	var out chatstate.BilledUsage
	err := tx.QueryRowContext(ctx, `WITH source AS (SELECT * FROM conversation_activities WHERE conversation_id=$1),live AS (
 SELECT DISTINCT ON (COALESCE(run_id,'')) * FROM source u WHERE kind='usage' AND NOT EXISTS(SELECT 1 FROM source r WHERE r.kind='result' AND COALESCE(r.run_id,'')=COALESCE(u.run_id,'')) ORDER BY COALESCE(run_id,''),id DESC
 ), billed AS (SELECT input_tokens,output_tokens,cache_read_tokens,cache_write_tokens FROM source WHERE kind='result' UNION ALL SELECT input_tokens,output_tokens,cache_read_tokens,cache_write_tokens FROM live)
 SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(cache_read_tokens),0),COALESCE(sum(cache_write_tokens),0),(SELECT count(*) FROM source WHERE kind='usage' AND (input_tokens IS NOT NULL OR output_tokens IS NOT NULL)) FROM billed`, id).Scan(&out.InputTokens, &out.OutputTokens, &out.CacheReadTokens, &out.CacheWriteTokens, &out.Turns)
	return &out, err
}
