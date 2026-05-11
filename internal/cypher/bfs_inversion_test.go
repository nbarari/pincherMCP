package cypher

import (
	"context"
	"strings"
	"testing"
	"time"
)

// #426: `MATCH (a)-[*1..3]->(b) WHERE b.name="X"` previously enumerated
// up to 100 a-candidates and ran a recursive CTE per start, fanning out
// 3 hops each — timed out at 10s on a 2k-symbol corpus even when only a
// handful of paths reach b. The planner now inverts to walk inbound
// from the single b-match instead. These tests assert correctness
// (same answer as the un-inverted equivalent) and that the inversion
// gate is conservative.

func TestBFSInversion_EndPredicateInvertsAndFinishesFast(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Chain: caller → middle → target. With many other "caller_*" Functions
	// the un-inverted plan fans out from all of them; the inverted plan
	// walks back from the single target match.
	insertSym(t, db, "target", "Target", "Function", "Go")
	insertSym(t, db, "middle", "Middle", "Function", "Go")
	insertSym(t, db, "caller", "Caller", "Function", "Go")
	insertEdge(t, db, "caller", "middle", "CALLS")
	insertEdge(t, db, "middle", "target", "CALLS")

	// Decoy Functions that have no path to Target — these are what the
	// un-inverted plan would waste cycles BFS'ing through.
	for _, id := range []string{"d1", "d2", "d3"} {
		insertSym(t, db, id, "Decoy"+strings.ToUpper(id), "Function", "Go")
	}

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	start := time.Now()
	res, err := e.Execute(context.Background(),
		`MATCH (a:Function)-[:CALLS*1..3]->(b:Function) WHERE b.name="Target" RETURN a.id`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Errorf("BFS took %v — inversion should keep this under 500ms", elapsed)
	}

	got := make(map[string]bool)
	for _, row := range res.Rows {
		if id, _ := row["a.id"].(string); id != "" {
			got[id] = true
		}
	}
	if len(got) != 2 || !got["caller"] || !got["middle"] {
		t.Errorf("got %v, want exactly {caller, middle}", got)
	}
}

func TestBFSInversion_BothSidesPredicate_NoInvert(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a1", "Alpha", "Function", "Go")
	insertSym(t, db, "b1", "Beta", "Function", "Go")
	insertEdge(t, db, "a1", "b1", "CALLS")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	res, err := e.Execute(context.Background(),
		`MATCH (a:Function)-[:CALLS*1..2]->(b:Function) WHERE a.name="Alpha" AND b.name="Beta" RETURN a.id, b.id`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1; got %v", len(res.Rows), res.Rows)
	}
	if id, _ := res.Rows[0]["a.id"].(string); id != "a1" {
		t.Errorf("a.id = %q, want a1", id)
	}
	if id, _ := res.Rows[0]["b.id"].(string); id != "b1" {
		t.Errorf("b.id = %q, want b1", id)
	}
}

func TestBFSInversion_StartPredicateOnly_NoInvert(t *testing.T) {
	// Symmetric of the bug case — predicate only on fromVar should
	// run the existing outbound plan, unchanged.
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a1", "Alpha", "Function", "Go")
	insertSym(t, db, "b1", "Beta", "Function", "Go")
	insertEdge(t, db, "a1", "b1", "CALLS")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	res, err := e.Execute(context.Background(),
		`MATCH (a:Function)-[:CALLS*1..2]->(b:Function) WHERE a.name="Alpha" RETURN b.id`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1; got %v", len(res.Rows), res.Rows)
	}
	if id, _ := res.Rows[0]["b.id"].(string); id != "b1" {
		t.Errorf("b.id = %q, want b1", id)
	}
}

func TestBFSInversion_AnswerEquivalence(t *testing.T) {
	// Inverted plan must return the same path set as the un-inverted
	// equivalent. Construct a diamond: caller → m1 → target AND
	// caller → m2 → target. Both intermediates should be reported.
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "caller", "Caller", "Function", "Go")
	insertSym(t, db, "m1", "Mid1", "Function", "Go")
	insertSym(t, db, "m2", "Mid2", "Function", "Go")
	insertSym(t, db, "target", "Target", "Function", "Go")
	insertEdge(t, db, "caller", "m1", "CALLS")
	insertEdge(t, db, "caller", "m2", "CALLS")
	insertEdge(t, db, "m1", "target", "CALLS")
	insertEdge(t, db, "m2", "target", "CALLS")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	res, err := e.Execute(context.Background(),
		`MATCH (a:Function)-[:CALLS*1..3]->(b:Function) WHERE b.name="Target" RETURN a.id`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := make(map[string]bool)
	for _, row := range res.Rows {
		if id, _ := row["a.id"].(string); id != "" {
			got[id] = true
		}
	}
	want := map[string]bool{"caller": true, "m1": true, "m2": true}
	for k := range want {
		if !got[k] {
			t.Errorf("missing %q in result; got %v", k, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d distinct results, want %d; got=%v", len(got), len(want), got)
	}
}
