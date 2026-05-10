package cypher

import (
	"database/sql"
	"fmt"
	"testing"
)

// insertSymWithLine is a test-local extension of insertSym that
// also sets start_line — needed for ORDER BY numeric tests because
// the shared helper hardcodes start_line=1.
func insertSymWithLine(t *testing.T, db *sql.DB, id, name, kind, lang string, line int) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
			start_byte, end_byte, start_line, end_line) VALUES (?,?,?,?,?,?,?, 0, 100, ?, ?)`,
		id, "proj1", "file.go", name, name, kind, lang, line, line+5,
	)
	if err != nil {
		t.Fatalf("insert symbol %q: %v", id, err)
	}
}

// #313: ORDER BY on numeric columns now compares numerically. The
// pre-fix path stringified via fmt.Sprint and sorted lex, so 126
// landed AFTER 1004 because "1" < "1" then "0" < "2" in string compare.

func TestNodeScan_OrderBy_NumericColumn_AscendingOrder(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert in non-sorted order. start_line values span across the
	// power-of-10 boundary (126, 1004, 1175, 141) so a string-compare
	// sort would scramble them.
	for i, line := range []int{1004, 126, 1175, 141, 1019} {
		insertSymWithLine(t, db,
			fmt.Sprintf("id-%d", i),
			fmt.Sprintf("F%d", i),
			"Function", "Go", line)
	}

	r := exec(t, db, "MATCH (n:Function) RETURN n.name, n.start_line ORDER BY n.start_line")
	if len(r.Rows) != 5 {
		t.Fatalf("got %d rows, want 5", len(r.Rows))
	}
	wantOrder := []int{126, 141, 1004, 1019, 1175}
	for i, want := range wantOrder {
		got := toInt(t, r.Rows[i]["n.start_line"])
		if got != want {
			t.Errorf("rows[%d].n.start_line = %v, want %d (numeric ORDER BY ascending)",
				i, r.Rows[i]["n.start_line"], want)
		}
	}
}

func TestNodeScan_OrderBy_NumericColumn_Descending(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	for i, line := range []int{100, 1500, 25, 999} {
		insertSymWithLine(t, db,
			fmt.Sprintf("id-%d", i),
			fmt.Sprintf("F%d", i),
			"Function", "Go", line)
	}
	r := exec(t, db, "MATCH (n:Function) RETURN n.start_line ORDER BY n.start_line DESC")
	wantOrder := []int{1500, 999, 100, 25}
	for i, want := range wantOrder {
		got := toInt(t, r.Rows[i]["n.start_line"])
		if got != want {
			t.Errorf("rows[%d].n.start_line = %v, want %d (DESC numeric)",
				i, r.Rows[i]["n.start_line"], want)
		}
	}
}

// String columns still sort lexicographically.
func TestNodeScan_OrderBy_StringColumn_Lexicographic(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "id-c", "Charlie", "Function", "Go")
	insertSym(t, db, "id-a", "Alpha", "Function", "Go")
	insertSym(t, db, "id-b", "Bravo", "Function", "Go")

	r := exec(t, db, "MATCH (n:Function) RETURN n.name ORDER BY n.name")
	wantOrder := []string{"Alpha", "Bravo", "Charlie"}
	for i, want := range wantOrder {
		got, _ := r.Rows[i]["n.name"].(string)
		if got != want {
			t.Errorf("rows[%d].n.name = %q, want %q", i, got, want)
		}
	}
}

// Helper coverage: cypherLessThan + toFloatForOrderBy across the
// integer / float / string / nil shapes.
func TestCypherLessThan_TypeMatrix(t *testing.T) {
	cases := []struct {
		name string
		a, b any
		desc bool
		want bool // true means a < b (or a > b under desc)
	}{
		{"int vs int", 5, 10, false, true},
		{"int desc", 5, 10, true, false},
		{"int64 vs int64", int64(126), int64(1004), false, true},
		{"int vs int64", 5, int64(10), false, true},
		{"float vs float", 1.5, 2.5, false, true},
		{"int vs float", 1, 2.5, false, true},
		{"string vs string", "alpha", "bravo", false, true},
		{"string desc", "alpha", "bravo", true, false},
		// Mixed types fall to string compare (rare).
		{"int vs string falls to string", 100, "alpha", false, true}, // "100" < "alpha"
		// Nil handling — both nil sort equal (lex compare returns false).
		{"nil vs nil", nil, nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cypherLessThan(c.a, c.b, c.desc); got != c.want {
				t.Errorf("cypherLessThan(%v, %v, desc=%v) = %v, want %v",
					c.a, c.b, c.desc, got, c.want)
			}
		})
	}
}

// toInt converts any of the numeric shapes the test DB might return
// (int / int64 / float64) into a comparable int. Centralised here
// so the ORDER BY assertions don't have to type-switch inline.
func toInt(t *testing.T, v any) int {
	t.Helper()
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	t.Fatalf("toInt: unsupported type %T (%v)", v, v)
	return 0
}
