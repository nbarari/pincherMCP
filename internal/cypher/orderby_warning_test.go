package cypher

import (
	"context"
	"strings"
	"testing"
)

// #881: ORDER BY on an unrecognised column was silently dropped —
// orderByCol / joinOrderByCol both return "" for an unknown property,
// so the SQL never emits an ORDER BY clause and results come back in
// scan order while the caller thinks they're sorted. Same silent-
// confidently-wrong class as #473 (unknown WHERE property), but the
// WHERE-side warning didn't walk ORDER BY. Now it does.

func TestExecute_OrderByUnknownProperty_SurfacesWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")
	insertSym(t, db, "b", "B", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name ORDER BY n.bogus_field DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var saw bool
	for _, w := range r.Warnings {
		if strings.Contains(w, "ORDER BY") && strings.Contains(w, "n.bogus_field") &&
			strings.Contains(w, "silently dropped") {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("expected ORDER BY unknown-property warning naming n.bogus_field; got: %v", r.Warnings)
	}
}

// Control: a valid ORDER BY column produces no warning.
func TestExecute_OrderByKnownProperty_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name ORDER BY n.name DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "ORDER BY") {
			t.Errorf("valid ORDER BY n.name must not warn; got: %v", w)
		}
	}
}

// Aggregate ORDER BY targets are not whitelist-checked.
func TestExecute_OrderByAggregate_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.language, COUNT(n) ORDER BY COUNT(n) DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "ORDER BY") {
			t.Errorf("aggregate ORDER BY must not warn; got: %v", w)
		}
	}
}
