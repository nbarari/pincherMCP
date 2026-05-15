package cypher

import (
	"context"
	"testing"
)

// #906: COUNT(n.property) must count only rows where the property is
// non-null, matching SQL/Cypher semantics. Pre-fix it returned the
// row count (len(rows)), making `COUNT(n.docstring)` indistinguishable
// from `COUNT(n)` — silently wrong on the canonical "how many
// functions are documented" query.

func TestExecute_CountProperty_ExcludesNullRows(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Seed 5 functions: 2 with docstrings, 3 with NULL docstrings.
	if _, err := db.Exec(`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language, docstring, start_byte, end_byte, start_line, end_line) VALUES
		('a','proj1','f.go','A','A','Function','Go','doc A', 0,10,1,2),
		('b','proj1','f.go','B','B','Function','Go','doc B', 11,20,3,4),
		('c','proj1','f.go','C','C','Function','Go',NULL,    21,30,5,6),
		('d','proj1','f.go','D','D','Function','Go',NULL,    31,40,7,8),
		('e','proj1','f.go','E','E','Function','Go',NULL,    41,50,9,10)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN COUNT(n.docstring) AS docs, COUNT(n) AS funcs`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("expected single aggregate row; got %d", len(r.Rows))
	}
	row := r.Rows[0]
	docs := toIntForTest(t, row["docs"])
	funcs := toIntForTest(t, row["funcs"])
	if funcs != 5 {
		t.Errorf("COUNT(n) must count all 5 functions; got %d", funcs)
	}
	if docs != 2 {
		t.Errorf("COUNT(n.docstring) must count only the 2 non-null rows; got %d", docs)
	}
	if docs == funcs {
		t.Errorf("COUNT(n.docstring) must differ from COUNT(n) when NULLs are present; both are %d (the pre-fix bug shape)", docs)
	}
}

// Empty-string is non-null and IS counted.
func TestExecute_CountProperty_CountsEmptyStrings(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language, docstring, start_byte, end_byte, start_line, end_line) VALUES
		('a','proj1','f.go','A','A','Function','Go','',   0,10,1,2),
		('b','proj1','f.go','B','B','Function','Go',NULL, 11,20,3,4)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN COUNT(n.docstring) AS docs`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	docs := toIntForTest(t, r.Rows[0]["docs"])
	// SQL counts "" as non-null — only the NULL row is excluded.
	if docs != 1 {
		t.Errorf("COUNT(n.docstring) with one empty-string + one NULL row should be 1; got %d", docs)
	}
}

// COUNT(*) and COUNT(n) keep their existing row-count semantics.
func TestExecute_CountStar_StillCountsAllRows(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language, docstring, start_byte, end_byte, start_line, end_line) VALUES
		('a','proj1','f.go','A','A','Function','Go','doc', 0,10,1,2),
		('b','proj1','f.go','B','B','Function','Go',NULL,  11,20,3,4),
		('c','proj1','f.go','C','C','Function','Go',NULL,  21,30,5,6)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(), `MATCH (n:Function) RETURN COUNT(n) AS total`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	total := toIntForTest(t, r.Rows[0]["total"])
	if total != 3 {
		t.Errorf("COUNT(n) must count all 3 rows including NULL-docstring; got %d", total)
	}
}

func toIntForTest(t *testing.T, v any) int {
	t.Helper()
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	t.Fatalf("expected numeric, got %T (%v)", v, v)
	return 0
}
