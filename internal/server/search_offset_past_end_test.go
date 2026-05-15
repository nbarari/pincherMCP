package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1033: search with offset >= total fell into the generic empty-result
// diagnosis ("no exact-term matches for X — wildcards often surface...")
// even though `total > 0` in the SAME response told a contradictory
// story. Agents reading the diagnosis concluded the symbol didn't
// exist; the response actually said there were matches, just outside
// the page window. Now: detect pagination overshoot and surface a
// diagnosis naming the cause + a next_step that retries at offset=0.

func TestHandleSearch_OffsetPastEnd_DiagnosisNamesPaginationOvershoot(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-search-offset"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.OffsetProbeA#Function", ProjectID: pid, FilePath: "a.go",
			Name: "OffsetProbeA", QualifiedName: "pkg.OffsetProbeA", Kind: "Function", Language: "Go",
			Signature: "func OffsetProbeA()", ExtractionConfidence: 1.0},
		{ID: pid + "::pkg.OffsetProbeB#Function", ProjectID: pid, FilePath: "b.go",
			Name: "OffsetProbeB", QualifiedName: "pkg.OffsetProbeB", Kind: "Function", Language: "Go",
			Signature: "func OffsetProbeB()", ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":  "OffsetProbe*",
		"offset": float64(999),
		"limit":  float64(5),
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)

	// total > 0 confirms the query matched; results empty due to offset.
	total, _ := body["total"].(float64)
	if total < 1 {
		t.Fatalf("expected total > 0 (query matches exist); got total=%v body=%v", total, body)
	}
	meta, _ := body["_meta"].(map[string]any)
	diagnosis, _ := meta["diagnosis"].(string)
	if !strings.Contains(diagnosis, "pagination overshoot") ||
		!strings.Contains(diagnosis, "offset=999") {
		t.Errorf("expected pagination-overshoot diagnosis naming offset; got %q", diagnosis)
	}
	// Diagnosis must NOT use the misleading "no exact-term matches" wording.
	if strings.Contains(diagnosis, "no exact-term matches") {
		t.Errorf("diagnosis must not claim zero matches when total>0; got %q", diagnosis)
	}
	steps, _ := meta["next_steps"].([]any)
	foundRetry := false
	for _, st := range steps {
		stMap, _ := st.(map[string]any)
		why, _ := stMap["why"].(string)
		if strings.Contains(why, "retry without offset") {
			foundRetry = true
			break
		}
	}
	if !foundRetry {
		t.Errorf("expected next_step suggesting retry without offset; got %v", steps)
	}
}

// Control: when total == 0 (the query really matches nothing), the
// original empty-diagnosis path still runs (no pagination-overshoot
// branch).
func TestHandleSearch_TrulyNoMatches_KeepsOriginalDiagnosis(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-search-truly-empty"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "DefinitelyDoesNotExist",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	diagnosis, _ := meta["diagnosis"].(string)
	if strings.Contains(diagnosis, "pagination overshoot") {
		t.Errorf("must not claim pagination overshoot when total=0; got %q", diagnosis)
	}
}
