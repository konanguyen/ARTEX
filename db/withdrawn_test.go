package db

import "testing"

func TestOutputWithdrawalsFollowLineageAndDigestCoverage(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exp, err := d.CreateExploration("withdrawal fixture", "retain audit")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	add := func(kind, state string) int64 {
		t.Helper()
		id, err := ts.AddNode(kind, map[string]any{"summary": "fixture"}, 5, state, "fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	link := func(from int64, rel string, to int64) {
		t.Helper()
		if err := ts.Link(from, rel, to); err != nil {
			t.Fatal(err)
		}
	}
	withdrawn := add(KindIntent, "done")
	fact := add(KindFact, "confirmed")
	link(withdrawn, RelYields, fact)
	downstream := add(KindIntent, "done")
	link(fact, RelDerivedFrom, downstream)
	finding := add(KindFinding, "confirmed")
	link(downstream, RelYields, finding)
	// A cycle must neither loop forever nor duplicate a producer's warning.
	link(finding, RelDerivedFrom, downstream)
	normal := add(KindIntent, "done")
	independent := add(KindFact, "confirmed")
	link(normal, RelYields, independent)
	// Multiple producers do not erase the withdrawn producer's provenance.
	link(normal, RelYields, fact)
	digest, err := ts.AddDigest(map[string]any{"body": "retained claim"}, []int64{fact, independent})
	if err != nil {
		t.Fatal(err)
	}
	const reason = "hallucinated evidence"
	if _, err := ts.SoftDeleteIntent(withdrawn, "done", reason); err != nil {
		t.Fatal(err)
	}
	ws, err := ts.OutputWithdrawals()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{withdrawn, fact, downstream, finding, digest} {
		if got := ws[id]; len(got) != 1 || got[0].IntentID != withdrawn || got[0].Reason != reason {
			t.Errorf("node %d withdrawal=%v", id, got)
		}
	}
	for _, id := range []int64{normal, independent} {
		if len(ws[id]) != 0 {
			t.Errorf("independent node %d marked withdrawn: %v", id, ws[id])
		}
	}
	otherExp, err := d.CreateExploration("separate task", "isolation")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, otherExp)
	if got, err := d.Exploration(otherExp).OutputWithdrawals(); err != nil || len(got) != 0 {
		t.Fatalf("withdrawals escaped owning exploration: %v err=%v", got, err)
	}
}
