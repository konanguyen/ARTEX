package db

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func cascadeFixture(t *testing.T) (*DB, *ExplorationStore, int64, int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exp, err := d.CreateExploration("cascade concurrency", "preserve surviving support")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, exp) })
	store := d.Exploration(exp)
	root := mustIntent(t, store, "finished")
	if err := store.SetIntentState(root, "done"); err != nil {
		t.Fatal(err)
	}
	fact := mustNode(t, store, KindFact, "worker output")
	mustLink(t, store, root, RelYields, fact)
	return d, store, root, fact
}

func waitCascadeLock(t *testing.T, d *DB, query string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND query LIKE $1 AND pid<>pg_backend_pid())`, query).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no lock wait for %q", query)
}

type cascadeResult struct {
	cleanup IntentCleanup
	err     error
}

func startCascade(store *ExplorationStore, root int64) <-chan cascadeResult {
	done := make(chan cascadeResult, 1)
	go func() {
		cleanup, err := store.CancelIntent(root, "done")
		done <- cascadeResult{cleanup, err}
	}()
	return done
}

func awaitCascade(t *testing.T, done <-chan cascadeResult) IntentCleanup {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("cascade did not finish")
		return IntentCleanup{}
	}
}

func TestCancelIntentPreservesDigestCommittedDuringCascade(t *testing.T) {
	d, store, root, fact := cascadeFixture(t)
	gate, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	// NO KEY UPDATE admits FK KEY SHARE, but stalls deletion's stronger lock.
	if _, err := gate.Exec(`SELECT id FROM exploration_nodes WHERE id=$1 FOR NO KEY UPDATE`, fact); err != nil {
		t.Fatal(err)
	}
	done := startCascade(store, root)
	waitCascadeLock(t, d, "%exploration_nodes%")
	digest, err := store.AddDigest(map[string]any{"body": "concurrent support"}, []int64{fact})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Commit(); err != nil {
		t.Fatal(err)
	}
	cleanup := awaitCascade(t, done)
	if cleanup.Intents != 1 || cleanup.Facts != 0 {
		t.Fatalf("committed covers support lost: cleanup=%+v", cleanup)
	}
	if n, err := store.GetNode(fact); err != nil || n == nil {
		t.Fatalf("covered fact=%+v err=%v", n, err)
	}
	members, err := store.DigestMembers(digest)
	if err != nil || len(members) != 1 || members[0] != fact {
		t.Fatalf("digest members=%v err=%v", members, err)
	}
	t.Logf("covers committed while cascade waited; cleanup=%+v, digest members=%v", cleanup, members)
}

func TestCancelIntentFencesLateDigestAndRollsItBack(t *testing.T) {
	d, store, root, fact := cascadeFixture(t)
	activity, err := store.AppendActivity(Activity{NodeID: &root, Worker: "worker", Kind: "result", Summary: "finished"})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	if _, err := gate.Exec(`SELECT id FROM activity WHERE id=$1 FOR UPDATE`, activity); err != nil {
		t.Fatal(err)
	}
	done := startCascade(store, root)
	waitCascadeLock(t, d, "DELETE FROM activity%")
	digestDone := make(chan error, 1)
	go func() {
		_, err := store.AddDigest(map[string]any{"body": "late support"}, []int64{fact})
		digestDone <- err
	}()
	// The FK check must wait on the candidate node, before any DELETE reaches it.
	waitCascadeLock(t, d, "%INSERT INTO exploration_edges%")
	if err := gate.Commit(); err != nil {
		t.Fatal(err)
	}
	cleanup := awaitCascade(t, done)
	select {
	case err := <-digestDone:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("late digest err=%v, want FK violation 23503", err)
		}
		t.Logf("late AddDigest waited for cascade; cleanup=%+v; SQLSTATE=%s", cleanup, pgErr.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("AddDigest did not finish")
	}
	var digests, edges int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='digest'`, store.expID).Scan(&digests); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1`, store.expID).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if cleanup.Intents != 1 || cleanup.Facts != 1 || digests != 0 || edges != 0 {
		t.Fatalf("partial digest/cascade: cleanup=%+v digests=%d edges=%d", cleanup, digests, edges)
	}
}

func TestCancelIntentRechecksDescendantStateUnderLock(t *testing.T) {
	d, store, root, fact := cascadeFixture(t)
	child := mustIntent(t, store, "new worker")
	mustLink(t, store, fact, RelDerivedFrom, child)
	gate, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	if _, err := gate.Exec(`UPDATE exploration_nodes SET state='running' WHERE id=$1`, child); err != nil {
		t.Fatal(err)
	}
	done := startCascade(store, root)
	waitCascadeLock(t, d, "%exploration_nodes%")
	if err := gate.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if !errors.Is(result.err, ErrIntentStateConflict) || !strings.Contains(result.err.Error(), "running") {
			t.Fatalf("concurrent running descendant: cleanup=%+v err=%v", result.cleanup, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cascade did not finish")
	}
	for _, id := range []int64{root, fact, child} {
		if n, err := store.GetNode(id); err != nil || n == nil {
			t.Fatalf("refused cascade removed node %d: %+v err=%v", id, n, err)
		}
	}
}

func TestCancelIntentPreservesCoveredRunningBranch(t *testing.T) {
	_, store, root, fact := cascadeFixture(t)
	child := mustIntent(t, store, "shared running worker")
	mustLink(t, store, fact, RelDerivedFrom, child)
	if err := store.SetIntentState(child, "running"); err != nil {
		t.Fatal(err)
	}
	digest, err := store.AddDigest(map[string]any{"body": "existing support"}, []int64{fact})
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := store.CancelIntent(root, "done")
	if err != nil || cleanup != (IntentCleanup{Intents: 1}) {
		t.Fatalf("covered running branch: cleanup=%+v err=%v", cleanup, err)
	}
	for _, id := range []int64{fact, child, digest} {
		if n, err := store.GetNode(id); err != nil || n == nil {
			t.Fatalf("covered branch lost node %d: %+v err=%v", id, n, err)
		}
	}
}

func TestCancelIntentPreservesExternalDigestSupport(t *testing.T) {
	d, store, root, fact := cascadeFixture(t)
	other, err := d.CreateExploration("inheriting task", "external digest support")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, other)
	digestStore := d.Exploration(other)
	digest, err := digestStore.AddDigest(map[string]any{"body": "inherited output"}, []int64{fact})
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := store.CancelIntent(root, "done")
	if err != nil || cleanup != (IntentCleanup{Intents: 1}) {
		t.Fatalf("external covers support lost: cleanup=%+v err=%v", cleanup, err)
	}
	members, err := digestStore.DigestMembers(digest)
	if err != nil || len(members) != 1 || members[0] != fact {
		t.Fatalf("external digest members=%v err=%v", members, err)
	}
}
