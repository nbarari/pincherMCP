package cypher

import (
	"context"
	"strings"
	"testing"
)

// #900: max_rows must be a hard upper bound on the result set, not
// a scan-headroom hint. Pre-fix `RETURN n.name LIMIT 99999999` with
// `max_rows=2` returned 4 rows (2× via scanLimitFor), because the
// post-scan trim used only the pinchQL LIMIT — not the MCP arg.
// Now Execute clamps to max_rows + emits a warning.

func TestExecute_MaxRowsClampsHighPinchQLLimit(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	// Seed 20 functions.
	for i := 0; i < 20; i++ {
		insertSym(t, db, sym(i), sym(i), "Function", "Go")
	}

	e := &Executor{DB: db, MaxRows: 5, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name LIMIT 99999999`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Total != 5 {
		t.Errorf("max_rows=5 must cap the result to 5 rows; got %d", r.Total)
	}
	if len(r.Rows) != 5 {
		t.Errorf("expected 5 rows in slice; got %d", len(r.Rows))
	}
	if !hasMaxRowsTrimWarning(r.Warnings) {
		t.Errorf("expected a max_rows-trim warning when LIMIT > max_rows; got %v", r.Warnings)
	}
}

// When the pinchQL LIMIT is within max_rows, no trim, no warning.
func TestExecute_MaxRowsNoTrimWhenLimitIsLower(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	for i := 0; i < 20; i++ {
		insertSym(t, db, sym(i), sym(i), "Function", "Go")
	}

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name LIMIT 3`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Total != 3 {
		t.Errorf("LIMIT 3 with max_rows=100 should return 3 rows; got %d", r.Total)
	}
	if hasMaxRowsTrimWarning(r.Warnings) {
		t.Errorf("no max_rows-trim warning when LIMIT < max_rows; got %v", r.Warnings)
	}
}

// Aggregating queries (COUNT) skip the trim — their row count is the
// cardinality, not a sample.
func TestExecute_MaxRowsDoesNotTrimAggregateRows(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	for i := 0; i < 20; i++ {
		insertSym(t, db, sym(i), sym(i), "Function", "Go")
	}
	// 5 Methods, distinct kinds yielding distinct GROUP BY rows.
	for i := 0; i < 5; i++ {
		insertSym(t, db, "m"+sym(i), "M"+sym(i), "Method", "Go")
	}

	e := &Executor{DB: db, MaxRows: 1, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n) RETURN n.kind, COUNT(n)`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// With kinds Function + Method we expect 2 group rows. The trim
	// must NOT clamp this to 1 just because max_rows=1 — that would
	// lose cardinality information.
	if r.Total != 2 {
		t.Errorf("aggregate RETURN must not be trimmed by max_rows; expected 2 group rows, got %d (rows=%v)", r.Total, r.Rows)
	}
	if hasMaxRowsTrimWarning(r.Warnings) {
		t.Errorf("aggregate queries must not emit the max_rows-trim warning; got %v", r.Warnings)
	}
}

func sym(i int) string {
	return "sym_" + string(rune('a'+i%26)) + "_" + string(rune('0'+i/26))
}

func hasMaxRowsTrimWarning(warnings []string) bool {
	for _, w := range warnings {
		if strings.Contains(w, "max_rows=") && strings.Contains(w, "trimmed") {
			return true
		}
	}
	return false
}
