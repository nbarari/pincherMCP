package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1035: neighborhood with offset >= totalNeighbors silently clamped
// the offset to totalNeighbors and returned an empty page with no
// signal the offset overshot. Agents reading `count: 224,
// page.returned: 0` couldn't distinguish "file has neighbors but I
// paged past them" from a degenerate seed. Same shape as #1033
// (search) / #1034 (list).

func TestHandleNeighborhood_OffsetPastEnd_DiagnosisNamesPaginationOvershoot(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-neighbor-overshoot"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid
	srv.sessionRoot = t.TempDir()

	// Seed 3 neighbors in the same file.
	syms := []db.Symbol{}
	for i := 0; i < 3; i++ {
		syms = append(syms, db.Symbol{
			ID:                   pid + "::pkg.Sym" + string(rune('A'+i)) + "#Function",
			ProjectID:            pid,
			FilePath:             "shared.go",
			Name:                 "Sym" + string(rune('A'+i)),
			QualifiedName:        "pkg.Sym" + string(rune('A'+i)),
			Kind:                 "Function",
			Language:             "Go",
			StartByte:            i * 100,
			EndByte:              i*100 + 50,
			ExtractionConfidence: 1.0,
		})
	}
	mustUpsertSymbols(t, store, syms)

	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     syms[0].ID,
		"offset": float64(999),
		"limit":  float64(2),
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)

	total, _ := body["count"].(float64)
	if total < 1 {
		t.Fatalf("expected total > 0 (we seeded 3 symbols); got count=%v", total)
	}
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	diagnosis, _ := meta["diagnosis"].(string)
	if !strings.Contains(diagnosis, "pagination overshoot") ||
		!strings.Contains(diagnosis, "offset=999") {
		t.Errorf("expected pagination-overshoot diagnosis naming offset; got %q", diagnosis)
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
		t.Errorf("expected retry-without-offset next_step; got %v", steps)
	}
}

// Control: a normal in-range call must not trip the overshoot diagnosis.
func TestHandleNeighborhood_NormalOffset_NoOvershootDiagnosis(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-neighbor-normal"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	syms := []db.Symbol{}
	for i := 0; i < 3; i++ {
		syms = append(syms, db.Symbol{
			ID:                   pid + "::pkg.Sym" + string(rune('A'+i)) + "#Function",
			ProjectID:            pid,
			FilePath:             "shared.go",
			Name:                 "Sym" + string(rune('A'+i)),
			QualifiedName:        "pkg.Sym" + string(rune('A'+i)),
			Kind:                 "Function",
			Language:             "Go",
			StartByte:            i * 100,
			EndByte:              i*100 + 50,
			ExtractionConfidence: 1.0,
		})
	}
	mustUpsertSymbols(t, store, syms)

	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":    syms[0].ID,
		"limit": float64(50),
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	diagnosis, _ := meta["diagnosis"].(string)
	if strings.Contains(diagnosis, "pagination overshoot") {
		t.Errorf("must not claim overshoot for in-range offset; got %q", diagnosis)
	}
}
