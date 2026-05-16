package cypher

import (
	"context"
	"testing"
)

// #1119: pre-fix, inline brace props with unknown keys (typo'd prop
// name like `{nme: "main"}`) were silently dropped — the filter never
// reached SQL and the query returned all rows. The warning text said
// "treated as undefined (always false in comparisons)" but the actual
// behavior was "ignore the predicate entirely", contradicting the
// warning. Now: unknown inline prop generates `AND 1=0` so the filter
// structurally evaluates to false, matching the contract: 0 rows.

func TestExecute_InlinePropUnknownKey_ReturnsZeroRows(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "main", "Function", "Go")
	insertSym(t, db, "b", "other", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function {nme: "main"}) RETURN n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 0 {
		t.Errorf("unknown inline prop should yield 0 rows (always-false contract); got %d rows: %v", len(r.Rows), r.Rows)
	}
	// The warning should still surface so the user learns the typo.
	if !hasWarning(r.Warnings, "nme") {
		t.Errorf("expected unknown-prop warning naming nme; got %v", r.Warnings)
	}
}

// Control: a valid inline prop still filters correctly.
func TestExecute_InlinePropValid_FiltersAsExpected(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "main", "Function", "Go")
	insertSym(t, db, "b", "other", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function {name: "main"}) RETURN n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 1 {
		t.Errorf("valid inline prop should filter to matching row; got %d: %v", len(r.Rows), r.Rows)
	}
}
