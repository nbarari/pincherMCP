package cypher

import (
	"context"
	"strings"
	"testing"
)

// #1116: WHERE / RETURN references to an unbound variable (typo of the
// MATCH pattern's variable name) silently evaluated to NULL/always-
// false — the predicate was effectively ignored and rows passed
// through unfiltered. Same silent-confidently-wrong shape as #473
// (unknown property), but at the variable scope. Now: a warning fires
// naming the unknown variable + the bound-variable list.

func TestExecute_UnknownVariableInWhere_SurfacesWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")
	insertSym(t, db, "b", "B", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	// `m` is not bound — the MATCH only declares `n`. Pre-fix the
	// predicate evaluated to NULL and returned both rows; post-fix the
	// warning surfaces the typo.
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE m.name = "A" RETURN n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasWarning(r.Warnings, "m", "not bound") {
		t.Errorf("expected unknown-variable warning naming m; got: %v", r.Warnings)
	}
	if !hasWarning(r.Warnings, "Bound variables") {
		t.Errorf("warning should list bound variables; got: %v", r.Warnings)
	}
}

func TestExecute_UnknownVariableInReturn_SurfacesWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.name = "A" RETURN x.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasWarning(r.Warnings, "x", "not bound") {
		t.Errorf("expected unknown-variable warning naming x in RETURN; got: %v", r.Warnings)
	}
}

// Control: a well-formed query uses only bound variables — no
// unknown-variable warning fires.
func TestExecute_BoundVariablesOnly_NoUnknownVariableWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.name = "A" RETURN n.file_path`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "not bound") {
			t.Errorf("well-formed query must not trip unknown-variable warning; got %q", w)
		}
	}
}

// Edge variables count as bound. A WHERE on an edge var with a
// recognized edge property should pass without the unknown-var
// warning firing.
func TestExecute_EdgeVariable_DoesNotTripUnknownVariableWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")
	insertSym(t, db, "b", "B", "Function", "Go")
	insertEdge(t, db, "a", "b", "CALLS")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (a)-[r:CALLS]->(b) WHERE r.confidence > 0.5 RETURN a.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "not bound") {
			t.Errorf("edge variable r should be bound; got unknown-variable warning %q", w)
		}
	}
}
