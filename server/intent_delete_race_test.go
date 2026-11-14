package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func intentDeleteFixture(t *testing.T) (*Server, *Task, int64) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	s := &Server{m: m, engine: NewEngine(m)}
	task, err := m.CreateTask("Intent deletion race", "Withdraw worker output", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteTask(taskID) })
	iid, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "Finished worker"}, 1, "blocked", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, task, iid
}

func deleteIntentHTTP(s *Server, task *Task, iid int64, mode string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/intents/"+fmt.Sprint(iid)+"/control",
		strings.NewReader(fmt.Sprintf(`{"action":"cancel","reason":"Incorrect conclusion","mode":%q}`, mode)))
	r.SetPathValue("id", task.ID)
	r.SetPathValue("iid", fmt.Sprint(iid))
	w := httptest.NewRecorder()
	s.controlIntent(w, r)
	return w
}

func lockDeleteIntent(t *testing.T, s *Server, iid int64) *sql.Tx {
	t.Helper()
	tx, err := s.m.pg.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if err := tx.QueryRow(`SELECT id FROM exploration_nodes WHERE id=$1 FOR UPDATE`, iid).Scan(new(int64)); err != nil {
		t.Fatal(err)
	}
	return tx
}

// Observe the mutation's lock wait, proving the handler has already read the old state.
func waitIntentDeleteLocks(t *testing.T, s *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := s.m.pg.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND query LIKE '%WHERE id=$1 AND exploration_id=$2 AND kind=''intent'' FOR UPDATE%'
			AND pid<>pg_backend_pid()`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d intent deletion row locks", want)
}

func awaitIntentDelete(t *testing.T, result <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-result:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("intent deletion did not finish")
		return nil
	}
}

func TestCancelIntentLockedState(t *testing.T) {
	for _, mode := range []string{"soft", "hard"} {
		for _, rerun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rerun=%t", mode, rerun), func(t *testing.T) {
				s, task, iid := intentDeleteFixture(t)
				tx := lockDeleteIntent(t, s, iid)
				result := make(chan *httptest.ResponseRecorder, 1)
				go func() { result <- deleteIntentHTTP(s, task, iid, mode) }()
				waitIntentDeleteLocks(t, s, 1)
				if rerun {
					// Commit the ReopenIntent -> ClaimIntent states before deletion gets its lock.
					for _, state := range []string{"open", "running"} {
						if _, err := tx.Exec(`UPDATE exploration_nodes SET state=$2 WHERE id=$1`, iid, state); err != nil {
							t.Fatal(err)
						}
					}
				}
				workerCtx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				if rerun {
					s.engine.registerWork(iid, cancel)
					defer func() { _, complete := s.engine.detachWork(iid); complete(nil) }()
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				w := awaitIntentDelete(t, result)
				node, err := task.Store.GetNode(iid)
				if err != nil {
					t.Fatal(err)
				}
				events := task.drainTriggers()
				if rerun {
					if w.Code != http.StatusConflict || node == nil || node.State != "running" || workerCtx.Err() != nil || len(events) != 0 {
						t.Fatalf("stale delete: HTTP %d %s node=%+v worker=%v events=%+v", w.Code, w.Body.String(), node, workerCtx.Err(), events)
					}
					return
				}
				if w.Code != http.StatusOK || len(events) != 1 {
					t.Fatalf("unchanged delete: HTTP %d %s events=%+v", w.Code, w.Body.String(), events)
				}
				if mode == "soft" && (node == nil || node.State != db.StateIntentDeleted) || mode == "hard" && node != nil {
					t.Fatalf("unchanged delete retained wrong state: %+v", node)
				}
			})
		}
	}
}

func TestCancelIntentConcurrentSoftThenHard(t *testing.T) {
	s, task, iid := intentDeleteFixture(t)
	tx := lockDeleteIntent(t, s, iid)
	soft := make(chan *httptest.ResponseRecorder, 1)
	hard := make(chan *httptest.ResponseRecorder, 1)
	go func() { soft <- deleteIntentHTTP(s, task, iid, "soft") }()
	waitIntentDeleteLocks(t, s, 1)
	go func() { hard <- deleteIntentHTTP(s, task, iid, "hard") }()
	waitIntentDeleteLocks(t, s, 2)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	softResponse, hardResponse := awaitIntentDelete(t, soft), awaitIntentDelete(t, hard)
	if softResponse.Code != http.StatusOK || hardResponse.Code != http.StatusConflict {
		t.Fatalf("soft: HTTP %d %s hard: HTTP %d %s", softResponse.Code, softResponse.Body.String(), hardResponse.Code, hardResponse.Body.String())
	}
	node, err := task.Store.GetNode(iid)
	if err != nil || node == nil || node.State != db.StateIntentDeleted {
		t.Fatalf("soft deletion audit trail lost: node=%+v err=%v", node, err)
	}
	if events := task.drainTriggers(); len(events) != 1 {
		t.Fatalf("duplicate cancellation triggers: %+v", events)
	}
}

func TestWorkerSideParentRejectsDeletedIntent(t *testing.T) {
	s, task, iid := intentDeleteFixture(t)
	s.side = &sideQuestionState{}
	for _, state := range []string{"done", "stopped", db.StateIntentDeleted} {
		t.Run(state, func(t *testing.T) {
			if err := task.Store.SetIntentState(iid, state); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/api/tasks/"+task.ID+"/intents/"+fmt.Sprint(iid)+"/side-questions", nil)
			r.SetPathValue("id", task.ID)
			r.SetPathValue("iid", fmt.Sprint(iid))
			w := httptest.NewRecorder()
			parent, ok := s.sideParent(w, r, "worker")
			if state == "done" {
				if !ok || parent.IntentID != iid {
					t.Fatalf("done worker side parent rejected: HTTP %d %s", w.Code, w.Body.String())
				}
			} else if ok || w.Code != http.StatusConflict {
				t.Fatalf("%s worker side parent accepted: HTTP %d %s", state, w.Code, w.Body.String())
			}
		})
	}
}

func TestCancelRunningIntentWaitsForDrain(t *testing.T) {
	for _, mode := range []string{"soft", "hard"} {
		t.Run(mode, func(t *testing.T) {
			s, task, iid := intentDeleteFixture(t)
			if err := task.Store.SetIntentState(iid, "running"); err != nil {
				t.Fatal(err)
			}
			workerCtx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			s.engine.registerWork(iid, cancel)
			defer func() { _, complete := s.engine.detachWork(iid); complete(nil) }()
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { result <- deleteIntentHTTP(s, task, iid, mode) }()
			select {
			case <-workerCtx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("delete did not cancel observed running worker")
			}
			if context.Cause(workerCtx) != agent.AbortWorkCancelledByUser {
				t.Fatalf("worker cause=%v", context.Cause(workerCtx))
			}
			select {
			case w := <-result:
				t.Fatalf("delete finished before drain: HTTP %d %s", w.Code, w.Body.String())
			default:
			}
			// Mirror runWorkerStep's persisted cancellation barrier before completing ControlWork.
			changed, err := task.Store.CompareAndSetIntentState(iid, "running", "paused")
			if err == nil && !changed {
				err = fmt.Errorf("%w: intent %d is no longer in running state", db.ErrIntentStateConflict, iid)
			}
			action, complete := s.engine.detachWork(iid)
			complete(err)
			if err != nil || action != "cancel" {
				t.Fatalf("drain: action=%s err=%v", action, err)
			}
			w := awaitIntentDelete(t, result)
			if w.Code != http.StatusOK {
				t.Fatalf("drained delete: HTTP %d %s", w.Code, w.Body.String())
			}
			if events := task.drainTriggers(); len(events) != 1 {
				t.Fatalf("unfinished worker output deletion=%+v", events)
			}
		})
	}
}
