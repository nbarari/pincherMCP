package cypher

import (
	"testing"
)

// #358: WHERE ... OR ... was silently treated as AND. Pre-fix, a query
// like `WHERE n.name='Foo' OR n.name='Bar'` returned only rows where
// BOTH conditions held — which for an equality on a single property is
// always zero. Post-fix, OR is honored: rows that match EITHER side
// surface. Mixed AND/OR is documented as left-to-right (no operator
// precedence): `A AND B OR C` evaluates as `(A AND B) OR C`.

func TestExecute_OR_TwoConditions(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Alpha", "Function", "Go")
	insertSym(t, db, "f2", "Beta", "Function", "Go")
	insertSym(t, db, "f3", "Gamma", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) WHERE f.name='Alpha' OR f.name='Beta' RETURN f.name")
	if r.Total != 2 {
		t.Fatalf("expected 2 rows for OR, got %d", r.Total)
	}
	got := map[string]bool{}
	for _, row := range r.Rows {
		got[row["f.name"].(string)] = true
	}
	if !got["Alpha"] || !got["Beta"] {
		t.Errorf("expected Alpha+Beta in OR result, got %v", got)
	}
	if got["Gamma"] {
		t.Errorf("Gamma must not appear: %v", got)
	}
}

func TestExecute_OR_ThreeConditions(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Alpha", "Function", "Go")
	insertSym(t, db, "f2", "Beta", "Function", "Go")
	insertSym(t, db, "f3", "Gamma", "Function", "Go")
	insertSym(t, db, "f4", "Delta", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) WHERE f.name='Alpha' OR f.name='Beta' OR f.name='Gamma' RETURN f.name")
	if r.Total != 3 {
		t.Fatalf("expected 3 rows for 3-way OR, got %d", r.Total)
	}
}

func TestExecute_AND_StillWorks(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Alpha", "Function", "Go")
	insertSym(t, db, "f2", "Alpha", "Method", "Go")

	r := exec(t, db, "MATCH (f) WHERE f.name='Alpha' AND f.kind='Function' RETURN f.name")
	if r.Total != 1 {
		t.Fatalf("expected 1 row for AND, got %d", r.Total)
	}
}

// Mixed AND/OR walks left-to-right: A AND B OR C == (A AND B) OR C.
// Here: name='Alpha' AND kind='Function' OR name='Zeta' should match
// f1 (Alpha+Function) and f3 (Zeta) but not f2 (Alpha+Method).
func TestExecute_MixedAndOr_LeftToRight(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Alpha", "Function", "Go")
	insertSym(t, db, "f2", "Alpha", "Method", "Go")
	insertSym(t, db, "f3", "Zeta", "Method", "Go")

	r := exec(t, db, "MATCH (f) WHERE f.name='Alpha' AND f.kind='Function' OR f.name='Zeta' RETURN f.name, f.kind")
	if r.Total != 2 {
		t.Fatalf("expected 2 rows for mixed AND/OR, got %d", r.Total)
	}
	names := map[string]bool{}
	for _, row := range r.Rows {
		names[row["f.name"].(string)] = true
	}
	if !names["Alpha"] || !names["Zeta"] {
		t.Errorf("expected Alpha+Zeta, got %v", names)
	}
}

// Sanity-check the parser stamps connector="OR" on the second condition.
func TestParseConditions_StampsOrConnector(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) WHERE f.name='A' OR f.name='B' RETURN f.name")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if len(q.conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(q.conditions))
	}
	if q.conditions[0].connector != "" {
		t.Errorf("first condition connector should be empty, got %q", q.conditions[0].connector)
	}
	if q.conditions[1].connector != "OR" {
		t.Errorf("second condition connector should be OR, got %q", q.conditions[1].connector)
	}
}

func TestConditionsHaveOr(t *testing.T) {
	cases := []struct {
		name string
		in   []condition
		want bool
	}{
		{"empty", nil, false},
		{"single", []condition{{}}, false},
		{"all-AND", []condition{{}, {connector: "AND"}}, false},
		{"has-OR", []condition{{}, {connector: "OR"}}, true},
		{"trailing-OR", []condition{{}, {connector: "AND"}, {connector: "OR"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := conditionsHaveOr(c.in); got != c.want {
				t.Errorf("conditionsHaveOr = %v, want %v", got, c.want)
			}
		})
	}
}
