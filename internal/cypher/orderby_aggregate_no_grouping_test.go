package cypher

import (
	"context"
	"testing"
)

// #1122: ORDER BY references an aggregate (COUNT/SUM/AVG/MIN/MAX) but
// the projection has no aggregate, so there's no grouping context.
// Pre-fix the sort silently no-op'd: ORDER BY COUNT(*) evaluated to
// one value across the whole match set, the sort had nothing to
// order, results came back in scan order. Same silent-confidently-
// wrong family as #1120 (asterisk-as-HOPS) — and the
// collectUnknownOrderByWarnings detector explicitly skipped aggregate
// targets, so this gap had no detector before #1122.

// Positive: ORDER BY COUNT(*) with no aggregate in RETURN — warning.
func TestExecute_OrderByCountStarNoProjection_Warns(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")
	insertSym(t, db, "g2", "g", "Function", "Go")
	insertSym(t, db, "p1", "f", "Function", "Python")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.language ORDER BY COUNT(*) DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasWarning(r.Warnings, "ORDER BY", "no grouping context", "scan order") {
		t.Errorf("expected ORDER-BY-aggregate-without-grouping warning naming the silent no-op; got %v", r.Warnings)
	}
	// Remediation must compile: it should reference the same aggregate.
	if !hasWarning(r.Warnings, "COUNT(*)") {
		t.Errorf("warning should include the aggregate name in the remediation example; got %v", r.Warnings)
	}
}

// Positive: same for SUM.
func TestExecute_OrderBySumNoProjection_Warns(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")
	insertSym(t, db, "g2", "g", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name ORDER BY SUM(n.complexity) DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasWarning(r.Warnings, "ORDER BY", "no grouping context") {
		t.Errorf("expected warning for ORDER BY SUM with no aggregate in RETURN; got %v", r.Warnings)
	}
}

// Control: ORDER BY aggregate WITH matching aggregate in RETURN —
// silent (no warning). This is the well-formed grouped query shape
// that #1120 made actually-sort.
func TestExecute_OrderByCountStar_WithProjection_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")
	insertSym(t, db, "g2", "g", "Function", "Go")
	insertSym(t, db, "p1", "f", "Function", "Python")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.language, COUNT(*) ORDER BY COUNT(*) DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if containsCI(w, "no grouping context") {
			t.Errorf("known-good grouped query should not trip the #1122 warning; got %v", r.Warnings)
		}
	}
}

// Control: ORDER BY on a regular column with no aggregate anywhere —
// silent.
func TestExecute_OrderByPlainColumn_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")
	insertSym(t, db, "g2", "g", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name ORDER BY n.name DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if containsCI(w, "no grouping context") {
			t.Errorf("plain-column ORDER BY should not trip #1122; got %v", r.Warnings)
		}
	}
}

// Control: no ORDER BY at all — silent.
func TestExecute_NoOrderBy_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if containsCI(w, "no grouping context") {
			t.Errorf("no-ORDER-BY query should not trip #1122; got %v", r.Warnings)
		}
	}
}

// containsCI is a tiny case-insensitive substring check; hasWarning's
// internal helper isn't exported but the same shape is fine inline.
func containsCI(s, frag string) bool {
	return hasWarning([]string{s}, frag)
}
