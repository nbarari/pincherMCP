package cypher

import (
	"context"
	"testing"
)

// #1155: aggregate mixed with non-aggregate column in RETURN. pinchQL
// applies implicit GROUP BY on the bare columns (#348/#432), so
// `count(a) AS total, a.name` returns per-name counts (typically 1)
// rather than the overall total the alias suggests. Same silent-
// confidently-wrong family as #1122 (ORDER BY aggregate without
// projection aggregate) and #1135 (bare RETURN property).
//
// Tests follow the four-case shape from #1152: positive + negative
// + control + known-good cross-check (the 41-shape suite must still
// emit zero warnings unless it explicitly exercises this case).

// Positive: count + bare column trips the warning.
func TestExecute_CountWithBareColumn_Warns(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")
	insertSym(t, db, "g2", "g", "Function", "Go")
	insertSym(t, db, "g3", "h", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (a:Function) RETURN count(a) AS total, a.name LIMIT 5`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasWarning(r.Warnings, "implicit GROUP BY", "per-group") {
		t.Errorf("expected implicit-GROUP-BY warning naming per-group semantics; got %v", r.Warnings)
	}
	// The warning must reference the bare column the user wrote so they
	// can see WHICH column triggered the grouping.
	if !hasWarning(r.Warnings, "a.name") {
		t.Errorf("warning should name the bare column triggering implicit GROUP BY; got %v", r.Warnings)
	}
}

// Positive: avg(numeric) + bare column also trips.
func TestExecute_AvgWithBareColumn_Warns(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")
	insertSym(t, db, "g2", "g", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN avg(n.complexity) AS mean, n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasWarning(r.Warnings, "implicit GROUP BY") {
		t.Errorf("expected implicit-GROUP-BY warning for avg+bare; got %v", r.Warnings)
	}
}

// Negative (control): pure aggregate — no bare column, no warning.
// This is the well-formed "overall count" shape and must stay silent.
func TestExecute_PureAggregate_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")
	insertSym(t, db, "g2", "g", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (a:Function) RETURN count(a) AS total`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if containsCIAggMixed(w, "implicit GROUP BY") {
			t.Errorf("pure aggregate should not trip #1155; got %v", r.Warnings)
		}
	}
}

// Negative (control): pure bare columns — no aggregate, no warning.
func TestExecute_PureBareColumns_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (a:Function) RETURN a.name, a.language`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if containsCIAggMixed(w, "implicit GROUP BY") {
			t.Errorf("pure bare columns should not trip #1155; got %v", r.Warnings)
		}
	}
}

// Negative (control): single RETURN column — too few entries to mix.
func TestExecute_SingleReturnCol_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (a:Function) RETURN a.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if containsCIAggMixed(w, "implicit GROUP BY") {
			t.Errorf("single bare column should not trip #1155; got %v", r.Warnings)
		}
	}
}

// Negative (control): DISTINCT + bare + aggregate is the canonical
// grouped-count shape — explicitly per-group, must stay silent. This
// is the case the known-good suite caught when #1155 first shipped.
func TestExecute_DistinctGroupedAggregate_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")
	insertSym(t, db, "p1", "f", "Function", "Python")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN DISTINCT n.language, COUNT(*) ORDER BY COUNT(*) DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if containsCIAggMixed(w, "implicit GROUP BY") {
			t.Errorf("DISTINCT grouped aggregate should not trip #1155; got %v", r.Warnings)
		}
	}
}

// Negative (control): ORDER BY on the aggregate signals intentional
// per-group consumption — silent.
func TestExecute_OrderByAggregateOverBare_NoWarning(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")
	insertSym(t, db, "p1", "f", "Function", "Python")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.language, COUNT(*) ORDER BY COUNT(*) DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, w := range r.Warnings {
		if containsCIAggMixed(w, "implicit GROUP BY") {
			t.Errorf("ORDER-BY-on-aggregate should not trip #1155; got %v", r.Warnings)
		}
	}
}

// Cross-check: warning text round-trips through formatAggExpr +
// formatBareCol — the surfaced strings must match user-typed shapes.
func TestExecute_AggMixedWarning_RoundTripsTypedShape(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "f", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN sum(n.start_line) AS s, n.language`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Warning text must mention "sum(n.start_line)" — formatted lower-
	// case, with the variable.property shape — and "n.language" as the
	// implicit-group column. Brittle "exact string" matching would
	// regress on cosmetic tweaks; assert the structural pieces.
	if !hasWarning(r.Warnings, "sum(n.start_line)") {
		t.Errorf("warning should echo the aggregate expression %q; got %v",
			"sum(n.start_line)", r.Warnings)
	}
	if !hasWarning(r.Warnings, "n.language") {
		t.Errorf("warning should echo the bare column %q; got %v",
			"n.language", r.Warnings)
	}
}

func containsCIAggMixed(s, frag string) bool {
	return hasWarning([]string{s}, frag)
}
