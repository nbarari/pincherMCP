package cypher

import (
	"context"
	"database/sql"
	"testing"
)

// #438: docstring (and signature/return_type/is_test) were not exposed
// in the cypher row map, so `WHERE n.docstring IS NULL` matched every
// Function (raw undefined) and `IS NOT NULL` matched none. Sweep covers
// the full nullable-text+bool surface added in the same fix.

func seedDocstringSymbols(t *testing.T, db *sql.DB) {
	t.Helper()
	insert := func(id, name, doc, sig, ret string, isTest int) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
				start_byte, end_byte, start_line, end_line,
				signature, return_type, docstring, is_test)
			 VALUES (?, 'proj1', 'file.go', ?, ?, 'Function', 'Go', 0, 100, 1, 5, ?, ?, ?, ?)`,
			id, name, name, nullable(sig), nullable(ret), nullable(doc), isTest,
		)
		if err != nil {
			t.Fatalf("insert symbol %q: %v", id, err)
		}
	}
	insert("documented", "Documented", "Documented does the thing.", "func() error", "error", 0)
	insert("undocumented", "Undocumented", "", "func()", "", 0)
	insert("testFn", "TestSomething", "", "func(*testing.T)", "", 1)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestDocstringProperty_IsNotNullPushdown(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	seedDocstringSymbols(t, db)

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	res, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.docstring IS NOT NULL RETURN n.name`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := len(res.Rows); got != 1 {
		t.Fatalf("IS NOT NULL row count = %d, want 1; rows=%v", got, res.Rows)
	}
	if name, _ := res.Rows[0]["n.name"].(string); name != "Documented" {
		t.Errorf("IS NOT NULL returned %q, want Documented", name)
	}
}

func TestDocstringProperty_IsNullPushdown(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	seedDocstringSymbols(t, db)

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	res, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.docstring IS NULL RETURN n.name`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := len(res.Rows); got != 2 {
		t.Fatalf("IS NULL row count = %d, want 2; rows=%v", got, res.Rows)
	}
}

func TestDocstringProperty_RowProjection(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	seedDocstringSymbols(t, db)

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	res, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.name="Documented" RETURN n.name, n.docstring, n.signature, n.return_type, n.is_test`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(res.Rows))
	}
	row := res.Rows[0]
	if got, _ := row["n.docstring"].(string); got != "Documented does the thing." {
		t.Errorf("docstring = %q", got)
	}
	if got, _ := row["n.signature"].(string); got != "func() error" {
		t.Errorf("signature = %q", got)
	}
	if got, _ := row["n.return_type"].(string); got != "error" {
		t.Errorf("return_type = %q", got)
	}
	if got, _ := row["n.is_test"].(bool); got != false {
		t.Errorf("is_test = %v, want false", got)
	}
}

func TestIsTestProperty_FiltersTests(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	seedDocstringSymbols(t, db)

	// is_test=true should isolate the test fixture.
	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	res, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.is_test=true RETURN n.name`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := len(res.Rows); got != 1 {
		t.Fatalf("is_test=true row count = %d, want 1; rows=%v", got, res.Rows)
	}
	if name, _ := res.Rows[0]["n.name"].(string); name != "TestSomething" {
		t.Errorf("is_test=true returned %q, want TestSomething", name)
	}
}

func TestReturnTypeProperty_EqualityPushdown(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	seedDocstringSymbols(t, db)

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	res, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.return_type="error" RETURN n.name`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := len(res.Rows); got != 1 {
		t.Fatalf("return_type='error' row count = %d, want 1; rows=%v", got, res.Rows)
	}
}
