package db

import (
	"errors"
	"testing"
)

// mustIntent / mustNode / mustLink are terse builders for the delete-cascade tests.
func mustIntent(t *testing.T, es *ExplorationStore, summary string) int64 {
	t.Helper()
	id, err := es.AddIntent(map[string]any{"summary": summary}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustNode(t *testing.T, es *ExplorationStore, kind, summary string) int64 {
	t.Helper()
	id, err := es.AddNode(kind, map[string]any{"summary": summary}, 1, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustLink(t *testing.T, es *ExplorationStore, from int64, rel string, to int64) {
	t.Helper()
	if err := es.Link(from, rel, to); err != nil {
		t.Fatal(err)
	}
}

func gone(t *testing.T, es *ExplorationStore, id int64) bool {
	t.Helper()
	n, err := es.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	return n == nil
}

// TestSoftDeleteIntent 假删除置 deleted + delete_reason,保留节点。
func TestSoftDeleteIntent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("soft delete", "假删除")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	intent := mustIntent(t, es, "待删意图")
	if err := es.SetIntentState(intent, "paused"); err != nil {
		t.Fatal(err)
	}
	summary, err := es.SoftDeleteIntent(intent, "paused", "方向判断错误")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "待删意图" {
		t.Fatalf("summary=%q, want 待删意图", summary)
	}
	n, err := es.GetNode(intent)
	if err != nil || n == nil {
		t.Fatalf("intent removed by soft delete: n=%+v err=%v", n, err)
	}
	if n.State != StateIntentDeleted || n.DeleteReason != "方向判断错误" {
		t.Fatalf("state=%q delete_reason=%q, want deleted/方向判断错误", n.State, n.DeleteReason)
	}
	// 待领(open)意图也允许假删除。
	openIntent := mustIntent(t, es, "待领意图")
	if _, err := es.SoftDeleteIntent(openIntent, "open", "方向不需要了"); err != nil {
		t.Fatalf("soft delete open intent: %v", err)
	}
	if n, err := es.GetNode(openIntent); err != nil || n == nil || n.State != StateIntentDeleted {
		t.Fatalf("open intent not soft-deleted: n=%+v err=%v", n, err)
	}

	// 已删除(deleted)等其它状态不能再假删除。
	if _, err := es.SoftDeleteIntent(intent, StateIntentDeleted, "再删"); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatal("soft-deleting an already-deleted intent unexpectedly succeeded")
	}
}

func TestDeleteIntentStateConflict(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("stale deletion", "Reject changed or deleted intent")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	store := d.Exploration(expID)
	for _, mode := range []string{"soft", "hard"} {
		for _, state := range []string{"running", StateIntentDeleted} {
			t.Run(mode+"/"+state, func(t *testing.T) {
				iid := mustIntent(t, store, "Worker")
				if err := store.SetIntentState(iid, state); err != nil {
					t.Fatal(err)
				}
				for _, expected := range []string{"blocked", StateIntentDeleted} {
					var err error
					if mode == "soft" {
						_, err = store.SoftDeleteIntent(iid, expected, "Withdraw")
					} else {
						_, err = store.CancelIntent(iid, expected)
					}
					if !errors.Is(err, ErrIntentStateConflict) {
						t.Fatalf("expected=%q state=%q: got %v, want ErrIntentStateConflict", expected, state, err)
					}
				}
				node, err := store.GetNode(iid)
				if err != nil || node == nil || node.State != state || node.DeleteReason != "" {
					t.Fatalf("conflict changed intent: %+v %v", node, err)
				}
			})
		}
	}
}

// TestHardDeleteCascadesExclusiveDescendants 真删除沿链路级联删除独占子孙到叶子。
func TestHardDeleteCascadesExclusiveDescendants(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("hard cascade", "级联删除")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	// intent1 --yields--> fact1 --derived_from--> intent2 --yields--> fact2(叶子)
	intent1 := mustIntent(t, es, "根意图")
	fact1 := mustNode(t, es, KindFact, "事实1")
	mustLink(t, es, intent1, RelYields, fact1)
	intent2 := mustIntent(t, es, "衍生意图")
	mustLink(t, es, fact1, RelDerivedFrom, intent2)
	fact2 := mustNode(t, es, KindFact, "事实2")
	mustLink(t, es, intent2, RelYields, fact2)

	cleanup, err := es.CancelIntent(intent1, "open")
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Intents != 2 || cleanup.Facts != 2 {
		t.Fatalf("cleanup=%+v, want 2 intents / 2 facts", cleanup)
	}
	for _, id := range []int64{intent1, fact1, intent2, fact2} {
		if !gone(t, es, id) {
			t.Fatalf("node %d survived cascade", id)
		}
	}
}

// TestHardDeletePreservesSharedAndGoal 真删除保留共享子孙(还有其它父)与目标。
func TestHardDeletePreservesSharedAndGoal(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("hard preserve", "保留共享/目标")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	goal, err := es.AddGoal(map[string]any{"text": "拿下后台"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	// intent1 独占 finding(proves goal),intent1 与 intentX 共享 fact1(fact1 衍生出 intent2)。
	intent1 := mustIntent(t, es, "待删意图")
	intentX := mustIntent(t, es, "旁路意图")
	finding := mustNode(t, es, KindFinding, "漏洞")
	mustLink(t, es, intent1, RelYields, finding)
	mustLink(t, es, finding, RelProves, goal)
	shared := mustNode(t, es, KindFact, "共享事实")
	mustLink(t, es, intent1, RelYields, shared)
	mustLink(t, es, intentX, RelYields, shared)
	intent2 := mustIntent(t, es, "由共享事实衍生")
	mustLink(t, es, shared, RelDerivedFrom, intent2)

	cleanup, err := es.CancelIntent(intent1, "open")
	if err != nil {
		t.Fatal(err)
	}
	// 只删 intent1 与其独占的 finding;shared(有 intentX 父)及其下游 intent2、goal 全保留。
	if cleanup.Intents != 1 || cleanup.Findings != 1 || cleanup.Facts != 0 {
		t.Fatalf("cleanup=%+v, want 1 intent / 1 finding / 0 fact", cleanup)
	}
	if !gone(t, es, intent1) || !gone(t, es, finding) {
		t.Fatal("intent1/finding should be removed")
	}
	for _, id := range []int64{goal, intentX, shared, intent2} {
		if gone(t, es, id) {
			t.Fatalf("node %d was wrongly cascaded", id)
		}
	}
}

// Terminal worker output is retained by soft deletion and exclusively cascaded by hard deletion.
func TestDeleteTerminalIntent(t *testing.T) {
	for _, state := range []string{"done", "blocked", "exhausted", "stopped"} {
		for _, mode := range []string{"soft", "hard"} {
			t.Run(state+"/"+mode, func(t *testing.T) {
				d, err := Open(testDSN(t))
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				expID, err := d.CreateExploration("terminal deletion", "worker output")
				if err != nil {
					t.Fatal(err)
				}
				defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
				es := d.Exploration(expID)
				intent := mustIntent(t, es, "Finished worker")
				if err := es.SetIntentState(intent, state); err != nil {
					t.Fatal(err)
				}
				fact := mustNode(t, es, KindFact, "Exclusive fact")
				finding := mustNode(t, es, KindFinding, "Exclusive finding")
				findingID, err := d.AddFinding(0, finding, "fixture", "Finding", SeverityLow, "Summary", "Evidence", "worker", nil)
				if err != nil {
					t.Fatal(err)
				}
				// Clean up retained standalone rows before the exploration's FK nulls node_id.
				defer d.Exec(`DELETE FROM findings WHERE id=$1`, findingID)
				goal, err := es.AddGoal(map[string]any{"text": "Keep goal"}, "human")
				if err != nil {
					t.Fatal(err)
				}
				origin, err := es.AddNode(KindFact, map[string]any{"summary": "Origin"}, 1, StateOrigin, "human", nil)
				if err != nil {
					t.Fatal(err)
				}
				other := mustIntent(t, es, "Surviving worker")
				shared := mustNode(t, es, KindFact, "Shared fact")
				covered := mustNode(t, es, KindFact, "Cold digest member")
				digest, err := es.AddNode(KindDigest, map[string]any{"summary": "Surviving cold digest"}, 1, StateDigestActive, "compactor", nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, id := range []int64{fact, finding, goal, origin, shared, covered} {
					mustLink(t, es, intent, RelYields, id)
				}
				mustLink(t, es, other, RelYields, shared)
				mustLink(t, es, digest, RelCovers, covered)
				activityID, err := es.AppendActivity(Activity{NodeID: &intent, Worker: "worker", Kind: "result", Detail: "Worker conclusion"})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "soft" {
					summary, err := es.SoftDeleteIntent(intent, state, "Output is incorrect")
					if err != nil {
						t.Fatal(err)
					}
					n, err := es.GetNode(intent)
					if err != nil || n == nil || n.State != StateIntentDeleted || n.DeleteReason != "Output is incorrect" || summary != "Finished worker" {
						t.Fatalf("soft deletion: node=%+v summary=%q err=%v", n, summary, err)
					}
					for _, id := range []int64{fact, finding} {
						if gone(t, es, id) {
							t.Fatalf("soft deletion removed output %d", id)
						}
					}
				} else {
					cleanup, err := es.CancelIntent(intent, state)
					if err != nil {
						t.Fatal(err)
					}
					if cleanup != (IntentCleanup{Intents: 1, Facts: 1, Findings: 1, Activities: 1}) {
						t.Fatalf("cleanup=%+v", cleanup)
					}
					for _, id := range []int64{intent, fact, finding} {
						if !gone(t, es, id) {
							t.Fatalf("exclusive node %d survived", id)
						}
					}
				}
				for _, id := range []int64{goal, origin, other, shared, covered, digest} {
					if gone(t, es, id) {
						t.Fatalf("protected/shared node %d removed", id)
					}
				}
				f, err := d.GetFinding(findingID)
				if err != nil || (f != nil) != (mode == "soft") {
					t.Fatalf("finding=%+v err=%v mode=%s", f, err, mode)
				}
				var count int
				if err := d.QueryRow(`SELECT count(*) FROM activity WHERE id=$1`, activityID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if (count == 1) != (mode == "soft") {
					t.Fatalf("activity count=%d mode=%s", count, mode)
				}
			})
		}
	}
}
