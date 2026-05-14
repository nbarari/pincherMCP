package cypher

import (
	"strings"
	"testing"
)

// #744: a node label that IS a valid kind but doesn't match the kind
// of the WHERE-named symbol is a silent-zero. `MATCH (n:Function) WHERE
// n.name = "X"` returns 0 rows when X is a Method — :Function is a
// valid kind value (so the enum-value check stays quiet) and `name` is
// a valid property (so the property check stays quiet). The engine now
// emits a warning naming the actual kind.
func TestExecute_KindLabelMismatch_Warns(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	// "handler" exists only as a Method.
	insertSym(t, db, "m1", "handler", "Method", "Go")

	r := exec(t, db, `MATCH (n:Function) WHERE n.name = "handler" RETURN n.name`)
	if len(r.Rows) != 0 {
		t.Fatalf("rows=%d, want 0 (handler is a Method, not a Function)", len(r.Rows))
	}
	if len(r.Warnings) == 0 {
		t.Fatal("Warnings empty; expected a kind-label-mismatch advisory")
	}
	w := strings.Join(r.Warnings, " | ")
	if !strings.Contains(w, "Function") || !strings.Contains(w, "handler") || !strings.Contains(w, "Method") {
		t.Errorf("warning must name the label, the symbol, and the actual kind; got %q", w)
	}
}

// When the label DOES match one of the symbol's kinds, no warning —
// the zero rows are caused by something else (another predicate).
func TestExecute_KindLabelMatches_NoMismatchWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "realFn", "Function", "Go")

	// Label matches; the empty result is due to the second predicate.
	r := exec(t, db, `MATCH (n:Function) WHERE n.name = "realFn" AND n.language = "Rust" RETURN n.name`)
	if len(r.Rows) != 0 {
		t.Fatalf("rows=%d, want 0 (no Rust symbol)", len(r.Rows))
	}
	for _, warn := range r.Warnings {
		if strings.Contains(warn, "matched 0 nodes named") {
			t.Errorf("no kind-label-mismatch warning expected when label matches; got %q", warn)
		}
	}
}

// A name that doesn't exist at all under any kind must NOT produce a
// kind-mismatch warning — that's a genuine "no such symbol", not a
// label problem.
func TestExecute_KindLabelMismatch_UnknownNameNoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "realFn", "Function", "Go")

	r := exec(t, db, `MATCH (n:Function) WHERE n.name = "ZZZNoSuchSymbol" RETURN n.name`)
	if len(r.Rows) != 0 {
		t.Fatalf("rows=%d, want 0", len(r.Rows))
	}
	for _, warn := range r.Warnings {
		if strings.Contains(warn, "matched 0 nodes named") {
			t.Errorf("no kind-label-mismatch warning expected for a wholly-unknown name; got %q", warn)
		}
	}
}

// Non-empty results never trigger the warning (the gate is res.Total == 0).
func TestExecute_KindLabelMismatch_NonEmptyResultNoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "realFn", "Function", "Go")

	r := exec(t, db, `MATCH (n:Function) WHERE n.name = "realFn" RETURN n.name`)
	if len(r.Rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(r.Rows))
	}
	for _, warn := range r.Warnings {
		if strings.Contains(warn, "matched 0 nodes named") {
			t.Errorf("no kind-label-mismatch warning expected on a non-empty result; got %q", warn)
		}
	}
}
