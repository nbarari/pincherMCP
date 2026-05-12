package cypher

import (
	"context"
	"strings"
	"testing"
)

// #612: when the user references an unknown property on an EDGE
// variable (the `r` in `[r:CALLS]`), the warning previously listed
// SYMBOL properties — useless misdirection. Test that:
//   1. unknown edge property → warning lists edge properties
//   2. unknown node property → warning still lists node properties
//   3. known edge properties (kind, confidence) produce no warning

func TestExecute_UnknownEdgePropertyWarnsWithEdgePropsList(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language, start_byte, end_byte, start_line, end_line) VALUES
		('a','proj1','f.go','A','A','Function','Go', 0,10,1,2),
		('b','proj1','f.go','B','B','Function','Go', 11,20,3,4)`); err != nil {
		t.Fatalf("seed symbols: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO edges(project_id, from_id, to_id, kind, confidence) VALUES
		('proj1','a','b','CALLS', 0.9)`); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function)-[r:CALLS]->(m) WHERE r.source = "x" RETURN n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Warnings) == 0 {
		t.Fatalf("expected an unknown-property warning for r.source; got none")
	}
	w := r.Warnings[0]
	if !strings.Contains(w, "source") {
		t.Errorf("warning must name the offending property; got %q", w)
	}
	if !strings.Contains(w, "edge") {
		t.Errorf("warning must say `edge` so the user knows the variable kind; got %q", w)
	}
	if !strings.Contains(w, "confidence") {
		t.Errorf("warning must list edge property `confidence`; got %q", w)
	}
	// Pre-#612 regression: must NOT advertise symbol-only properties.
	if strings.Contains(w, "qualified_name") || strings.Contains(w, "is_exported") {
		t.Errorf("edge-property warning leaked symbol props (pre-#612 bug); got %q", w)
	}
}

func TestExecute_UnknownNodePropertyStillWarnsWithNodePropsList(t *testing.T) {
	// Sanity: node-variable warnings unchanged. Pre-#612 fix shouldn't
	// have over-broadened.
	db := newTestDB(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language, start_byte, end_byte, start_line, end_line) VALUES
		('a','proj1','f.go','A','A','Function','Go', 0,10,1,2)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.bogus = "x" RETURN n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Warnings) == 0 {
		t.Fatal("expected node-property warning; got none")
	}
	w := r.Warnings[0]
	if !strings.Contains(w, "node") {
		t.Errorf("warning must say `node`; got %q", w)
	}
	// Must list real node props (qualified_name is canonical).
	if !strings.Contains(w, "qualified_name") {
		t.Errorf("node-property warning must list qualified_name; got %q", w)
	}
}

func TestExecute_KnownEdgePropertiesProduceNoWarning(t *testing.T) {
	// Negative: r.kind and r.confidence are recognized; no warning.
	db := newTestDB(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language, start_byte, end_byte, start_line, end_line) VALUES
		('a','proj1','f.go','A','A','Function','Go', 0,10,1,2),
		('b','proj1','f.go','B','B','Function','Go', 11,20,3,4)`); err != nil {
		t.Fatalf("seed symbols: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO edges(project_id, from_id, to_id, kind, confidence) VALUES
		('proj1','a','b','CALLS', 0.9)`); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function)-[r:CALLS]->(m) RETURN n.name, r.kind, r.confidence`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("known edge properties should produce no warnings; got %v", r.Warnings)
	}
}
