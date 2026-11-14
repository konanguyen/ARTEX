package agent

import (
	"bytes"
	"encoding/json"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const withdrawalNotice = "Retained for audit: output from the listed intents is withdrawn by the user; do not rely on that output or summaries containing it in further planning."

type outputWithdrawals map[int64][]db.OutputWithdrawal

func (t *ToolSet) outputWithdrawals() (outputWithdrawals, error) {
	out, err := t.ts.OutputWithdrawals()
	if err != nil {
		return nil, err
	}
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		ws, err := source.Store.OutputWithdrawals()
		if err != nil {
			return nil, err
		}
		for id, entries := range ws {
			out[id] = entries
		}
	}
	return out, nil
}

func (ws outputWithdrawals) mark(m map[string]any, id int64) {
	if entries := ws[id]; len(entries) > 0 {
		m["withdrawn_output"] = entries
		m["withdrawal_notice"] = withdrawalNotice
	}
}

// Only descend into exploration result containers; payload/evidence and asset
// IDs are separate namespaces and must never be interpreted as exploration IDs.
func (ws outputWithdrawals) annotate(v any) {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range []string{"id", "intent_id"} {
			if raw, ok := x[key]; ok {
				var id int64
				switch n := raw.(type) {
				case int64:
					id = n
				case json.Number:
					id, _ = n.Int64()
				}
				ws.mark(x, id)
				break
			}
		}
		for _, key := range []string{"facts", "members", "hits", "related_tasks", "open_intents", "running_intents", "recent_done_intents", "finding_list", "recent_facts", "cold_digests", "recent_findings", "recent_intent_results"} {
			ws.annotate(x[key])
		}
	case []map[string]any:
		for _, entry := range x {
			ws.annotate(entry)
		}
	case []any:
		for _, entry := range x {
			ws.annotate(entry)
		}
	}
}

func (t *ToolSet) withdrawnResult(value any) (actool.Result, error) {
	ws, err := t.outputWithdrawals()
	if err != nil {
		return actool.Result{}, err // Do not serve retained output without its withdrawal state.
	}
	if len(ws) == 0 {
		return jsonResult(value)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return actool.Result{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // Keep bigint IDs exact when adding metadata to full nodes.
	var out any
	if err := decoder.Decode(&out); err != nil {
		return actool.Result{}, err
	}
	ws.annotate(out)
	return jsonResult(out)
}
