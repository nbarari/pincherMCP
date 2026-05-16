package cypher

import (
	"context"
	"strings"
	"testing"
)

// #1118: Cypher's comma-separated patterns in MATCH (`MATCH (a), (b)
// WHERE ...`) are syntax for joining independent patterns. pinchQL
// only supports one pattern per MATCH (and one MATCH per query, #871).
// Pre-fix the comma got the generic "unexpected token ','" error
// pointing at WHERE/RETURN, which reads as a syntax bug; the real
// story is the multi-pattern shape isn't supported. New error names
// the coverage gap + points at the workaround (two query calls, or
// the edge form).

func TestExecute_CommaSeparatedPatterns_RejectedWithCoverageGapMessage(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (a:Function), (b:Function) WHERE a.name="x" AND b.name="y" RETURN a.name, b.name`)
	if err == nil {
		t.Fatal("comma-separated patterns must be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "comma-separated patterns") {
		t.Errorf("error must name the comma-pattern shape; got %q", msg)
	}
	if !strings.Contains(msg, "not supported") {
		t.Errorf("error must say not supported; got %q", msg)
	}
	// Must point at the workaround (two query calls OR the edge form).
	if !strings.Contains(msg, "edge form") && !strings.Contains(msg, "separate query") {
		t.Errorf("error must name a workaround; got %q", msg)
	}
}
