package cypher

import (
	"context"
	"strings"
	"testing"
)

// #578: pinchQL silently accepted unknown function names in RETURN as
// bare variable references — `RETURN LENGTH(f.docstring)` rendered a
// column named `LENGTH` with every value null, no warning. Same UX
// class as #473 (typo'd properties): the engine accepts malformed
// input and returns an answer that looks valid but isn't.
//
// Fix surfaces a clear pinchQL error naming the offender + the
// supported aggregator set, so the caller can correct the query
// instead of acting on null results.

func TestExecute_UnknownFunctionInReturn_Errors(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	cases := []struct {
		name  string
		query string
	}{
		{"LENGTH (common typo for COUNT-of-string)", `MATCH (n:Function) RETURN LENGTH(n.docstring)`},
		{"SUBSTR (string fn doesn't exist)", `MATCH (n:Function) RETURN SUBSTR(n.name)`},
		{"FOO (made-up name)", `MATCH (n:Function) RETURN FOO(n.id)`},
	}
	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	for _, c := range cases {
		_, err := e.Execute(context.Background(), c.query)
		if err == nil {
			t.Errorf("%s: expected error, got nil — pinchQL silently accepted unknown function (#578 regression)", c.name)
			continue
		}
		msg := err.Error()
		// Error must name the offending function so the caller can
		// fix the query without guessing what went wrong.
		if !strings.Contains(msg, "unknown function") {
			t.Errorf("%s: error should say %q; got: %v", c.name, "unknown function", err)
		}
		// Error should list the supported aggregators so the caller
		// can pick a real one.
		if !strings.Contains(msg, "COUNT") {
			t.Errorf("%s: error should list supported set; got: %v", c.name, err)
		}
	}
}

// Sanity: known aggregators still work after the gate landed.
func TestExecute_KnownAggregatorsStillWork(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSymWithComplexity(t, db, "f1", "A", 10)
	insertSymWithComplexity(t, db, "f2", "B", 20)

	for _, fn := range []string{"COUNT", "AVG", "MIN", "MAX", "SUM"} {
		query := `MATCH (n:Function) RETURN ` + fn + `(n.complexity)`
		if fn == "COUNT" {
			query = `MATCH (n:Function) RETURN COUNT(n)`
		}
		r := exec(t, db, query)
		if r.Total != 1 {
			t.Errorf("%s should produce 1 row, got %d", fn, r.Total)
		}
	}
}
