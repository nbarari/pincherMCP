package cypher

import (
	"context"
	"strings"
	"testing"
)

// #916: pre-fix `(a)-[r:CALLS]-(b)` parsed cleanly but the executor
// only consulted outbound edges, so Cypher's undirected semantics
// (match either direction) silently produced only half the rows.
// pinchQL now rejects the syntax with a remediation pointing at the
// directed forms or a client-side union.

func TestExecute_UndirectedEdge_Rejected(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "Open", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (a)-[r:CALLS]-(b) WHERE a.name = "Open" RETURN a.name, b.name`)
	if err == nil {
		t.Fatal("undirected edge must produce a parse error; got success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "undirected edges") {
		t.Errorf("error message must explain the issue; got %q", msg)
	}
	if !strings.Contains(msg, "-[r:KIND]->") || !strings.Contains(msg, "<-[r:KIND]-") {
		t.Errorf("error message must name the directed-form remediations; got %q", msg)
	}
}

// Directed edges still work — both arrow forms.
func TestExecute_DirectedEdge_OutboundStillParses(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "Open", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (a)-[r:CALLS]->(b) WHERE a.name = "Open" RETURN a.name, b.name`)
	if err != nil {
		t.Errorf("directed outbound must still parse; got %v", err)
	}
}

// Bare-edge form (no explicit kind) with directed arrow still works.
func TestExecute_BareDirectedEdge_StillParses(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (a)-->(b) WHERE a.name = "A" RETURN a.name`)
	if err != nil {
		t.Errorf("bare directed edge must still parse; got %v", err)
	}
}
