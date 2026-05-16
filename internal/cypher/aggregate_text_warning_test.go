package cypher

import (
	"context"
	"strings"
	"testing"
)

// #1108: MIN/MAX/SUM/AVG on text/bool columns silently returns null —
// computeAgg parses each value as float64 and skips non-numeric rows,
// so an all-text column yields nums=[] → nil. SQLite's MAX/MIN actually
// work lexicographically on text, so the behavior diverges silently;
// the agent reads "MAX(n.name): null" as "no rows match" when the real
// cause is the aggregator/column-type mismatch.

func TestExecute_MaxOnTextColumn_SurfacesWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN MAX(n.name)`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasWarning(r.Warnings, "MAX", "text") {
		t.Errorf("expected aggregate-type-mismatch warning naming MAX + text; got: %v", r.Warnings)
	}
	// The warning should suggest COUNT or a numeric column.
	matched := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "COUNT") || strings.Contains(w, "complexity") || strings.Contains(w, "numeric") {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("warning should suggest COUNT or a numeric column; got: %v", r.Warnings)
	}
}

func TestExecute_MinOnTextColumn_SurfacesWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN MIN(n.file_path)`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasWarning(r.Warnings, "MIN", "text") {
		t.Errorf("expected aggregate-type-mismatch warning naming MIN + text; got: %v", r.Warnings)
	}
}

func TestExecute_SumOnBoolColumn_SurfacesWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN SUM(n.is_exported)`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasWarning(r.Warnings, "SUM", "bool") {
		t.Errorf("expected aggregate-type-mismatch warning naming SUM + bool; got: %v", r.Warnings)
	}
}

// Control: SUM/AVG/MIN/MAX on a numeric column produces NO warning.
func TestExecute_SumOnIntColumn_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN SUM(n.complexity)`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "MIN/MAX/SUM/AVG") || strings.Contains(w, "aggregator on") {
			t.Errorf("numeric SUM must not trip aggregate-type-mismatch warning; got: %q", w)
		}
	}
}

// Control: COUNT on a text column is the correct usage — must NOT
// trip the warning. COUNT works on any type.
func TestExecute_CountOnTextColumn_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "A", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN COUNT(n.name)`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "aggregator on") {
			t.Errorf("COUNT on text must not trip aggregate-type-mismatch warning; got: %q", w)
		}
	}
}
