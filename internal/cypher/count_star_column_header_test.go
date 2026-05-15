package cypher

import "testing"

// #946: `RETURN COUNT(*)` rendered the column header as "COUNT()" —
// the `*` argument was silently stripped because the tokenizer reads
// `*` as an empty HOPS token (the variable-length path scanner). The
// value was correct; only the column name lost the asterisk. Round-trip
// breaks when a caller uses the header as a property reference, and
// it's the same silent-confidently-wrong shape as #836/#945.

func TestRETURN_CountStar_PreservesAsteriskInColumnHeader(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	insertSym(t, db, "id-1", "Foo", "Function", "Go")
	insertSym(t, db, "id-2", "Bar", "Function", "Go")

	r := exec(t, db, "MATCH (n:Function) RETURN COUNT(*)")
	if r.Total != 1 {
		t.Fatalf("Total = %d, want 1 (one aggregated row)", r.Total)
	}
	if len(r.Columns) != 1 || r.Columns[0] != "COUNT(*)" {
		t.Fatalf("Columns = %v, want [COUNT(*)] (asterisk must round-trip)", r.Columns)
	}
	if _, ok := r.Rows[0]["COUNT(*)"]; !ok {
		t.Fatalf("row missing COUNT(*) key; got %v", r.Rows[0])
	}
	got, _ := r.Rows[0]["COUNT(*)"].(int)
	if got != 2 {
		t.Errorf("COUNT(*) = %d, want 2", got)
	}
}

// COUNT(n) (no asterisk) keeps its original "COUNT(n)" rendering.
func TestRETURN_CountVariable_UnchangedColumnHeader(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	insertSym(t, db, "id-1", "Foo", "Function", "Go")

	r := exec(t, db, "MATCH (n:Function) RETURN COUNT(n)")
	if len(r.Columns) != 1 || r.Columns[0] != "COUNT(n)" {
		t.Errorf("Columns = %v, want [COUNT(n)] (variable form is the existing canonical)", r.Columns)
	}
}
