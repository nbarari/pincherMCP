package cypher

import (
	"testing"
)

// #591: pre-fix MATCH (a)-[:CALLS]->(b) returned duplicate rows when
// the same logical edge was multi-sourced (per_file + resolve_pass +
// binding_pass). Each pass owns its source-tagged subset by design,
// but the user-facing semantic is "this caller calls this callee
// once" — the JOIN must collapse to one row per logical edge,
// keeping the highest-confidence variant.

func TestExecute_MultiSourcedEdge_DedupsRowsByFromToKind(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	insertSym(t, db, "caller", "Caller", "Function", "Go")
	insertSym(t, db, "callee", "Callee", "Function", "Go")

	// Three rows in the edges table for the same (from, to, kind) —
	// matches what the pre-#591 schema does post-resolve_pass and
	// post-binding_pass.
	// Three rows for the same (from, to, kind) with different
	// confidences — simulates the per_file / resolve_pass /
	// binding_pass triple that the real schema produces. The test
	// schema doesn't carry the `source` column but the dedup logic
	// is keyed on (from, to, kind) regardless, so this exercises the
	// fix path identically.
	for _, conf := range []float64{0.5, 0.7, 0.4} {
		_, err := db.Exec(
			`INSERT INTO edges(project_id, from_id, to_id, kind, confidence)
			 VALUES ('proj1', 'caller', 'callee', 'CALLS', ?)`,
			conf,
		)
		if err != nil {
			t.Fatalf("insert edge (conf=%v): %v", conf, err)
		}
	}

	r := exec(t, db, `MATCH (a:Function)-[e:CALLS]->(b:Function) WHERE a.name="Caller" RETURN a.id, b.id, e.confidence`)

	if r.Total != 1 {
		t.Errorf("expected 1 row (logical edge dedup), got %d rows: %v",
			r.Total, r.Rows)
	}
	if r.Total >= 1 {
		// Highest confidence wins → resolve_pass at 0.7.
		conf, _ := r.Rows[0]["e.confidence"].(float64)
		if conf != 0.7 {
			t.Errorf("expected highest-confidence variant (0.7) preserved; got %v", conf)
		}
	}
}

// Sanity: edges that are NOT multi-sourced still surface as one row
// (no false-negative dedup).
func TestExecute_SingleSourcedEdge_NotAccidentallyDeduped(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	insertSym(t, db, "a", "A", "Function", "Go")
	insertSym(t, db, "b", "B", "Function", "Go")
	insertSym(t, db, "c", "C", "Function", "Go")

	insertEdge(t, db, "a", "b", "CALLS")
	insertEdge(t, db, "a", "c", "CALLS")

	r := exec(t, db, `MATCH (a:Function)-[:CALLS]->(b:Function) WHERE a.name="A" RETURN b.name`)
	if r.Total != 2 {
		t.Errorf("expected 2 distinct callees (B + C), got %d: %v", r.Total, r.Rows)
	}
}
