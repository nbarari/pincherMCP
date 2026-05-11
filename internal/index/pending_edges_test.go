package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #457: persist deferred edge candidates so incremental re-index
// preserves cross-file CALLS even when the caller's file is
// hash-skipped. Pre-#457 the watcher worked around this by clearing
// the caller's hash via invalidateReferencers — but that only caught
// one-hop ripples (#427). With pending_edges, the caller's candidate
// rows survive across runs, so the resolver can rebuild the edge
// regardless of which files actually re-extract.

func TestIndex_PendingEdges_PreservesCrossFileCALLSWithoutInvalidation(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "callee.go", "package mypkg\n\nfunc Foo() {}\n")
	writeFile(t, dir, "caller.go", "package mypkg\n\nfunc Bar() {\n\tFoo()\n}\n")

	// Initial full index. pending_edges now holds caller.go's deferred
	// "Bar -> Foo" candidate.
	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	_ = db.ProjectIDFromPath(dir) // not needed for this assertion; EdgesFrom is global by ID

	calleeID := db.MakeSymbolID("callee.go", "mypkg.Foo", "Function")
	callerID := db.MakeSymbolID("caller.go", "mypkg.Bar", "Function")
	if pre, _ := store.EdgesFrom(callerID, []string{"CALLS"}); len(pre) == 0 {
		t.Fatalf("expected initial Bar→Foo CALLS edge; got none")
	}

	// Edit callee.go only. Critically, do NOT call invalidateReferencers —
	// the old #427 partial fix's mechanism. With pending_edges, the
	// edge survives via the persisted candidate alone.
	time.Sleep(20 * time.Millisecond)
	writeFile(t, dir, "callee.go", "package mypkg\n\n// edited\nfunc Foo() {}\n")
	future := time.Now().Add(10 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "callee.go"), future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("second Index: %v", err)
	}

	post, _ := store.EdgesFrom(callerID, []string{"CALLS"})
	foundFoo := false
	for _, e := range post {
		if e.ToID == calleeID {
			foundFoo = true
			break
		}
	}
	if !foundFoo {
		t.Errorf("Bar→Foo CALLS edge lost after callee-only re-index; pending_edges should have preserved the candidate. got %d edges: %v", len(post), post)
	}
}

func TestIndex_PendingEdges_GCdOnFileDeletion(t *testing.T) {
	// A file removed from disk must clear its pending_edges rows in
	// the tail-pass GC. Otherwise re-resolution would try to bind
	// stale FromQN→ToName candidates against the shrunk symbol set.
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "callee.go", "package mypkg\n\nfunc Foo() {}\n")
	writeFile(t, dir, "caller.go", "package mypkg\n\nfunc Bar() {\n\tFoo()\n}\n")

	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	projectID := db.ProjectIDFromPath(dir)

	// Pre-condition: caller.go has at least one CALLS candidate.
	pre, err := store.LoadPendingEdges(projectID, "CALLS")
	if err != nil {
		t.Fatalf("LoadPendingEdges: %v", err)
	}
	preCallerCount := 0
	for _, p := range pre {
		if p.FromFile == "caller.go" {
			preCallerCount++
		}
	}
	if preCallerCount == 0 {
		t.Fatalf("expected ≥1 pending_edges row for caller.go before deletion; got %d", preCallerCount)
	}

	// Delete caller.go and re-index. Tail-pass GC should clear its
	// pending_edges rows.
	if err := os.Remove(filepath.Join(dir, "caller.go")); err != nil {
		t.Fatalf("remove caller.go: %v", err)
	}
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("second Index: %v", err)
	}

	post, _ := store.LoadPendingEdges(projectID, "CALLS")
	for _, p := range post {
		if p.FromFile == "caller.go" {
			t.Errorf("pending_edges row for deleted caller.go survived GC: %+v", p)
		}
	}
}
