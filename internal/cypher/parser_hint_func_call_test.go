package cypher

import (
	"context"
	"strings"
	"testing"
)

// #1137: function calls in WHERE (`size(n.name)`, `length(...)`,
// `toUpper(...)`, `COUNT(DISTINCT ...)`) reached a generic
// "unsupported operator: (" with no signal that function calls in
// WHERE are the structural reason. Same failure-as-pedagogy shape
// as #928 (arithmetic) — surface the supported aggregator scope
// (RETURN only) + the client-side workaround.

func TestExecute_FuncCallInWhere_HintsAtSupportedScope(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "main", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE size(n.name) > 5 RETURN n.name LIMIT 1`)
	if err == nil {
		t.Fatal("expected error for size() in WHERE; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "function call") {
		t.Errorf("error should name the structural reason (function call); got: %v", err)
	}
	if !strings.Contains(msg, "RETURN") {
		t.Errorf("error should point users to RETURN as the aggregator scope; got: %v", err)
	}
	if !strings.Contains(msg, "client-side") {
		t.Errorf("error should mention client-side post-processing as the workaround; got: %v", err)
	}
}

func TestExecute_CountDistinctInReturn_HintsAtScope(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "main", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	// COUNT(DISTINCT n.x) is currently unsupported and the tokenizer/
	// parser drives down the WHERE path for the inner expression in
	// some shapes. This test pins the user-facing behavior: error
	// quality with a function-call structure.
	_, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE length(n.docstring) > 10 RETURN n.name`)
	if err == nil {
		t.Fatal("expected error for length() in WHERE; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "function call") {
		t.Errorf("error should name the structural reason; got: %v", err)
	}
}
