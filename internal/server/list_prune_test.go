package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #302: list prune_dead=true physically deletes projects whose
// on-disk path no longer exists. Defaults are unchanged — pruning
// is opt-in.

// Default behaviour: dead-on-disk projects are HIDDEN but not deleted.
func TestHandleList_DefaultDoesNotPrune(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{
		ID: "ghost", Path: filepath.Join(t.TempDir(), "no-such-dir"),
		Name: "ghost", IndexedAt: time.Now(),
	})

	result, err := srv.handleList(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, result)
	if body["count"].(float64) != 0 {
		t.Errorf("count = %v, want 0 (ghost project hidden)", body["count"])
	}
	if _, ok := body["pruned"]; ok {
		t.Errorf("default call must not include `pruned` field; got %v", body["pruned"])
	}
	// Confirm the row is still in the DB.
	projects, _ := store.ListProjects()
	if len(projects) != 1 {
		t.Errorf("expected ghost project still in DB; got %d projects", len(projects))
	}
}

// prune_dead=true physically removes the missing-path projects and
// returns their ids in `pruned`.
func TestHandleList_PruneDeadDeletesMissingProjects(t *testing.T) {
	srv, store, _ := newTestServer(t)
	deadDir := filepath.Join(t.TempDir(), "dead-dir-doesnt-exist")
	store.UpsertProject(db.Project{
		ID: "ghost", Path: deadDir, Name: "ghost", IndexedAt: time.Now(),
	})
	aliveDir := t.TempDir()
	store.UpsertProject(db.Project{
		ID: "alive", Path: aliveDir, Name: "alive", IndexedAt: time.Now(),
	})

	result, err := srv.handleList(context.Background(), makeReq(map[string]any{
		"prune_dead": true,
	}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, result)
	pruned, _ := body["pruned"].([]any)
	if len(pruned) != 1 || pruned[0] != "ghost" {
		t.Errorf("pruned = %v, want [ghost]", pruned)
	}
	if body["count"].(float64) != 1 {
		t.Errorf("count = %v, want 1 (alive only)", body["count"])
	}

	// Confirm the ghost row is GONE from the DB.
	projects, _ := store.ListProjects()
	if len(projects) != 1 {
		t.Errorf("expected 1 project after prune; got %d", len(projects))
	}
	for _, p := range projects {
		if p.ID == "ghost" {
			t.Errorf("ghost project still in DB after prune_dead=true: %v", p)
		}
	}
}

// prune_dead=true with no dead projects returns an empty `pruned`
// array (not nil) — distinguishes "I tried to prune and there was
// nothing" from "I never tried to prune".
func TestHandleList_PruneDeadEmptyArrayWhenNothingDead(t *testing.T) {
	srv, store, _ := newTestServer(t)
	aliveDir := t.TempDir()
	store.UpsertProject(db.Project{
		ID: "alive", Path: aliveDir, Name: "alive", IndexedAt: time.Now(),
	})

	result, err := srv.handleList(context.Background(), makeReq(map[string]any{
		"prune_dead": true,
	}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, result)
	pruned, ok := body["pruned"].([]any)
	if !ok {
		t.Fatalf("pruned field missing despite prune_dead=true; body: %v", body)
	}
	if len(pruned) != 0 {
		t.Errorf("pruned = %v, want empty array", pruned)
	}
}

// #378: include_dead and prune_dead are orthogonal. Pre-#378 the
// prune branch was nested inside !includeDead, so combining the two
// silently no-op'd the prune — surprising for a caller who naturally
// reads "show dead rows AND delete them" as audit + cleanup.
//
// Post-fix: the dead row appears in the response (via include_dead)
// AND is removed from the DB (via prune_dead). The pruned field
// reports exactly what got deleted.
func TestHandleList_IncludeDeadAndPruneDead_BothHonored(t *testing.T) {
	srv, store, _ := newTestServer(t)
	deadDir := filepath.Join(t.TempDir(), "dead-dir-doesnt-exist")
	store.UpsertProject(db.Project{
		ID: "ghost", Path: deadDir, Name: "ghost", IndexedAt: time.Now(),
	})

	result, err := srv.handleList(context.Background(), makeReq(map[string]any{
		"include_dead": true,
		"prune_dead":   true,
	}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, result)
	if body["count"].(float64) != 1 {
		t.Errorf("count = %v, want 1 (ghost surfaced via include_dead)", body["count"])
	}
	// Pruned reports what was deleted from the DB.
	pruned, ok := body["pruned"].([]any)
	if !ok {
		t.Fatalf("pruned field missing despite prune_dead=true; body: %v", body)
	}
	if len(pruned) != 1 || pruned[0] != "ghost" {
		t.Errorf("pruned = %v, want [ghost]", pruned)
	}
	// Ghost is GONE from the DB — even though it appeared in the response.
	projects, _ := store.ListProjects()
	if len(projects) != 0 {
		t.Errorf("ghost should have been pruned despite include_dead=true; %d projects remain", len(projects))
	}
}

// #378 dogfood repro: caller passes both flags wanting "audit + cleanup".
// Pre-fix, dropped + 0 entries returned (silently filtered). Post-fix,
// the dead row IS shown AND deleted, so subsequent calls see it gone.
func TestHandleList_IncludeDeadAndPruneDead_DogfoodRepro(t *testing.T) {
	srv, store, _ := newTestServer(t)
	deadDir := filepath.Join(t.TempDir(), "NonexistentProjectThatDoesNotExist")
	store.UpsertProject(db.Project{
		ID: "ghost", Path: deadDir, Name: "ghost", IndexedAt: time.Now(),
	})
	aliveDir := t.TempDir()
	store.UpsertProject(db.Project{
		ID: "alive", Path: aliveDir, Name: "alive", IndexedAt: time.Now(),
	})

	// Call shape from the dogfood report: active=false (see all),
	// include_dead=true (don't filter dead), prune_dead=true (clean up).
	result, err := srv.handleList(context.Background(), makeReq(map[string]any{
		"active":       false,
		"include_dead": true,
		"prune_dead":   true,
	}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, result)
	pruned, _ := body["pruned"].([]any)
	if len(pruned) != 1 || pruned[0] != "ghost" {
		t.Errorf("pruned = %v, want [ghost] (the dogfood case must actually delete)", pruned)
	}

	// Subsequent call with no flags: only alive remains.
	result2, err := srv.handleList(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleList second call: %v", err)
	}
	body2 := decode(t, result2)
	if body2["count"].(float64) != 1 {
		t.Errorf("after prune, count = %v, want 1 (alive only)", body2["count"])
	}
}
