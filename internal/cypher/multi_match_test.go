package cypher

import (
	"context"
	"strings"
	"testing"
)

// #871: a query with multiple MATCH clauses parsed cleanly — patterns
// past the first appended to q.patterns — but the executor only ran
// q.patterns[0]. Variables introduced by the second/third MATCH never
// got bound, so RETURN columns referencing them (e.g. `c.name`) silently
// projected NULL across every row. Same silent-confidently-wrong
// pattern as #433's chained-edge rejection; same remediation shape.
//
// pinchQL now rejects multi-MATCH explicitly at Execute time, pointing
// at the single-MATCH or client-side-join workarounds.
func TestExecute_MultipleMatchClauses_Rejected(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")
	insertSym(t, db, "b", "B", "Function", "Go")
	insertEdge(t, db, "a", "b", "CALLS")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (a)-[:CALLS]->(b) MATCH (a)-[:READS]->(c) RETURN a.name, c.name`)
	if err == nil {
		t.Fatal("expected error rejecting multi-MATCH; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "multiple MATCH") {
		t.Errorf("error should name 'multiple MATCH'; got %q", msg)
	}
	if !strings.Contains(msg, "single MATCH") && !strings.Contains(msg, "separate queries") {
		t.Errorf("error should point at a remediation (single MATCH or separate queries); got %q", msg)
	}
}

// Control: a single MATCH still runs cleanly.
func TestExecute_SingleMatchClause_Runs(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")
	insertSym(t, db, "b", "B", "Function", "Go")
	insertEdge(t, db, "a", "b", "CALLS")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (a)-[:CALLS]->(b) RETURN a.name, b.name`)
	if err != nil {
		t.Fatalf("single-MATCH Execute: %v", err)
	}
	if r.Total != 1 {
		t.Errorf("expected 1 row for the A→B edge; got %d", r.Total)
	}
}
