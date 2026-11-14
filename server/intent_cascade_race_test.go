package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestHardDeleteRefusesLiveDescendant(t *testing.T) {
	s, task, root := intentDeleteFixture(t)
	fact, err := task.Store.AddNode(db.KindFact, map[string]any{"summary": "prerequisite"}, 1, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := task.Store.AddIntent(map[string]any{"summary": "dependent worker"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range []struct {
		src, dst int64
		rel      string
	}{{root, fact, db.RelYields}, {fact, child, db.RelDerivedFrom}} {
		if err := task.Store.Link(edge.src, edge.rel, edge.dst); err != nil {
			t.Fatal(err)
		}
	}
	if claimed, err := task.Store.ClaimIntent(child, "worker"); err != nil || !claimed {
		t.Fatalf("claim child=%t err=%v", claimed, err)
	}
	workerCtx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	s.engine.registerWork(child, cancel)
	defer func() { _, complete := s.engine.detachWork(child); complete(nil) }()
	w := deleteIntentHTTP(s, task, root, "hard")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), fmt.Sprint(child)) {
		t.Fatalf("live cascade: HTTP %d %s; worker=%v", w.Code, w.Body.String(), workerCtx.Err())
	}
	if workerCtx.Err() != nil {
		t.Fatalf("refusal unexpectedly stopped child: %v", workerCtx.Err())
	}
	for _, id := range []int64{root, fact, child} {
		if n, err := task.Store.GetNode(id); err != nil || n == nil {
			t.Fatalf("refusal removed node %d: %+v err=%v", id, n, err)
		}
	}
	if events := task.drainTriggers(); len(events) != 0 {
		t.Fatalf("refusal emitted cancellation: %+v", events)
	}
	// Once the child has drained to paused, the same exclusive cascade succeeds.
	_, complete := s.engine.detachWork(child)
	complete(nil)
	if err := task.Store.SetIntentState(child, "paused"); err != nil {
		t.Fatal(err)
	}
	w = deleteIntentHTTP(s, task, root, "hard")
	if w.Code != http.StatusOK {
		t.Fatalf("drained cascade: HTTP %d %s", w.Code, w.Body.String())
	}
	for _, id := range []int64{root, fact, child} {
		if n, err := task.Store.GetNode(id); err != nil || n != nil {
			t.Fatalf("drained cascade retained node %d: %+v err=%v", id, n, err)
		}
	}
}
