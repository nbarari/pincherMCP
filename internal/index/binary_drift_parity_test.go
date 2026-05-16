package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #986 regression guard: the symbol count produced by a binary-drift-
// triggered reindex (Index called by maybeReindexOnDrift on a project
// whose stored binary_version differs from the running binary) MUST
// match the symbol count produced by an immediately-following explicit
// `force=true` call. Both code paths flow through the same
// Indexer.Index function with force=true, so they should be byte-
// identical in extraction output.
//
// The user's reported repro (1830 vs 5815 symbols, 30% / 70% split on a
// 501-file corpus) was observed on v0.57.0-26-g6e9bc67. The exact
// cause is still uncertain (see #986 investigation comment 2026-05-15
// — disproved hypothesis: cross-file resolvers don't gate on force).
// This test pins the parity contract on a multi-file corpus so a
// regression at any future change to the drift path fails CI rather
// than reaching the dogfood loop silently.
//
// If this test ever fails on master, the gap reproduces in unit
// scope and the next investigator has a deterministic starting point.

func TestIndex_BinaryDriftParity_MultiFile(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()

	dir := t.TempDir()
	// 60 Go files × ~10 symbols each = ~600 symbols, well past the
	// 500-symbol flushBuffers threshold so the drift-vs-explicit
	// comparison exercises at least one mid-run flush. The user's
	// reported repro was at 501 files / 5800 symbols.
	const numFiles = 60
	for i := 0; i < numFiles; i++ {
		var src string
		src = fmt.Sprintf("package main\n\n")
		for j := 0; j < 10; j++ {
			src += fmt.Sprintf("func f%d_%d() {\n\tf%d_%d()\n}\n",
				i, j, i, (j+1)%10)
		}
		if err := os.WriteFile(
			filepath.Join(dir, fmt.Sprintf("f%02d.go", i)),
			[]byte(src), 0o600,
		); err != nil {
			t.Fatalf("write f%02d.go: %v", i, err)
		}
	}

	// Pass 1: index with binary v0.9.0.
	idx1 := New(store)
	idx1.SetBinaryVersion("0.9.0")
	if _, err := idx1.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("initial Index: %v", err)
	}
	projectID := projectIDForTest(dir)
	driftCounts, err := captureCounts(store, projectID)
	if err != nil {
		t.Fatalf("capture initial counts: %v", err)
	}

	// Pass 2: drift-triggered reindex — new binary version, force=true.
	// This mirrors maybeReindexOnDrift in server.go (Index with
	// force=true after the binary version stamp changed).
	idx2 := New(store)
	idx2.SetBinaryVersion("0.10.0")
	r2, err := idx2.Index(context.Background(), dir, true)
	if err != nil {
		t.Fatalf("drift-triggered Index (force=true): %v", err)
	}
	driftCounts2, err := captureCounts(store, projectID)
	if err != nil {
		t.Fatalf("capture drift counts: %v", err)
	}
	_ = r2

	// Pass 3: immediate explicit force=true on the same binary version.
	// In the bug repro, this is what the user runs after seeing the
	// drift reindex's truncated symbol count.
	idx3 := New(store)
	idx3.SetBinaryVersion("0.10.0")
	if _, err := idx3.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("explicit force=true Index: %v", err)
	}
	explicitCounts, err := captureCounts(store, projectID)
	if err != nil {
		t.Fatalf("capture explicit counts: %v", err)
	}

	// Parity assertions: drift-triggered and explicit-force runs must
	// produce identical symbol + edge counts.
	if driftCounts2.symbols != explicitCounts.symbols {
		t.Errorf("#986 parity gap: drift-triggered Index produced %d symbols, explicit force=true produced %d. Same corpus, same binary, both force=true — these MUST match.",
			driftCounts2.symbols, explicitCounts.symbols)
	}
	if driftCounts2.edges != explicitCounts.edges {
		t.Errorf("#986 parity gap: drift-triggered Index produced %d edges, explicit force=true produced %d.",
			driftCounts2.edges, explicitCounts.edges)
	}

	// Sanity: explicit force on a corpus with cross-file calls must
	// produce non-trivial edge count (the cycle fn0 → fn1 → … → fn19 →
	// fn0 has 20 CALLS edges). If explicitCounts.edges is 0, the
	// resolver itself is broken in this test setup and the parity
	// assertion above is vacuous.
	if explicitCounts.edges == 0 {
		t.Fatalf("test setup broken: explicit force=true produced 0 edges on a 20-cycle Go corpus")
	}

	_ = driftCounts
}

type indexCounts struct {
	symbols int
	edges   int
}

func captureCounts(store storeForCounts, projectID string) (indexCounts, error) {
	syms, edges, _, _, err := store.GraphStats(projectID)
	if err != nil {
		return indexCounts{}, err
	}
	return indexCounts{symbols: syms, edges: edges}, nil
}

// storeForCounts is the minimum store interface this test needs.
// Defined here (not in db) so the test stays self-contained.
type storeForCounts interface {
	GraphStats(projectID string) (int, int, map[string]int, map[string]int, error)
}

func projectIDForTest(dir string) string {
	return db.ProjectIDFromPath(dir)
}
