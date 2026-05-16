package cypher

import (
	"context"
	"database/sql"
	"testing"
)

// #1126: ORDER BY <alias> silently returns the top-N of a random
// scan window rather than the global top-N. Pre-fix:
//
//	RETURN n.complexity AS cx ORDER BY cx DESC LIMIT 5
//
// against ~2500 functions returned five small-complexity rows (27, 24,
// 20, 18, 16) while the same query with `ORDER BY n.complexity`
// returned the actual top five (98, 79, 59, 49, 45). orderByCol
// stripped the var prefix, looked up "cx" in cypherPropToCol's
// whitelist, got "", skipped the SQL ORDER BY pushdown. The safety
// LIMIT then clamped scan to scanLimitFor(maxRows) rows in their
// natural order; buildResult sorted that pre-truncated window. Same
// #847 family that bit the non-aliased path.
//
// The fix: resolveOrderByAlias maps the alias back to its source
// `var.prop` form before orderByCol / joinOrderByCol runs.

// Positive: aliased ORDER BY returns the same top-N as the
// non-aliased equivalent.
func TestExecute_OrderByAlias_ReturnsGlobalTopN(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	// Seed: enough rows past the post-scan window to prove the SQL
	// pushdown actually ran. We want a high-complexity row inserted
	// LATE so a scan-order LIMIT would miss it.
	insertSymWithCx(t, db, "low_a", "alpha", "Function", "Go", 5)
	insertSymWithCx(t, db, "low_b", "bravo", "Function", "Go", 6)
	insertSymWithCx(t, db, "low_c", "charlie", "Function", "Go", 7)
	insertSymWithCx(t, db, "low_d", "delta", "Function", "Go", 8)
	insertSymWithCx(t, db, "low_e", "echo", "Function", "Go", 9)
	// The high-complexity row at the END — scan_order LIMIT 5 misses it.
	insertSymWithCx(t, db, "high", "TOPFUNC", "Function", "Go", 999)

	e := &Executor{DB: db, MaxRows: 3, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name AS title, n.complexity AS cx ORDER BY cx DESC LIMIT 3`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("expected 3 rows; got %d: %v", len(r.Rows), r.Rows)
	}
	// First row must be the high-complexity entry — proves SQL ORDER
	// BY was pushed BEFORE the scan cap.
	if r.Rows[0]["title"] != "TOPFUNC" {
		t.Errorf("aliased ORDER BY must return global top-1 (TOPFUNC, cx=999); got %v", r.Rows[0])
	}
}

// Control: ORDER BY the source property (n.complexity, no alias) —
// existing #847 path, must keep working.
func TestExecute_OrderBySourceProp_ReturnsGlobalTopN(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSymWithCx(t, db, "low_a", "alpha", "Function", "Go", 5)
	insertSymWithCx(t, db, "low_b", "bravo", "Function", "Go", 6)
	insertSymWithCx(t, db, "high", "TOPFUNC", "Function", "Go", 999)

	e := &Executor{DB: db, MaxRows: 1, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name, n.complexity ORDER BY n.complexity DESC LIMIT 1`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("expected 1 row; got %d", len(r.Rows))
	}
	if r.Rows[0]["n.name"] != "TOPFUNC" {
		t.Errorf("expected TOPFUNC; got %v", r.Rows[0])
	}
}

// Control: ORDER BY an alias that maps to the same source as a
// non-aliased return — both forms must agree.
func TestExecute_OrderByAlias_MatchesSourceOrderBy(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		insertSymWithCx(t, db, "n"+name, name, "Function", "Go", (i+1)*10)
	}
	insertSymWithCx(t, db, "ntop", "TOPFUNC", "Function", "Go", 999)

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	source, _ := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name, n.complexity ORDER BY n.complexity DESC`)
	alias, _ := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name AS title, n.complexity AS cx ORDER BY cx DESC`)
	if len(source.Rows) != len(alias.Rows) {
		t.Fatalf("row counts differ: source=%d alias=%d", len(source.Rows), len(alias.Rows))
	}
	for i := range source.Rows {
		sname := source.Rows[i]["n.name"]
		atitle := alias.Rows[i]["title"]
		if sname != atitle {
			t.Errorf("row %d ordering mismatch: source=%v alias=%v", i, sname, atitle)
		}
	}
}

// insertSymWithCx mirrors insertSym but lets the test set complexity
// so ORDER BY n.complexity has something distinct to sort on.
func insertSymWithCx(t *testing.T, db *sql.DB, id, name, kind, lang string, cx int) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
			start_byte, end_byte, start_line, end_line, complexity)
			VALUES (?, 'proj1', 'file.go', ?, ?, ?, ?, 0, 100, 1, 5, ?)`,
		id, name, name, kind, lang, cx,
	)
	if err != nil {
		t.Fatalf("insert symbol %q: %v", id, err)
	}
}
