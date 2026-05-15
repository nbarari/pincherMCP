package cypher

import (
	"context"
	"testing"
)

// #904: `RETURN n.name AS funcname ORDER BY n.name` silently dropped
// the sort because the projected row map's key was the alias
// (`funcname`), not the source reference (`n.name`). Both spellings
// must now sort identically — the user wrote ORDER BY against the
// column they understood it by, and pinchQL should honor that.

func TestExecute_OrderBy_SourceColumn_WithProjectionAlias(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "z", "zeta", "Function", "Go")
	insertSym(t, db, "a", "alpha", "Function", "Go")
	insertSym(t, db, "m", "mu", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name AS funcname ORDER BY n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("expected 3 rows; got %d", len(r.Rows))
	}
	got := []string{
		r.Rows[0]["funcname"].(string),
		r.Rows[1]["funcname"].(string),
		r.Rows[2]["funcname"].(string),
	}
	want := []string{"alpha", "mu", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ORDER BY n.name with alias: row %d got %q, want %q (full result %v)", i, got[i], want[i], got)
		}
	}
}

// The alias name alone also still sorts correctly (regression guard
// — fix mustn't break the already-working aliased ORDER BY).
func TestExecute_OrderBy_AliasName_StillSorts(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "z", "zeta", "Function", "Go")
	insertSym(t, db, "a", "alpha", "Function", "Go")
	insertSym(t, db, "m", "mu", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name AS funcname ORDER BY funcname`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("expected 3 rows; got %d", len(r.Rows))
	}
	got := []string{
		r.Rows[0]["funcname"].(string),
		r.Rows[1]["funcname"].(string),
		r.Rows[2]["funcname"].(string),
	}
	want := []string{"alpha", "mu", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ORDER BY funcname (alias): row %d got %q, want %q", i, got[i], want[i])
		}
	}
}

// DESC against the source column with an alias must also flip the
// order — symmetric to the ASC test above.
func TestExecute_OrderBy_SourceColumn_DESC_WithAlias(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "z", "zeta", "Function", "Go")
	insertSym(t, db, "a", "alpha", "Function", "Go")
	insertSym(t, db, "m", "mu", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name AS funcname ORDER BY n.name DESC`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("expected 3 rows; got %d", len(r.Rows))
	}
	got := []string{
		r.Rows[0]["funcname"].(string),
		r.Rows[1]["funcname"].(string),
		r.Rows[2]["funcname"].(string),
	}
	want := []string{"zeta", "mu", "alpha"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ORDER BY n.name DESC with alias: row %d got %q, want %q", i, got[i], want[i])
		}
	}
}

// Non-aliased ORDER BY (the simple case) must still work — no
// regression from the alias-resolution code.
func TestExecute_OrderBy_NoAlias_StillSorts(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "z", "zeta", "Function", "Go")
	insertSym(t, db, "a", "alpha", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name ORDER BY n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := []string{
		r.Rows[0]["n.name"].(string),
		r.Rows[1]["n.name"].(string),
	}
	if got[0] != "alpha" || got[1] != "zeta" {
		t.Errorf("non-aliased ORDER BY n.name: got %v, want [alpha zeta]", got)
	}
}
