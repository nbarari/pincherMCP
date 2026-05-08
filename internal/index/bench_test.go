package index

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pincherMCP/pincher/internal/db"
)

// Benchmarks for the index pipeline against pinned corpora (#50 substrate).
//
// Run a single benchmark:
//   go test ./internal/index/ -bench=BenchmarkIndex_Cold -run=^$ -benchtime=3s
//
// Run the whole bench suite:
//   make bench
//
// Each benchmark uses one of the corpora committed under testdata/corpus/
// from #33. Same corpora as the snapshot tests, so latency numbers can be
// correlated to known-good symbol counts.
//
// What we measure:
//   - Cold index (no pre-existing DB) — the user-facing first-run experience
//   - Incremental re-index with zero changes — the hash-skip happy path
//   - Force re-index — every file re-parsed
//
// What we deliberately don't include in the per-op timer:
//   - DB.Open / migrate / WAL guardrail PRAGMAs (one-time setup cost)
//   - Walker initialisation
//   - Test harness mkdir / cleanup
//
// b.ReportAllocs() makes allocation count a primary signal — it's
// deterministic across runs (unlike wall-clock, which has GC noise).

func benchIndexerForCorpus(b *testing.B, corpusName string) (*Indexer, string, func()) {
	b.Helper()

	corpusPath, err := filepath.Abs("../../testdata/corpus/" + corpusName)
	if err != nil {
		b.Fatalf("abs corpus path: %v", err)
	}

	dataDir := b.TempDir()
	store, err := db.Open(dataDir)
	if err != nil {
		b.Fatalf("db.Open: %v", err)
	}
	idx := New(store)
	cleanup := func() { store.Close() }
	return idx, corpusPath, cleanup
}

// BenchmarkIndex_Cold measures the user-facing first-run cost: open a
// fresh DB, walk the corpus, extract every file, populate symbols + edges
// + FTS5 from scratch. This is the latency a user sees when they first
// point pincher at their project.
func BenchmarkIndex_Cold_GoProject(b *testing.B) { benchColdIndex(b, "go-project") }
func BenchmarkIndex_Cold_K8sOps(b *testing.B)    { benchColdIndex(b, "k8s-ops") }
func BenchmarkIndex_Cold_NodeMonorepo(b *testing.B) {
	benchColdIndex(b, "node-monorepo")
}

func benchColdIndex(b *testing.B, corpus string) {
	b.Helper()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Each iteration gets a fresh DB so we measure cold-index cost,
		// not "first iteration cold + rest are skip-noop".
		b.StopTimer()
		idx, corpusPath, cleanup := benchIndexerForCorpus(b, corpus)
		b.StartTimer()

		if _, err := idx.Index(context.Background(), corpusPath, false); err != nil {
			b.Fatalf("Index: %v", err)
		}

		b.StopTimer()
		cleanup()
		b.StartTimer()
	}
}

// BenchmarkIndex_Incremental_NoChange measures the hash-skip happy path.
// First Index() populates the DB; subsequent runs of the same corpus
// against the same DB MUST short-circuit at the hash check and skip
// every file. This is the cost of `idx.Watch()`'s 2s polling tick when
// nothing has changed — measured here at much higher rate than the
// real watcher would tick.
func BenchmarkIndex_Incremental_NoChange_GoProject(b *testing.B) {
	benchIncrementalNoChange(b, "go-project")
}
func BenchmarkIndex_Incremental_NoChange_K8sOps(b *testing.B) {
	benchIncrementalNoChange(b, "k8s-ops")
}
func BenchmarkIndex_Incremental_NoChange_NodeMonorepo(b *testing.B) {
	benchIncrementalNoChange(b, "node-monorepo")
}

func benchIncrementalNoChange(b *testing.B, corpus string) {
	b.Helper()
	b.ReportAllocs()

	idx, corpusPath, cleanup := benchIndexerForCorpus(b, corpus)
	defer cleanup()

	// Prime: first call populates the DB.
	if _, err := idx.Index(context.Background(), corpusPath, false); err != nil {
		b.Fatalf("prime Index: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.Index(context.Background(), corpusPath, false); err != nil {
			b.Fatalf("incremental Index: %v", err)
		}
	}
}

// BenchmarkIndex_Force measures the force-rebuild path: every file
// re-parsed, every symbol re-emitted (DeleteSymbolsForFile + reinsert).
// Documents the cost of `pincher index --force` after a schema change
// or a "something looks wrong" reset.
func BenchmarkIndex_Force_GoProject(b *testing.B) { benchForceIndex(b, "go-project") }

func benchForceIndex(b *testing.B, corpus string) {
	b.Helper()
	b.ReportAllocs()

	idx, corpusPath, cleanup := benchIndexerForCorpus(b, corpus)
	defer cleanup()

	// Prime: first call populates the DB.
	if _, err := idx.Index(context.Background(), corpusPath, false); err != nil {
		b.Fatalf("prime Index: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.Index(context.Background(), corpusPath, true); err != nil {
			b.Fatalf("force Index: %v", err)
		}
	}
}
