package cypher

import (
	"context"
	"strings"
	"testing"
)

// BETWEEN x AND y is the SQL/Neo4j range-membership spelling agents
// reach for by muscle memory. pinchQL doesn't have a BETWEEN keyword;
// pre-hint the parser fell through to a bare "unsupported operator:
// BETWEEN" that left the caller no path forward. operatorHint now
// maps BETWEEN to the two-ANDed-comparisons workaround.

func TestOperatorHint_Between_TeachesWorkaround(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	q := `MATCH (n:Function) WHERE n.start_line BETWEEN 100 AND 200 RETURN n.name`
	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(), q)
	if err == nil {
		t.Fatal("expected error on BETWEEN predicate; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "BETWEEN") {
		t.Errorf("error should name BETWEEN; got %q", msg)
	}
	if !strings.Contains(msg, "AND") || !strings.Contains(msg, ">=") {
		t.Errorf("error should show the AND-of-comparisons workaround; got %q", msg)
	}
}
