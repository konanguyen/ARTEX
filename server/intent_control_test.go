package server

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestCancelTerminalIntent(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	s := &Server{m: m, engine: NewEngine(m)}
	task, err := m.CreateTask("Terminal deletion", "worker output", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteTask(taskID) })
	for _, state := range []string{"open", "paused", "done", "blocked", "exhausted", "stopped"} {
		for _, mode := range []string{"soft", "hard"} {
			t.Run(state+"/"+mode, func(t *testing.T) {
				iid, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "Finished worker"}, 1, state, "planner", nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.applyIntentControl(t.Context(), task, iid, "cancel", "  ", mode); err == nil || !strings.Contains(err.Error(), "A deletion reason is required") {
					t.Fatalf("missing reason: %v", err)
				}
				node, err := task.Store.GetNode(iid)
				if err != nil || node == nil || node.State != state {
					t.Fatalf("rejected deletion changed node: %+v %v", node, err)
				}
				result, err := s.applyIntentControl(t.Context(), task, iid, "cancel", "  Incorrect conclusion  ", mode)
				if err != nil {
					t.Fatal(err)
				}
				if result.ID != iid {
					t.Fatalf("result ID=%d", result.ID)
				}
				node, err = task.Store.GetNode(iid)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "soft" {
					if result.State != db.StateIntentDeleted || result.Deleted != nil || node == nil || node.State != db.StateIntentDeleted || node.DeleteReason != "Incorrect conclusion" {
						t.Fatalf("soft result=%+v node=%+v", result, node)
					}
					for _, repeatMode := range []string{"soft", "hard"} {
						if _, err := s.applyIntentControl(t.Context(), task, iid, "cancel", "again", repeatMode); err == nil {
							t.Fatal("already-deleted intent accepted")
						}
					}
				} else if result.State != "" || result.Deleted == nil || result.Deleted.Intents != 1 || node != nil {
					t.Fatalf("hard result=%+v node=%+v", result, node)
				}
				events := task.drainTriggers()
				if len(events) != 1 || events[0].Kind != "cancelled" || events[0].Summary != "Finished worker" || events[0].Detail != "Incorrect conclusion" {
					t.Fatalf("events=%+v", events)
				}
			})
		}
	}
	goal, err := task.Store.AddGoal(map[string]any{"text": "Keep goal"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.applyIntentControl(t.Context(), task, goal, "cancel", "reason", "hard"); err == nil || !strings.Contains(err.Error(), "node is not an intent") {
		t.Fatalf("non-intent cancellation: %v", err)
	}
}
