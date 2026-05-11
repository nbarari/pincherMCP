package cypher

import (
	"context"
	"strings"
	"testing"
)

// #433: previously these two query shapes parsed silently and
// returned garbage. Now they error explicitly so the agent knows
// the query shape isn't supported instead of trusting a result
// that can't be right.

func TestExecute_RejectsWITHClause(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Alpha", "Function", "Go")

	exe := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := exe.Execute(context.Background(),
		`MATCH (n:Function) WITH n WHERE n.complexity > 0 RETURN n.id`)
	if err == nil {
		t.Fatal("expected error for WITH clause, got nil")
	}
	if !strings.Contains(err.Error(), "WITH clause is not supported") {
		t.Errorf("expected explicit WITH-rejected error, got: %v", err)
	}
}

func TestExecute_RejectsChainedEdgePattern(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Alpha", "Function", "Go")

	exe := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := exe.Execute(context.Background(),
		`MATCH (a)-[:CALLS]->(b)-[:CALLS]->(c) WHERE a.name="Alpha" RETURN c.name`)
	if err == nil {
		t.Fatal("expected error for chained-edge pattern, got nil")
	}
	if !strings.Contains(err.Error(), "chained edge patterns") {
		t.Errorf("expected chained-edge rejection error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "*2..2") {
		t.Errorf("error should point at variable-length workaround, got: %v", err)
	}
}

// Sanity: variable-length patterns and single-hop patterns still
// parse and run. The new rejection only fires for chained literal
// edges, not for the legitimate forms.
func TestExecute_VariableLengthStillWorks(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")
	insertSym(t, db, "b", "B", "Function", "Go")
	insertEdge(t, db, "a", "b", "CALLS")

	r := exec(t, db, `MATCH (a)-[:CALLS*1..2]->(b) WHERE a.name="A" RETURN b.name`)
	if r.Total != 1 {
		t.Fatalf("variable-length pattern should still match, got %d rows", r.Total)
	}
}
