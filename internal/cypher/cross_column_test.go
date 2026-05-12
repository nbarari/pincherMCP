package cypher

import (
	"context"
	"strings"
	"testing"
)

// #593: pre-fix, `WHERE a.col <op> b.col` (column-vs-column
// comparison) silently parsed and evaluated to true — RHS treated as
// an unmatched literal — so the predicate inflated result sets
// instead of filtering. Same UX class as #473 (typo'd properties)
// and #578 (unknown function names): malformed input silently
// returns an answer that isn't.
//
// The fix surfaces a warning naming the offending clause + makes
// evaluation return false (consistent with #473's "unknown property
// → 0 rows + warning" handling) so callers see the predicate isn't
// being honored.

func TestExecute_CrossColumnComparison_SurfacesWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	insertSym(t, db, "a", "A", "Function", "Go")
	insertSym(t, db, "b", "B", "Function", "Python")
	insertEdge(t, db, "a", "b", "CALLS")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (a)-[:CALLS]->(b) WHERE a.language <> b.language RETURN a.name, b.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Warning surfaced — names the clause so the agent can rewrite.
	var saw bool
	for _, w := range r.Warnings {
		if strings.Contains(w, "column-vs-column") &&
			strings.Contains(w, "a.language") &&
			strings.Contains(w, "b.language") {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("expected column-vs-column warning naming a.language and b.language; got: %v", r.Warnings)
	}

	// Predicate ignored → 0 rows (NOT inflated). Pre-fix this returned
	// 1 row because the predicate silently evaluated to always-true.
	if r.Total != 0 {
		t.Errorf("cross-column predicate should filter to 0 rows (consistent with #473); got %d rows: %v",
			r.Total, r.Rows)
	}
}

// Sanity: literal-RHS comparisons unchanged.
func TestExecute_LiteralRHSComparison_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, _ := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.language <> "Python" RETURN n.name`)

	for _, w := range r.Warnings {
		if strings.Contains(w, "column-vs-column") {
			t.Errorf("literal-RHS comparison wrongly flagged: %v", w)
		}
	}
	if r.Total != 1 {
		t.Errorf("expected 1 row (Go function != Python); got %d", r.Total)
	}
}
