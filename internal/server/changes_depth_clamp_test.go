package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #877: `changes` accepted any depth and passed it straight to
// TraceByID, which silently coerced out-of-range values (depth=0,
// depth=99) to 3. The user got a depth-3 blast radius while believing
// they got what they asked for. Mirrors the trace depth clamp (#703).
// `changes` now clamps to [1, 5] at the handler level and surfaces
// `_meta.warnings` so the silent coercion turns into a teachable error.

func TestHandleChanges_DepthTooHigh_ClampsAndWarns(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	repoDir := setupChangesGitRepo(t)
	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "depth-too-high", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope": "all",
		"depth": 99,
	}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	if !warningContains(ws, "depth=99 clamped to 5") {
		t.Errorf("expected depth-clamp warning naming 99 → 5; got warnings=%v", ws)
	}
}

func TestHandleChanges_DepthZero_ClampsAndWarns(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	repoDir := setupChangesGitRepo(t)
	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "depth-zero", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope": "all",
		"depth": 0,
	}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	if !warningContains(ws, "depth=0 clamped to 1") {
		t.Errorf("expected depth-clamp warning naming 0 → 1; got warnings=%v", ws)
	}
}

// Control: depth in range produces no clamp warning.
func TestHandleChanges_DepthInRange_NoClampWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	repoDir := setupChangesGitRepo(t)
	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "depth-in-range", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope": "all",
		"depth": 3,
	}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	for _, w := range ws {
		if s, ok := w.(string); ok && strings.Contains(s, "depth=") && strings.Contains(s, "clamped") {
			t.Errorf("in-range depth=3 must not trip the clamp warning; got %q", s)
		}
	}
}
