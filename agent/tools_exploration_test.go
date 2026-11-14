package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func TestProveGoalWriteFailuresAreAtomic(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	for _, failure := range []struct {
		name, table, event string
		deferred           bool
	}{
		{name: "proof_insert", table: "exploration_edges", event: "BEFORE INSERT"},
		{name: "goal_update", table: "exploration_nodes", event: "BEFORE UPDATE"},
		{name: "commit", table: "exploration_edges", event: "AFTER INSERT", deferred: true},
	} {
		t.Run(failure.name, func(t *testing.T) {
			expID, err := d.CreateExploration("prove_goal atomic write fixture", "record proof before completing")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
			store := d.Exploration(expID)
			goal, err := store.AddGoal(map[string]any{"text": "controlled objective"}, "test")
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := store.AddNode(db.KindFact, map[string]any{"summary": "controlled evidence"}, 0, "confirmed", "test", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(`UPDATE exploration_nodes SET blocked_reason='waiting for proof' WHERE id=$1`, goal); err != nil {
				t.Fatal(err)
			}
			var beforeVersion int64
			if err := d.QueryRow(`SELECT content_version FROM exploration_nodes WHERE id=$1`, goal).Scan(&beforeVersion); err != nil {
				t.Fatal(err)
			}

			// Scope every trigger to this exploration so unrelated fixtures are unaffected.
			name := fmt.Sprintf("prove_goal_failure_%d", expID)
			if _, err := d.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.exploration_id = %d THEN
        RAISE EXCEPTION 'injected prove_goal write failure';
    END IF;
    RETURN NEW;
END $$`, name, expID)); err != nil {
				t.Fatal(err)
			}
			defer d.Exec(fmt.Sprintf(`DROP FUNCTION %s() CASCADE`, name))
			triggerKind, deferClause := "", ""
			if failure.deferred {
				triggerKind, deferClause = "CONSTRAINT ", "DEFERRABLE INITIALLY DEFERRED"
			}
			if _, err := d.Exec(fmt.Sprintf(`CREATE %sTRIGGER %s %s ON %s %s FOR EACH ROW EXECUTE FUNCTION %s()`,
				triggerKind, name, failure.event, failure.table, deferClause, name)); err != nil {
				t.Fatal(err)
			}

			tools := NewToolSet(store, "planner")
			input, err := json.Marshal(map[string]any{"goal_id": goal, "evidence_id": evidence})
			if err != nil {
				t.Fatal(err)
			}
			res, err := tools.proveGoal().Call(context.Background(), input, &actool.ToolContext{ToolUseID: "atomic-proof"})
			if err != nil || !res.IsError || !strings.Contains(res.Flatten(), "injected prove_goal write failure") {
				t.Errorf("write failure must return a tool error with its cause: result=%+v err=%v", res, err)
			}
			var state string
			var blockedReason sql.NullString
			var version, edges int64
			if err := d.QueryRow(`SELECT state, blocked_reason, content_version FROM exploration_nodes WHERE id=$1`, goal).Scan(&state, &blockedReason, &version); err != nil {
				t.Fatal(err)
			}
			if state != "open" || !blockedReason.Valid || blockedReason.String != "waiting for proof" || version != beforeVersion {
				t.Errorf("failed proof mutated goal: state=%q reason=%+v version=%d; want open, original reason, version=%d", state, blockedReason, version, beforeVersion)
			}
			if err := d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND src_id=$2 AND rel=$3 AND dst_id=$4`, expID, evidence, db.RelProves, goal).Scan(&edges); err != nil {
				t.Fatal(err)
			}
			if edges != 0 {
				t.Errorf("failed proof left %d proof edges, want 0", edges)
			}
			if tools.GoalMet || tools.Reason != "" {
				t.Errorf("failed proof signaled completion: GoalMet=%v Reason=%q", tools.GoalMet, tools.Reason)
			}

			if _, err := d.Exec(fmt.Sprintf(`DROP TRIGGER %s ON %s`, name, failure.table)); err != nil {
				t.Fatal(err)
			}
			// Retrying after the fault clears must persist the proof, clear the block,
			// and retain Link's idempotence and SetNodeState's version increments.
			for attempt := int64(1); attempt <= 2; attempt++ {
				res, err := tools.proveGoal().Call(context.Background(), input, &actool.ToolContext{ToolUseID: "atomic-proof"})
				if err != nil || res.IsError {
					t.Fatalf("retry %d failed: result=%+v err=%v", attempt, res, err)
				}
				if err := d.QueryRow(`SELECT state, blocked_reason, content_version FROM exploration_nodes WHERE id=$1`, goal).Scan(&state, &blockedReason, &version); err != nil {
					t.Fatal(err)
				}
				if state != "met" || blockedReason.Valid || version != beforeVersion+attempt {
					t.Errorf("retry %d: state=%q reason=%+v version=%d, want met, NULL, %d", attempt, state, blockedReason, version, beforeVersion+attempt)
				}
				if err := d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND src_id=$2 AND rel=$3 AND dst_id=$4`, expID, evidence, db.RelProves, goal).Scan(&edges); err != nil {
					t.Fatal(err)
				}
				if edges != 1 || !tools.GoalMet {
					t.Errorf("retry %d: edges=%d GoalMet=%v", attempt, edges, tools.GoalMet)
				}
			}
		})
	}
}

func scopeTestTools(t *testing.T, d *db.DB) (*ToolSet, *db.Task) {
	t.Helper()
	task, err := d.CreateTaskWithOptions("scope backend", "isolated fixture", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	tools := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	tools.SetTaskID(task.ID)
	tools.SetAssetStore(d.Assets(), d.Companies())
	return tools, task
}

func TestInsertAssetsAllowSuffix(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	tools, task := scopeTestTools(t, d)
	as := d.Assets()
	if _, err := as.CreateTaskInterceptRule(task.ID, "allow", "fuzzy_domain", "juice-shop.internal", "", true); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE domain IN ('juice-shop.internal2','a.juice-shop.internal','juice-shop.internal')`)
	for _, tc := range []struct {
		kind, domain string
		want         bool
	}{{"root_domain", "juice-shop.internal2", false}, {"subdomain", "a.juice-shop.internal", true}} {
		t.Run(tc.domain, func(t *testing.T) {
			out := callInsertAssets(t, tools, map[string]any{"assets": []any{map[string]any{"type": tc.kind, "domain": tc.domain}}})
			var n int
			if err := d.QueryRow(`SELECT count(*) FROM assets WHERE domain=$1`, tc.domain).Scan(&n); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.want {
				want = 1
			}
			if n != want {
				t.Errorf("asset rows=%d want=%d output=%v", n, want, out)
			}
			rows, err := as.ListTaskScope(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != want {
				t.Errorf("scope rows=%+v want count=%d", rows, want)
			}
		})
	}
}

func TestAddTaskScopeToolGate(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	tools, task := scopeTestTools(t, d)
	if _, err := d.Assets().CreateTaskInterceptRule(task.ID, "allow", "fuzzy_domain", "example.org", "", true); err != nil {
		t.Fatal(err)
	}
	out, err := tools.addTaskScope().Call(context.Background(), json.RawMessage(`{"kind":"subdomain","value":"blocked.example","reason":"fixture"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Flatten(), "whitelist") {
		t.Errorf("expected allowlist rejection message, got: %s", out.Flatten())
	}
	rows, err := d.Assets().ListTaskScope(task.ID)
	if err != nil || len(rows) != 0 {
		t.Errorf("rejection persisted rows=%+v err=%v", rows, err)
	}
}

func TestAddTaskScopeRuleReadFailure(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	for _, table := range []string{"asset_intercept_rules", "task_intercept_rules"} {
		t.Run(table, func(t *testing.T) {
			tools, task := scopeTestTools(t, d)
			if _, err := d.Exec(`ALTER TABLE ` + table + ` RENAME TO gh50_hidden_rules`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := d.Exec(`ALTER TABLE gh50_hidden_rules RENAME TO ` + table); err != nil {
					t.Error(err)
				}
			}()
			out := callReadJSON(t, tools.addTaskScope(), `{"kind":"subdomain","value":"safe.example","reason":"fixture"}`)
			if !strings.Contains(fmt.Sprint(out), "errors") {
				t.Errorf("rule read failure must reject: %v", out)
			}
			rows, err := d.Assets().ListTaskScope(task.ID)
			if err != nil || len(rows) != 0 {
				t.Errorf("failure wrote scope rows=%+v err=%v", rows, err)
			}
		})
	}
}

func TestAddTaskScopeDescriptionMentionsAssetGate(t *testing.T) {
	text := NewToolSet(nil, "").addTaskScope().Description()
	for _, needle := range []string{"不是网络边界", "Interception", "绝不自行扩大"} {
		if !strings.Contains(text, needle) {
			t.Errorf("add_task_scope description missing %q: %s", needle, text)
		}
	}
}
