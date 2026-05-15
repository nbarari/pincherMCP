package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1034: list with offset past the end of the filtered result set
// returned a bare `returned: 0` with no signal the offset was the
// cause. Agents reading `count: N, page.returned: 0` couldn't tell
// whether the filter genuinely matched nothing or they'd overshot
// the page window. Same shape as #1033 for search.

func TestHandleList_OffsetPastEnd_DiagnosisNamesPaginationOvershoot(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	// Seed a handful of indexed projects with edges so they pass the
	// default filters.
	now := time.Now()
	for i := 0; i < 3; i++ {
		pid := "p-list-overshoot-" + string(rune('a'+i))
		store.UpsertProject(db.Project{
			ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: now,
			FileCount: 1, SymCount: 1, EdgeCount: 5,
		})
	}
	srv.sessionID = "p-list-overshoot-a"

	result, err := srv.handleList(context.Background(), makeReq(map[string]any{
		"offset": float64(999),
		"limit":  float64(2),
	}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, result)

	total, _ := body["count"].(float64)
	if total < 1 {
		t.Fatalf("expected total > 0 after filters (we seeded 3 live projects); got count=%v", total)
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
	// Diagnosis must NOT use the no-projects wording when total > 0.
	if strings.Contains(diagnosis, "no projects indexed yet") {
		t.Errorf("diagnosis must not claim empty store when total>0; got %q", diagnosis)
	}
}

// Control: the "no projects indexed" diagnosis path still runs when
// total truly is 0 (fresh-install + filters drop everything).
func TestHandleList_TrulyEmpty_KeepsOriginalDiagnosis(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	result, err := srv.handleList(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	diagnosis, _ := meta["diagnosis"].(string)
	if strings.Contains(diagnosis, "pagination overshoot") {
		t.Errorf("must not claim pagination overshoot when count=0; got %q", diagnosis)
	}
}
