package cypher

import (
	"fmt"
	"testing"
)

// #308: COUNT() returned the post-LIMIT row count instead of the
// match cardinality. This pinned the bug + the fix.

// Seed N>maxRows*2 Function symbols, then COUNT them. The pre-fix
// path returned `maxRows*2` (the SQL LIMIT clamp) instead of N.
func TestNodeScan_CountIgnoresLimitClamp(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// 500 symbols. Default maxRows=200 → SQL clamp would be 400; we
	// need N>400 to expose the bug.
	const N = 500
	for i := 0; i < N; i++ {
		insertSym(t, db,
			fmt.Sprintf("id-%d", i),
			fmt.Sprintf("F%d", i),
			"Function", "Go")
	}

	r := exec(t, db, "MATCH (n:Function) RETURN COUNT(n) AS total")
	if r.Total != 1 {
		t.Fatalf("Total = %d, want 1 (one aggregated row)", r.Total)
	}
	got, _ := r.Rows[0]["total"].(int)
	if got != N {
		t.Errorf("COUNT(n) = %d, want %d (cardinality, not the LIMIT clamp)", got, N)
	}
}

// Non-aggregating queries still respect LIMIT — the safety clamp
// stays in place to bound runaway scans.
func TestNodeScan_NonAggregatingStillRespectsClamp(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Seed 600 symbols; default max_rows=200 → SQL clamp = 400. Then
	// after the user-supplied LIMIT (or default 200) is applied,
	// projected rows should be 200.
	const N = 600
	for i := 0; i < N; i++ {
		insertSym(t, db,
			fmt.Sprintf("id-%d", i),
			fmt.Sprintf("F%d", i),
			"Function", "Go")
	}

	r := exec(t, db, "MATCH (n:Function) RETURN n.name")
	if r.Total > 200 {
		t.Errorf("Total = %d, expected ≤200 (LIMIT default for non-aggregating)", r.Total)
	}
}

// COUNT with WHERE filter — exercises the conditions path AND the
// aggregation skip together.
func TestNodeScan_CountWithWhereFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	for i := 0; i < 500; i++ {
		insertSym(t, db,
			fmt.Sprintf("get-%d", i),
			fmt.Sprintf("Get%d", i),
			"Function", "Go")
	}
	for i := 0; i < 100; i++ {
		insertSym(t, db,
			fmt.Sprintf("set-%d", i),
			fmt.Sprintf("Set%d", i),
			"Function", "Go")
	}

	r := exec(t, db, "MATCH (n:Function) WHERE n.name STARTS WITH 'Get' RETURN COUNT(n) AS total")
	got, _ := r.Rows[0]["total"].(int)
	if got != 500 {
		t.Errorf("COUNT for STARTS WITH 'Get' = %d, want 500", got)
	}
}

// hasAggregation helper — pin its decisions so a future tweak to the
// condition struct can't silently change which queries get the
// LIMIT-skip treatment.
func TestHasAggregation(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"MATCH (n) RETURN COUNT(n)", true},
		{"MATCH (n) RETURN COUNT(n) AS total", true},
		{"MATCH (n) RETURN n.name", false},
		{"MATCH (n) RETURN n.name LIMIT 5", false},
		{"MATCH (n) RETURN n.name, COUNT(n)", true}, // mixed
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			tokens := tokenize(c.query)
			p := &parser{tokens: tokens}
			q, err := p.parseQuery()
			if err != nil {
				t.Fatalf("parseQuery: %v", err)
			}
			if got := hasAggregation(q); got != c.want {
				t.Errorf("hasAggregation = %v, want %v for %q", got, c.want, c.query)
			}
		})
	}
}
