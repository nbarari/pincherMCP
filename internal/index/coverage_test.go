package index

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pincherMCP/pincher/internal/ast"
	"github.com/pincherMCP/pincher/internal/db"
)

// Targeted coverage tests for index-package paths that lacked direct
// exercise. Functional tests live in the various *_test.go files; this
// file fills coverage gaps surfaced by `go tool cover -func`.

// ─────────────────────────────────────────────────────────────────────────────
// recordExtractionHeuristics: synthesises both diagnostic shapes
// ─────────────────────────────────────────────────────────────────────────────

func TestRecordExtractionHeuristics_NegativeByteRange(t *testing.T) {
	store := newDBStore(t)
	if err := store.UpsertProject(db.Project{ID: "rh-proj", Path: "/rh", Name: "rh", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	idx := New(store)

	// Synthetic FileResult with one symbol whose byte range is inverted.
	// recordExtractionHeuristics should record a byte_range_negative row.
	bad := &ast.FileResult{
		Symbols: []ast.ExtractedSymbol{
			{Name: "Bad", QualifiedName: "pkg.Bad", Kind: "Function",
				StartByte: 100, EndByte: 50},
		},
	}
	recordExtractionHeuristics(idx, "rh-proj", "Go", "bad.go", bad)

	failures, err := store.ListExtractionFailures("rh-proj", 10)
	if err != nil {
		t.Fatalf("ListExtractionFailures: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(failures))
	}
	if failures[0].Reason != "byte_range_negative" {
		t.Errorf("reason = %q, want byte_range_negative", failures[0].Reason)
	}
}

func TestRecordExtractionHeuristics_QNCollision(t *testing.T) {
	store := newDBStore(t)
	if err := store.UpsertProject(db.Project{ID: "qn-proj", Path: "/qn", Name: "qn", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	idx := New(store)

	// Healthy symbols, but QNCollisions map populated — simulates the
	// post-disambiguation #115 path where the extractor produced
	// duplicates and disambiguator suffixed them.
	r := &ast.FileResult{
		Symbols: []ast.ExtractedSymbol{
			{Name: "F", QualifiedName: "pkg.F", Kind: "Function", StartByte: 0, EndByte: 10},
		},
		QNCollisions: map[string]int{"pkg.F": 3, "pkg.G": 2},
	}
	recordExtractionHeuristics(idx, "qn-proj", "Go", "dup.go", r)

	failures, err := store.ListExtractionFailures("qn-proj", 10)
	if err != nil {
		t.Fatalf("ListExtractionFailures: %v", err)
	}
	// Exactly one row: the worst offender (pkg.F, count=3).
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1 (one row even with multiple collisions)", len(failures))
	}
	if failures[0].Reason != "qualified_name_collision" {
		t.Errorf("reason = %q, want qualified_name_collision", failures[0].Reason)
	}
	if !strings.Contains(failures[0].Details, "pkg.F") {
		t.Errorf("details should mention worst offender pkg.F: %s", failures[0].Details)
	}
}

func TestRecordExtractionHeuristics_NoOpOnHealthyResult(t *testing.T) {
	store := newDBStore(t)
	if err := store.UpsertProject(db.Project{ID: "ok-proj", Path: "/ok", Name: "ok", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	idx := New(store)

	healthy := &ast.FileResult{
		Symbols: []ast.ExtractedSymbol{
			{Name: "Good", QualifiedName: "pkg.Good", Kind: "Function",
				StartByte: 0, EndByte: 50},
		},
		// QNCollisions empty.
	}
	recordExtractionHeuristics(idx, "ok-proj", "Go", "good.go", healthy)

	failures, err := store.ListExtractionFailures("ok-proj", 10)
	if err != nil {
		t.Fatalf("ListExtractionFailures: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("healthy file should record 0 failures, got %d", len(failures))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// processExists: covers the PID validation branches
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessExists_ZeroAndNegative(t *testing.T) {
	if processExists(0) {
		t.Error("PID 0 should not exist")
	}
	if processExists(-1) {
		t.Error("PID -1 should not exist")
	}
}

func TestProcessExists_CurrentProcess(t *testing.T) {
	pid := os.Getpid()
	if !processExists(pid) {
		t.Errorf("current process PID %d should exist", pid)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// acquireProjectLock: covers stale-reclaim, corrupt-payload, and contention
// ─────────────────────────────────────────────────────────────────────────────

func TestAcquireProjectLock_Basic(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireProjectLock(dir, "proj-a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	// Lockfile should exist.
	path := projectLockPath(dir, "proj-a")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lockfile not created: %v", err)
	}

	// Second concurrent acquire on same project should fail (live PID
	// holds the lock — we just got it from this same process).
	_, err = acquireProjectLock(dir, "proj-a")
	if err == nil {
		t.Error("second acquire should fail while first is held")
	}
}

func TestAcquireProjectLock_StaleReclaimByCorruptPayload(t *testing.T) {
	dir := t.TempDir()
	path := projectLockPath(dir, "stale-proj")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Write garbage as the lockfile contents — readLockInfo fails →
	// isStaleLockfile returns true → reclaim path engages.
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	release, err := acquireProjectLock(dir, "stale-proj")
	if err != nil {
		t.Fatalf("acquire after corrupt-payload reclaim: %v", err)
	}
	release()
}

func TestAcquireProjectLock_StaleReclaimByDeadPID(t *testing.T) {
	dir := t.TempDir()
	path := projectLockPath(dir, "dead-pid")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// PID 999999 is overwhelmingly likely not running. The reclaim path
	// inspects payload validity first, then probes the PID; an unreachable
	// PID is treated as "stale" and the lock is reclaimed.
	corpse := lockInfo{PID: 999999, StartTime: time.Now().Unix(), ProjectID: "dead-pid"}
	data, _ := json.Marshal(corpse)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	release, err := acquireProjectLock(dir, "dead-pid")
	if err != nil {
		t.Fatalf("acquire after dead-PID reclaim: %v", err)
	}
	release()
}

// ─────────────────────────────────────────────────────────────────────────────
// isStaleLockfile: covers the "old mtime" branch directly (no PID probe needed)
// ─────────────────────────────────────────────────────────────────────────────

func TestIsStaleLockfile_OldMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.lock")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Backdate mtime past the staleness threshold.
	old := time.Now().Add(-2 * lockStaleAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if !isStaleLockfile(path) {
		t.Error("lockfile older than lockStaleAge should be reported stale")
	}
}

func TestIsStaleLockfile_MissingFile(t *testing.T) {
	if isStaleLockfile(filepath.Join(t.TempDir(), "ghost.lock")) {
		t.Error("missing file is not stale (it's just gone)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Watch: covers the active-skip branch + the cancel-via-ctx exit
// ─────────────────────────────────────────────────────────────────────────────

func TestWatch_ExitsOnContextCancel(t *testing.T) {
	store := newDBStore(t)
	idx := New(store)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		idx.Watch(ctx)
		close(done)
	}()
	// Give the goroutine a moment to enter its select.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
		// Expected: Watch returned on ctx.Done.
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not exit within 2s of ctx cancel")
	}
}

func TestAnyActive_FalseOnIdleIndexer(t *testing.T) {
	store := newDBStore(t)
	idx := New(store)
	if idx.anyActive() {
		t.Error("fresh Indexer should have no active projects")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// hasChanges: covers the "no changes since last index" path
// ─────────────────────────────────────────────────────────────────────────────

func TestHasChanges_DetectsTouchedFile(t *testing.T) {
	store := newDBStore(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	idx := New(store)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	pid := db.ProjectIDFromPath(dir)
	p, err := store.GetProject(pid)
	if err != nil || p == nil {
		t.Fatalf("GetProject: %v / %v", err, p)
	}

	// Touch the file with a clearly-future mtime so hasChanges definitely
	// observes it as newer than IndexedAt regardless of FS mtime resolution.
	future := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.go"), future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if !idx.hasChanges(*p) {
		t.Error("hasChanges should return true after touching a source file with future mtime")
	}
}

func TestHasChanges_MissingDirReturnsFalse(t *testing.T) {
	store := newDBStore(t)
	idx := New(store)
	bogus := db.Project{ID: "ghost", Path: filepath.Join(t.TempDir(), "does-not-exist"), IndexedAt: time.Now()}
	if idx.hasChanges(bogus) {
		t.Error("hasChanges on missing dir should return false (ReadDir fails → no signal)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func newDBStore(t *testing.T) *db.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
