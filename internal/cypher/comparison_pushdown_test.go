package cypher

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// #434: comparison operators (>, <, >=, <=, <>) used to be excluded
// from SQL pushdown, so a query like `WHERE n.start_line > 4000`
// scanned the first maxRows()*2 rows from the symbols table and
// post-filtered in Go. When the matching rows sat past that clamp,
// the result was 0 — same bug class as #412 but for the comparison
// family.
//
// Repro: insert > maxRows()*2 noise rows with low start_line, then a
// few matching rows with high start_line. Pre-fix, the matching rows
// fell outside the SQL scan window and the in-Go filter saw zero.
func TestExecute_ComparisonPushdown_Greater(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertManyWithStartLine(t, db, 250, 100) // noise: start_line=100
	insertManyWithStartLine(t, db, 5, 5000)  // matches: start_line=5000

	r := exec(t, db, `MATCH (n) WHERE n.start_line > 4000 RETURN n.id`)
	if r.Total != 5 {
		t.Fatalf("expected 5 rows for start_line>4000 past LIMIT clamp, got %d", r.Total)
	}
}

func TestExecute_ComparisonPushdown_GreaterOrEqual(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertManyWithStartLine(t, db, 250, 100)
	insertManyWithStartLine(t, db, 5, 5000)

	r := exec(t, db, `MATCH (n) WHERE n.start_line >= 5000 RETURN n.id`)
	if r.Total != 5 {
		t.Fatalf("expected 5 rows for start_line>=5000, got %d", r.Total)
	}
}

func TestExecute_ComparisonPushdown_Less(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertManyWithStartLine(t, db, 250, 5000) // noise: start_line=5000
	insertManyWithStartLine(t, db, 5, 50)     // matches: start_line=50

	r := exec(t, db, `MATCH (n) WHERE n.start_line < 100 RETURN n.id`)
	if r.Total != 5 {
		t.Fatalf("expected 5 rows for start_line<100, got %d", r.Total)
	}
}

func TestExecute_ComparisonPushdown_LessOrEqual(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertManyWithStartLine(t, db, 250, 5000)
	insertManyWithStartLine(t, db, 5, 50)

	r := exec(t, db, `MATCH (n) WHERE n.start_line <= 50 RETURN n.id`)
	if r.Total != 5 {
		t.Fatalf("expected 5 rows for start_line<=50, got %d", r.Total)
	}
}

func TestExecute_ComparisonPushdown_NotEqual(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertManyWithStartLine(t, db, 250, 100) // noise: start_line=100, name="N"
	insertManyWithName(t, db, 5, "Match")    // matches: name="Match"

	// `n.name <> "N"` matches the 5 named "Match" rows.
	r := exec(t, db, `MATCH (n) WHERE n.name <> "N" RETURN n.id`)
	if r.Total != 5 {
		t.Fatalf("expected 5 rows for name<>'N', got %d", r.Total)
	}
}

// #434 in combination with OR (the v0.15.2 fix path): the OR pushdown
// in whereExprToSQL now handles comparison operators too.
func TestExecute_ComparisonPushdown_InsideOR(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertManyWithStartLine(t, db, 250, 100)
	insertManyWithStartLine(t, db, 3, 5000)
	insertManyWithStartLine(t, db, 2, 1)

	r := exec(t, db, `MATCH (n) WHERE n.start_line > 4000 OR n.start_line < 10 RETURN n.id`)
	if r.Total != 5 {
		t.Fatalf("expected 5 rows for OR with both branches comparison ops, got %d", r.Total)
	}
}

func insertManyWithStartLine(t *testing.T, db *sql.DB, count, startLine int) {
	t.Helper()
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("sl%d_%d", startLine, i)
		_, err := db.ExecContext(context.Background(),
			`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
				start_byte, end_byte, start_line, end_line) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			id, "proj1", "f.go", "N", "N", "Function", "Go", 0, 100, startLine, startLine+1,
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

func insertManyWithName(t *testing.T, db *sql.DB, count int, name string) {
	t.Helper()
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("nm_%s_%d", name, i)
		_, err := db.ExecContext(context.Background(),
			`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
				start_byte, end_byte, start_line, end_line) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			id, "proj1", "f.go", name, name, "Function", "Go", 0, 100, 1, 5,
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}
