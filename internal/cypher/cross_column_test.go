package cypher

import (
	"context"
	"strings"
	"testing"
)

// #593 + #1217 v0.66 DOGFOOD: column-vs-column comparisons
// (`WHERE a.col <op> b.col`) are unsupported in pinchQL. The
// contract is:
//
//   - Surface a warning naming the offending clause so the agent
//     can rewrite or post-filter.
//   - The predicate is DROPPED (treated as always-true), so the
//     surrounding WHERE clauses still apply. Other rows are not
//     dropped just because one predicate couldn't be evaluated.
//
// Pre-#1217 the predicate was substituted with `false`. That
// turned `WHERE x AND y` (where y was the unsupported predicate)
// into `WHERE x AND false → 0 rows` — silently dropping every
// row that x would have matched. Same silent-confidently-wrong
// family as v0.59's drain. The "ignored" framing in the warning
// must mean stripped (no-op), not "evaluates to false".

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

	// #1217 v0.66 DOGFOOD: predicate is DROPPED, not substituted
	// with false. The CALLS edge matches; the unsupported predicate
	// is ignored. Pre-#1217 this returned 0 rows because the
	// predicate substitution killed the conjunction.
	if r.Total != 1 {
		t.Errorf("cross-column predicate should be dropped, not substitute with false (#1217); got %d rows: %v",
			r.Total, r.Rows)
	}
}

// #1217 v0.66 DOGFOOD: when an unsupported predicate is ANDed
// with a supported predicate, the supported one must still
// filter. Pre-fix the substitution-with-false short-circuited
// the AND to false, dropping rows the supported predicate would
// have matched.
func TestExecute_CrossColumnComparison_AndedWithSupported_KeepsSupportedFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Two CALLS edges:
	//   a (Go)     → b (Python)
	//   c (Rust)   → d (Go)
	insertSym(t, db, "a", "A", "Function", "Go")
	insertSym(t, db, "b", "B", "Function", "Python")
	insertSym(t, db, "c", "C", "Function", "Rust")
	insertSym(t, db, "d", "D", "Function", "Go")
	insertEdge(t, db, "a", "b", "CALLS")
	insertEdge(t, db, "c", "d", "CALLS")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	// `a.language = "Go"` is supported and matches 1 row (a→b).
	// `a.language <> b.language` is unsupported → dropped.
	// Final filter: a.language="Go" only → 1 row.
	r, err := e.Execute(context.Background(),
		`MATCH (a)-[:CALLS]->(b) WHERE a.language = "Go" AND a.language <> b.language RETURN a.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Total != 1 {
		t.Errorf("supported predicate (a.language='Go') should still filter to 1 row when ANDed with dropped predicate; got %d rows: %v",
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
