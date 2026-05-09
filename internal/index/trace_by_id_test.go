package index

import (
	"context"
	"strings"
	"testing"

	"github.com/pincherMCP/pincher/internal/db"
)

// TestTraceByID_PicksExactSymbol pins the #5 fix: when multiple symbols
// share a short name (very common — Run, Handler, Open, Process, init),
// `Trace(name, ...)` falls back to GetSymbolsByName and picks the first
// match. That picks the wrong start node for blast-radius analysis if
// the caller already knows which symbol they want.
//
// `TraceByID(symbolID, ...)` skips the name lookup and traces from the
// exact symbol the caller passed. The `changes` tool relies on this so
// pre-commit blast radius is computed for the symbol that actually
// changed in the diff, not whichever same-named sibling sorts first.
func TestTraceByID_PicksExactSymbol(t *testing.T) {
	idx, store := newTestIndexer(t)

	pid := "trace-by-id-test"
	if err := store.UpsertProject(db.Project{ID: pid, Path: "/tmp/p", Name: "p"}); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	// Two symbols with identical name — the regression scenario.
	a := db.Symbol{ID: "pkgA::Run#Function", ProjectID: pid, FilePath: "a.go",
		Name: "Run", QualifiedName: "pkgA.Run", Kind: "Function", Language: "Go"}
	b := db.Symbol{ID: "pkgB::Run#Function", ProjectID: pid, FilePath: "b.go",
		Name: "Run", QualifiedName: "pkgB.Run", Kind: "Function", Language: "Go"}
	callerOfA := db.Symbol{ID: "pkgX::CallsA#Function", ProjectID: pid, FilePath: "x.go",
		Name: "CallsA", QualifiedName: "pkgX.CallsA", Kind: "Function", Language: "Go"}
	callerOfB := db.Symbol{ID: "pkgY::CallsB#Function", ProjectID: pid, FilePath: "y.go",
		Name: "CallsB", QualifiedName: "pkgY.CallsB", Kind: "Function", Language: "Go"}
	if err := store.BulkUpsertSymbols([]db.Symbol{a, b, callerOfA, callerOfB}); err != nil {
		t.Fatalf("upsert symbols: %v", err)
	}

	// Wire each caller to its specific Run. CallsA -> pkgA.Run; CallsB -> pkgB.Run.
	if err := store.BulkUpsertEdges([]db.Edge{
		{ProjectID: pid, FromID: callerOfA.ID, ToID: a.ID, Kind: "CALLS"},
		{ProjectID: pid, FromID: callerOfB.ID, ToID: b.ID, Kind: "CALLS"},
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	// TraceByID(a.ID, "inbound") MUST return CallsA only (not CallsB).
	hopsA, err := idx.TraceByID(context.Background(), a.ID, "inbound", 3, false)
	if err != nil {
		t.Fatalf("TraceByID(a): %v", err)
	}
	hasA, hasB := false, false
	for _, h := range hopsA {
		if h.Symbol.ID == callerOfA.ID {
			hasA = true
		}
		if h.Symbol.ID == callerOfB.ID {
			hasB = true
		}
	}
	if !hasA {
		t.Errorf("TraceByID(pkgA.Run): expected to find CallsA in hops; got %v", hopsA)
	}
	if hasB {
		t.Errorf("TraceByID(pkgA.Run): MUST NOT include CallsB (CallsB calls pkgB.Run, not pkgA.Run); got %v", hopsA)
	}

	// And TraceByID(b.ID) returns the symmetric result.
	hopsB, err := idx.TraceByID(context.Background(), b.ID, "inbound", 3, false)
	if err != nil {
		t.Fatalf("TraceByID(b): %v", err)
	}
	hasA, hasB = false, false
	for _, h := range hopsB {
		if h.Symbol.ID == callerOfA.ID {
			hasA = true
		}
		if h.Symbol.ID == callerOfB.ID {
			hasB = true
		}
	}
	if !hasB {
		t.Errorf("TraceByID(pkgB.Run): expected to find CallsB; got %v", hopsB)
	}
	if hasA {
		t.Errorf("TraceByID(pkgB.Run): MUST NOT include CallsA; got %v", hopsB)
	}
}

// TestTrace_NameWrapperStillResolvesByName proves the back-compat path:
// the `trace` MCP tool still takes a name (since callers there don't
// always have an ID), and the wrapper picks the first match — same
// documented heuristic as before. This pins that splitting Trace into
// Trace + TraceByID didn't change the name-based contract.
func TestTrace_NameWrapperStillResolvesByName(t *testing.T) {
	idx, store := newTestIndexer(t)

	pid := "trace-name-wrapper"
	if err := store.UpsertProject(db.Project{ID: pid, Path: "/tmp/p", Name: "p"}); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	if err := store.BulkUpsertSymbols([]db.Symbol{
		{ID: "pkgA::Greet#Function", ProjectID: pid, FilePath: "a.go",
			Name: "Greet", QualifiedName: "pkgA.Greet", Kind: "Function", Language: "Go"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Empty graph: no callers, but the name lookup still resolves.
	_, err := idx.Trace(context.Background(), pid, "Greet", "inbound", 3, false)
	if err != nil {
		t.Errorf("Trace(name=Greet) returned err: %v", err)
	}

	// Unknown name: error mentions the symbol that wasn't found.
	_, err = idx.Trace(context.Background(), pid, "NoSuchSymbol", "inbound", 3, false)
	if err == nil {
		t.Errorf("Trace(name=NoSuchSymbol): expected not-found error, got nil")
	} else if !strings.Contains(err.Error(), "NoSuchSymbol") {
		t.Errorf("error should name the missing symbol; got %v", err)
	}
}
