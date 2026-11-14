package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type withdrawalProvider struct{ requests []llm.CompletionRequest }

func (p *withdrawalProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.requests = append(p.requests, req)
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Wait for independent verification.")}}, "end_turn", llm.Usage{}, nil
}

func (p *withdrawalProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	panic("test uses Complete")
}

func TestPlannerWithdrawalSurvivesTriggerFreeRound(t *testing.T) {
	t.Setenv("AGENT_CORE_DISABLE_INTERACTIVE_SHELL", "1")
	d := testDB(t)
	defer d.Close()
	row, err := d.CreateTask("withdrawal fixture", "Verify the stored observation independently", nil, 0, 600)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(row.ID)
	ts := d.Exploration(row.ExplorationID)
	intent, fact := withdrawalFixture(t, ts, "withdrawn observation")
	normalIntent, normal := withdrawalFixture(t, ts, "ordinary observation")
	const reason = "The reported observation was hallucinated"
	if _, err := ts.SoftDeleteIntent(intent, "done", reason); err != nil {
		t.Fatal(err)
	}
	provider := &withdrawalProvider{}
	planner := NewPlanner(provider, "fixture", t.TempDir(), nil, 0, 1)
	planner.SetNonStreaming(func() bool { return true })
	ctx := context.Background()
	for _, triggers := range [][]TriggerEvent{
		{{Kind: "cancelled", IntentID: intent, Summary: "withdrawn worker", Detail: reason}},
		nil,
	} {
		if _, _, err := planner.Plan(ctx, row.ID, d.Assets(), ts, row.Goal, triggers, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests=%d want 2", len(provider.requests))
	}
	entries := map[int64][]map[string]any{}
	for _, message := range provider.requests[1].Messages {
		if message.Role == llm.RoleUser {
			for _, block := range message.Content {
				if block.Type == llm.BlockText {
					withdrawalJSONNodes(block.Text, entries)
				}
			}
		}
	}
	if len(entries[fact]) == 0 || len(entries[normal]) == 0 {
		t.Fatalf("later round omitted facts: withdrawn=%v normal=%v", entries[fact], entries[normal])
	}
	for _, entry := range entries[fact] {
		assertWithdrawal(t, entry, intent, reason)
	}
	t.Logf("trigger-free withdrawn fact: %v", entries[fact][0])
	t.Logf("trigger-free normal fact: %v", entries[normal][0])
	wantNormal := map[string]any{
		"id": float64(normal), "from_intent": float64(normalIntent), "summary": "ordinary observation",
		"state": "confirmed", "confidence": "observed",
	}
	for _, entry := range entries[normal] {
		if !reflect.DeepEqual(entry, wantNormal) {
			t.Fatalf("normal output changed: got=%v want=%v", entry, wantNormal)
		}
	}
}

func withdrawalFixture(t *testing.T, ts *db.ExplorationStore, summary string) (int64, int64) {
	t.Helper()
	intent, err := ts.AddNode(db.KindIntent, map[string]any{"summary": summary + " worker"}, 5, "done", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := ts.AddNode(db.KindFact, map[string]any{"summary": summary, "confidence": "observed"}, 5, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Link(intent, db.RelYields, fact); err != nil {
		t.Fatal(err)
	}
	return intent, fact
}

func withdrawalJSONNodes(text string, entries map[int64][]map[string]any) {
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if id, ok := x["id"].(float64); ok {
				entries[int64(id)] = append(entries[int64(id)], x)
			}
			for _, child := range x {
				visit(child)
			}
		case []any:
			for _, child := range x {
				visit(child)
			}
		}
	}
	for i := range text {
		if text[i] == '{' {
			var value any
			if json.NewDecoder(strings.NewReader(text[i:])).Decode(&value) == nil {
				visit(value)
			}
		}
	}
}

func assertWithdrawal(t *testing.T, entry map[string]any, intent int64, reason string) {
	t.Helper()
	withdrawals, ok := entry["withdrawn_output"].([]any)
	if !ok || len(withdrawals) != 1 {
		t.Fatalf("retained output lacks withdrawal: %v", entry)
	}
	w := withdrawals[0].(map[string]any)
	if w["intent_id"] != float64(intent) || w["reason"] != reason || !strings.Contains(entry["withdrawal_notice"].(string), "do not rely on") {
		t.Fatalf("withdrawal lacks provenance/reason/instruction: %v", entry)
	}
}

func TestWithdrawalTravelsWithRetainedToolOutput(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	row, err := d.CreateTask("withdrawal tool fixture", "audit", nil, 0, 600)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(row.ID)
	ts := d.Exploration(row.ExplorationID)
	intent, fact := withdrawalFixture(t, ts, "retained fact")
	finding, err := ts.AddNode(db.KindFinding, map[string]any{"summary": "retained finding"}, 5, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Link(intent, db.RelYields, finding); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AppendActivity(db.Activity{NodeID: &intent, Worker: "fixture", Kind: "result", Summary: "retained conclusion", Detail: "retained full conclusion"}); err != nil {
		t.Fatal(err)
	}
	digest, err := ts.AddDigest(map[string]any{"body": "retained fact folded into a summary"}, []int64{intent, fact})
	if err != nil {
		t.Fatal(err)
	}
	const reason = "unsupported output"
	if _, err := ts.SoftDeleteIntent(intent, "done", reason); err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(ts, "planner")
	current, err := d.CreateTaskWithOptions("consumer", "read inherited output", db.TaskCreateOptions{SourceTaskIDs: []int64{row.ID}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(current.ID)
	for _, tools := range []*ToolSet{tools, NewToolSet(d.Exploration(current.ExplorationID), "planner")} {
		for _, tc := range []struct {
			name  string
			input string
			entry func(any) map[string]any
		}{
			{"list_facts", `{"q":"retained"}`, func(v any) map[string]any { return v.(map[string]any)["facts"].([]any)[0].(map[string]any) }},
			{"list_findings", `{}`, func(v any) map[string]any { return v.([]any)[0].(map[string]any) }},
			{"node_detail", fmt.Sprintf(`{"id":%d}`, fact), func(v any) map[string]any { return v.(map[string]any) }},
			{"node_detail", fmt.Sprintf(`{"id":%d}`, digest), func(v any) map[string]any { return v.(map[string]any) }},
			{"get_worker_output", fmt.Sprintf(`{"intent_id":%d}`, intent), func(v any) map[string]any { return v.(map[string]any) }},
			{"get_worker_trace", fmt.Sprintf(`{"intent_id":%d}`, intent), func(v any) map[string]any { return v.(map[string]any) }},
			{"search_all_worker_traces", `{"q":"retained"}`, func(v any) map[string]any { return v.(map[string]any)["hits"].([]any)[0].(map[string]any) }},
			{"expand_digest", fmt.Sprintf(`{"id":%d}`, digest), func(v any) map[string]any {
				m := v.(map[string]any)
				for _, member := range m["members"].([]any) {
					assertWithdrawal(t, member.(map[string]any), intent, reason)
				}
				return m
			}},
		} {
			t.Run(tc.name+tc.input, func(t *testing.T) {
				var tool actool.CoreTool
				for _, candidate := range tools.PlannerTools() {
					if candidate.Name() == tc.name {
						tool = candidate
						break
					}
				}
				if tool == nil {
					t.Fatalf("tool %s absent", tc.name)
				}
				if tools.ts.ID() != ts.ID() && (tc.name == "get_worker_output" || tc.name == "get_worker_trace") {
					result, err := tool.Call(context.Background(), json.RawMessage(tc.input), nil)
					if err != nil || (!strings.Contains(result.Flatten(), "does not belong") && !strings.Contains(result.Flatten(), "不属于")) {
						t.Fatalf("deleted inherited worker should remain inaccessible: result=%s err=%v", result.Flatten(), err)
					}
					return
				}
				if tools.ts.ID() != ts.ID() && tc.name == "search_all_worker_traces" {
					result := callReadJSON(t, tool, tc.input).(map[string]any)
					if len(result["hits"].([]any)) != 0 {
						t.Fatalf("deleted inherited trace became visible: %v", result)
					}
					return
				}
				entry := tc.entry(callReadJSON(t, tool, tc.input))
				assertWithdrawal(t, entry, intent, reason)
				if tc.name == "get_worker_output" && entry["final_text"] != "retained full conclusion" {
					t.Fatalf("audit output lost: %v", entry)
				}
			})
		}
		overview := tools.graphOverviewData()
		if tools.ts.ID() != ts.ID() {
			overview = overview["related_tasks"].([]map[string]any)[0]
		}
		for _, key := range []string{"cold_digests", "finding_list"} {
			if key == "finding_list" && tools.ts.ID() != ts.ID() {
				key = "recent_findings"
			}
			entry := overview[key].([]map[string]any)[0]
			raw, _ := json.Marshal(entry)
			var decoded map[string]any
			_ = json.Unmarshal(raw, &decoded)
			assertWithdrawal(t, decoded, intent, reason)
		}
	}
}
