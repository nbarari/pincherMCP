package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1067: companion to #1010 (doctor ratio extension). The #1040
// architecture ghost-extraction diagnosis fires only when both
// hotspots AND entry-points are empty AND edges == 0 — a strict gate
// that misses ratio-class ghosts that leak a handful of edges from
// one file (producing hotspots). fools-gold-pirate at 11181/9 was
// the dogfood-discovered case. Now: a ratio-class warning fires
// regardless of hotspots existing, so the response is honest about
// the bulk of the corpus having no edges.

func TestHandleArchitecture_LowRatio_AttachesGhostWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-ratio"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
		FileCount: 50, SymCount: 5000, EdgeCount: 2, // ratio 0.0004 — well below 0.001
	})
	srv.sessionID = pid

	res, err := srv.handleArchitecture(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected _meta on ratio-ghost response; got body=%v", body)
	}
	warnings, _ := meta["warnings"].([]any)
	found := false
	for _, w := range warnings {
		ws, _ := w.(string)
		if strings.Contains(ws, "ratio") && strings.Contains(ws, "ghost-extraction") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ratio-ghost warning naming the ratio + ghost-extraction signature; got warnings=%v", warnings)
	}
}

// Control: a healthy project (ratio above 0.001) must NOT attach the
// ratio warning. Anchored at 0.01 — well above the floor.
func TestHandleArchitecture_HealthyRatio_NoGhostWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-healthy"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
		FileCount: 50, SymCount: 5000, EdgeCount: 5000, // ratio 1.0
	})
	srv.sessionID = pid

	res, err := srv.handleArchitecture(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return // no _meta at all is fine for an all-healthy project
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		ws, _ := w.(string)
		if strings.Contains(ws, "ghost-extraction") {
			t.Errorf("healthy project must not get ghost-extraction warning; got %q", ws)
		}
	}
}

// Control: a small project below the symbol threshold (< 1000) must
// NOT trip the ratio warning even at a bad ratio. Pure-config /
// pure-docs repos can legitimately land here.
func TestHandleArchitecture_SmallProject_NoGhostWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-small"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
		FileCount: 20, SymCount: 500, EdgeCount: 0, // ratio 0 but below sym threshold
	})
	srv.sessionID = pid

	res, err := srv.handleArchitecture(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		ws, _ := w.(string)
		if strings.Contains(ws, "ratio") && strings.Contains(ws, "ghost-extraction") {
			t.Errorf("small project below sym threshold must not get ratio warning; got %q", ws)
		}
	}
}
