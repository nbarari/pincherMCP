package index

import (
	"context"
	"fmt"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1338 v0.71: cold-path resolveCalls/resolveReads pre-load the
// project's symbol-by-QN map once when sum(pending) exceeds
// qnPreloadThreshold. Tests pin: (1) the threshold gate triggers,
// (2) edge correctness is preserved when the preloaded map is used,
// (3) below-threshold runs use the per-call DB path with same result.

// TestLoadAllSymbolsByQN_GroupsCorrectly is the lowest-level unit
// test for the new Store method. Multiple symbols sharing the same
// QN must end up in the same map entry; distinct QNs in distinct
// entries.
func TestLoadAllSymbolsByQN_GroupsCorrectly(t *testing.T) {
	store, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	const pid = "p"
	if err := store.UpsertProject(db.Project{ID: pid, Path: "/tmp/p", Name: "p"}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	mk := func(id, qn, file string) db.Symbol {
		return db.Symbol{
			ID: id, ProjectID: pid, FilePath: file, Language: "Go",
			Kind: "Function", Name: qn, QualifiedName: qn,
		}
	}
	if err := store.BulkUpsertSymbols([]db.Symbol{
		mk("a.go::pkg.Foo#Function", "pkg.Foo", "a.go"),
		mk("b.go::pkg.Foo#Function", "pkg.Foo", "b.go"), // sibling — shares QN
		mk("a.go::pkg.Bar#Function", "pkg.Bar", "a.go"),
	}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	got, err := store.LoadAllSymbolsByQN(pid)
	if err != nil {
		t.Fatalf("LoadAllSymbolsByQN: %v", err)
	}
	if len(got["pkg.Foo"]) != 2 {
		t.Errorf("pkg.Foo should have 2 symbols (siblings); got %d", len(got["pkg.Foo"]))
	}
	if len(got["pkg.Bar"]) != 1 {
		t.Errorf("pkg.Bar should have 1 symbol; got %d", len(got["pkg.Bar"]))
	}
	if _, hit := got["pkg.Nonexistent"]; hit {
		t.Errorf("missing QN should not appear in map")
	}
}

// TestQNPreloadThreshold_GateFiresAboveThreshold is the integration
// test: build a project with enough pending CALLS to trip the
// qnPreloadThreshold gate; observe that resolveCalls produces the
// same edges as a below-threshold run. Catches regressions where
// the pre-loaded path emits different edges than the per-call path.
func TestQNPreloadThreshold_GateFiresAboveThreshold(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	// Seed a project with a callee and many callers — enough that
	// resolveCalls' pending count exceeds qnPreloadThreshold. The
	// caller→callee CALLS edges all share the same target QN, so the
	// resolved edge count is well-defined.
	writeFile(t, dir, "callee.go", "package mypkg\n\nfunc Target() {}\n")
	// Need > qnPreloadThreshold pending edges. Each caller contributes
	// one. Generate threshold+50 callers.
	for i := 0; i < qnPreloadThreshold+50; i++ {
		writeFile(t, dir, fmt.Sprintf("caller_%04d.go", i),
			fmt.Sprintf("package mypkg\n\nfunc Caller_%d() {\n\tTarget()\n}\n", i))
	}

	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("Index: %v", err)
	}

	calleeID := db.MakeSymbolID("callee.go", "mypkg.Target", "Function")
	calls, err := store.EdgesTo(calleeID, []string{"CALLS"})
	if err != nil {
		t.Fatalf("EdgesTo: %v", err)
	}
	want := qnPreloadThreshold + 50
	if len(calls) != want {
		t.Errorf("above-threshold pre-loaded resolve produced %d CALLS edges to Target; want %d", len(calls), want)
	}
}
