package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestRerunRollbackPreservesConcurrentDeletion(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, deleted := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch=%t/deleted=%t", batch, deleted), func(t *testing.T) {
				s, task, iid := intentDeleteFixture(t)
				if err := task.Store.SetIntentBlockedReason(iid, "network_error"); err != nil {
					t.Fatal(err)
				}
				s.concMu.Lock()
				locked := true
				defer func() {
					if locked {
						s.concMu.Unlock()
					}
				}()
				r := httptest.NewRequest(http.MethodPost, "/rerun", nil)
				r.SetPathValue("id", task.ID)
				r.SetPathValue("iid", fmt.Sprint(iid))
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					if batch {
						s.rerunBlocked(w, r)
					} else {
						s.rerunIntent(w, r)
					}
					close(done)
				}()
				deadline := time.Now().Add(5 * time.Second)
				for {
					n, err := task.Store.GetNode(iid)
					if err != nil {
						t.Fatal(err)
					}
					if n != nil && n.State == "open" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("rerun did not reopen intent before admission")
					}
					time.Sleep(5 * time.Millisecond)
				}
				if deleted {
					if _, err := s.applyIntentControl(t.Context(), task, iid, "cancel", "withdraw", "soft"); err != nil {
						t.Fatal(err)
					}
				}
				s.engine.deleting.Store(task.ID, true) // Reject the pending admission.
				defer s.engine.deleting.Delete(task.ID)
				s.concMu.Unlock()
				locked = false
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("rerun did not finish")
				}
				if w.Code != http.StatusInternalServerError {
					t.Fatalf("failed admission: HTTP %d %s", w.Code, w.Body.String())
				}
				n, err := task.Store.GetNode(iid)
				if err != nil || n == nil {
					t.Fatalf("intent=%+v err=%v", n, err)
				}
				if deleted {
					if n.State != db.StateIntentDeleted || n.DeleteReason != "withdraw" || n.BlockedReason != "" {
						t.Fatalf("rollback resurrected deletion: %+v", n)
					}
					if !strings.Contains(w.Body.String(), "admission rollback") {
						t.Fatalf("unapplied rollback not reported: %s", w.Body.String())
					}
				} else if n.State != "blocked" || n.BlockedReason != "network_error" {
					t.Fatalf("untouched open intent not restored: %+v", n)
				}
			})
		}
	}
}
