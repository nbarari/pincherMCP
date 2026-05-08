package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pincherMCP/pincher/internal/db"
	"github.com/pincherMCP/pincher/internal/index"
)

// Benchmarks for the MCP tool handlers against pre-indexed pinned corpora
// (#50 substrate).
//
// What we measure:
//   - handleSymbol — the marquee O(1) byte-offset retrieval claim
//   - handleSearch — FTS5 BM25 hit
//   - handleQuery — Cypher (node scan, single-hop JOIN, BFS)
//   - handleArchitecture — whole-project graph scan
//
// Every benchmark indexes the corpus ONCE in setup and times only the
// handler call, so the numbers reflect query-time cost (the user-facing
// latency) not index-time cost.

func benchSetup(b *testing.B, corpusName string) (*Server, *db.Store, string) {
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
	idx := index.New(store)
	if _, err := idx.Index(context.Background(), corpusPath, false); err != nil {
		b.Fatalf("Index: %v", err)
	}

	srv := New(store, idx, "bench")
	projectID := db.ProjectIDFromPath(corpusPath)
	b.Cleanup(func() { store.Close() })
	return srv, store, projectID
}

// BenchmarkHandleSymbol — single byte-offset retrieval. The README claim
// is "<1ms"; this measures it.
func BenchmarkHandleSymbol_GoProject(b *testing.B) {
	srv, _, projectID := benchSetup(b, "go-project")

	// Pick a known symbol from the corpus.
	symbolID := "internal/auth/auth.go::auth.Open#Function"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
			"id":      symbolID,
			"project": projectID,
		}))
		if err != nil {
			b.Fatalf("handleSymbol: %v", err)
		}
	}
}

// BenchmarkHandleSearch_BM25 — FTS5 query. README claim: ~1ms.
func BenchmarkHandleSearch_BM25_GoProject(b *testing.B) {
	srv, _, projectID := benchSetup(b, "go-project")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
			"query":   "Open",
			"project": projectID,
		}))
		if err != nil {
			b.Fatalf("handleSearch: %v", err)
		}
	}
}

// BenchmarkHandleSearch_BM25_K8sOps — search across YAML/JSON Settings.
// Tests the post-#23 path with a heavy Setting-symbol corpus. Different
// BM25 distribution profile from go-project.
func BenchmarkHandleSearch_BM25_K8sOps(b *testing.B) {
	srv, _, projectID := benchSetup(b, "k8s-ops")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
			"query":   "image",
			"project": projectID,
		}))
		if err != nil {
			b.Fatalf("handleSearch: %v", err)
		}
	}
}

// BenchmarkHandleQuery_NodeScan — Cypher node-scan path (no edge pattern).
// README claim: sub-ms.
func BenchmarkHandleQuery_NodeScan_GoProject(b *testing.B) {
	srv, _, projectID := benchSetup(b, "go-project")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
			"cypher":  "MATCH (f:Function) RETURN f.name",
			"project": projectID,
		}))
		if err != nil {
			b.Fatalf("handleQuery: %v", err)
		}
	}
}

// BenchmarkHandleQuery_SingleHopJoin — Cypher JOIN path. README claim: 2ms.
func BenchmarkHandleQuery_SingleHopJoin_GoProject(b *testing.B) {
	srv, _, projectID := benchSetup(b, "go-project")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
			"cypher":  "MATCH (a:Function)-[:CALLS]->(b) RETURN a.name, b.name",
			"project": projectID,
		}))
		if err != nil {
			b.Fatalf("handleQuery: %v", err)
		}
	}
}

// BenchmarkHandleArchitecture — whole-project graph scan. README claim:
// 12ms for this codebase. On the go-project corpus expect much faster
// (smaller scale).
func BenchmarkHandleArchitecture_GoProject(b *testing.B) {
	srv, _, projectID := benchSetup(b, "go-project")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := srv.handleArchitecture(context.Background(), makeReq(map[string]any{
			"project": projectID,
		}))
		if err != nil {
			b.Fatalf("handleArchitecture: %v", err)
		}
	}
}
