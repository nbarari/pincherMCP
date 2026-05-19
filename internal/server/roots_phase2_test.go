package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #1081 Phase 2 (v0.78) — multi-root background auto-index.
//
// After Phase 1 picks the session root from advertised file:// roots
// that pass IsBloatTrap(hookMode=true), Phase 2 fires a background
// Index() per remaining cleared root and records each outcome in
// s.autoIndexedRoots. Audit shape: positive (extra root gets auto-
// indexed), negative (bloat-trap root never fires Index), control
// (single-root case still doesn't fire auto-index), cross-check
// (advertised order preserved across both head + tail).

func TestClearedSessionRoots_ReturnsAllCleared(t *testing.T) {
	// Positive shape. Three roots — `/` (trap), tmpA (cleared),
	// tmpB (cleared). clearedSessionRoots returns [tmpA, tmpB] in
	// advertised order.
	tmpA := t.TempDir()
	tmpB := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpA, ".git"), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpB, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	got := clearedSessionRoots([]*mcp.Root{
		{URI: uri("/")},
		{URI: uri(tmpA)},
		{URI: uri(tmpB)},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 cleared roots; got %d: %v", len(got), got)
	}
	if got[0] != tmpA || got[1] != tmpB {
		t.Errorf("ordering not preserved: got %v, want [%q, %q]", got, tmpA, tmpB)
	}
}

func TestClearedSessionRoots_AllTrapsReturnsEmpty(t *testing.T) {
	// Negative shape. Every advertised root fails IsBloatTrap →
	// empty result. detectRoot falls back to CWD on empty.
	tmpNoMarker := t.TempDir() // no .git / go.mod
	got := clearedSessionRoots([]*mcp.Root{
		{URI: uri("/")},
		{URI: uri(tmpNoMarker)},
	})
	if len(got) != 0 {
		t.Errorf("expected empty; got %v", got)
	}
}

func TestStartAutoIndex_RecordsIndexingThenIndexed(t *testing.T) {
	// Positive shape. Set up server + a real tmp project; fire
	// startAutoIndex; verify the root lands in autoIndexedRoots and
	// transitions to "indexed" status when the goroutine completes.
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, ".git"), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	// Drop a real source file so the indexer has something to extract.
	if err := os.WriteFile(filepath.Join(tmp, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	srv.startAutoIndex([]string{tmp})
	// #1605 v0.84: wait for the goroutine to finish before t.TempDir
	// cleanup runs. On Windows the indexer holds lockfiles in
	// <root>/locks/ and RemoveAll fails with "directory is not empty"
	// when the goroutine is still in flight.
	t.Cleanup(srv.WaitAutoIndex)

	// Wait up to 5s for the goroutine to flip "indexing" → "indexed".
	deadline := time.Now().Add(5 * time.Second)
	var got []autoIndexedRoot
	for time.Now().Before(deadline) {
		got = srv.AutoIndexedRoots()
		if len(got) == 1 && got[0].Status != "indexing" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 auto-indexed entry; got %d: %v", len(got), got)
	}
	if got[0].Root != tmp {
		t.Errorf("entry.Root = %q; want %q", got[0].Root, tmp)
	}
	if got[0].Status != "indexed" {
		t.Errorf("entry.Status = %q; want indexed (got %v)", got[0].Status, got[0])
	}
	if got[0].ProjectID == "" {
		t.Errorf("entry.ProjectID empty; should be derived from Root")
	}
}

func TestStartAutoIndex_PreservesAdvertisedOrder(t *testing.T) {
	// Cross-check. Fire startAutoIndex on multiple roots; the
	// recorded entries appear in advertised order even if the
	// goroutines complete out of order (the slot index is captured
	// at enqueue time).
	srv, _, _ := newTestServer(t)
	first := t.TempDir()
	second := t.TempDir()
	for _, d := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(d, "go.mod"), []byte("module test"), 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}

	srv.startAutoIndex([]string{first, second})
	// #1605 v0.84: same fix as TestStartAutoIndex_RecordsIndexingThenIndexed.
	// The test's assertion (slot order at enqueue time) doesn't need
	// the goroutines to complete first, but t.TempDir cleanup does on
	// Windows.
	t.Cleanup(srv.WaitAutoIndex)

	got := srv.AutoIndexedRoots()
	if len(got) != 2 {
		t.Fatalf("expected 2 entries; got %d: %v", len(got), got)
	}
	if got[0].Root != first || got[1].Root != second {
		t.Errorf("order not preserved: got [%q, %q]; want [%q, %q]",
			got[0].Root, got[1].Root, first, second)
	}
}
