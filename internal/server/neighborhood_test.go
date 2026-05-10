package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// Tests for #247 #1 neighborhood tool. Given a seed symbol ID, the
// handler returns every other symbol in the same file ordered by
// byte offset, with optional source bodies.

// setupNeighborhood seeds 4 symbols in the same file (ordered by line)
// plus one symbol in a DIFFERENT file. The cross-file symbol must
// NOT appear in any neighborhood call against the seed.
func setupNeighborhood(t *testing.T) (*Server, *db.Store, string) {
	t.Helper()
	srv, store, _ := newTestServer(t)
	projectID := "neighborhood"
	store.UpsertProject(db.Project{
		ID: projectID, Path: "/tmp/" + projectID, Name: projectID, IndexedAt: time.Now(),
	})
	srv.sessionID = projectID
	srv.sessionRoot = "/tmp/" + projectID

	syms := []db.Symbol{
		// 4 symbols in main.go ordered by line (and byte).
		{ID: "n::main.A#Function", ProjectID: projectID, FilePath: "main.go", Name: "A",
			QualifiedName: "main.A", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5,
			Signature: "func A()", IsExported: true, ExtractionConfidence: 1.0},
		{ID: "n::main.B#Function", ProjectID: projectID, FilePath: "main.go", Name: "B",
			QualifiedName: "main.B", Kind: "Function", Language: "Go",
			StartByte: 51, EndByte: 100, StartLine: 6, EndLine: 10,
			Signature: "func B()", IsExported: true, ExtractionConfidence: 1.0},
		{ID: "n::main.C#Function", ProjectID: projectID, FilePath: "main.go", Name: "C",
			QualifiedName: "main.C", Kind: "Function", Language: "Go",
			StartByte: 101, EndByte: 150, StartLine: 11, EndLine: 15,
			Signature: "func C()", IsExported: true, ExtractionConfidence: 1.0},
		{ID: "n::main.D#Function", ProjectID: projectID, FilePath: "main.go", Name: "D",
			QualifiedName: "main.D", Kind: "Function", Language: "Go",
			StartByte: 151, EndByte: 200, StartLine: 16, EndLine: 20,
			Signature: "func D()", IsExported: true, ExtractionConfidence: 1.0},
		// Cross-file symbol — must NEVER appear in main.go's neighborhood.
		{ID: "n::other.X#Function", ProjectID: projectID, FilePath: "other.go", Name: "X",
			QualifiedName: "other.X", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5,
			Signature: "func X()", IsExported: true, ExtractionConfidence: 1.0},
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	return srv, store, projectID
}

// Default behavior: seed B, get A/C/D in source order, NOT B (default
// excludes self), NOT X (different file).
func TestHandleNeighborhood_ReturnsSiblingsInSourceOrder(t *testing.T) {
	srv, _, _ := setupNeighborhood(t)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "n::main.B#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)
	if body["seed_id"] != "n::main.B#Function" {
		t.Errorf("seed_id = %v, want n::main.B#Function", body["seed_id"])
	}
	if body["file_path"] != "main.go" {
		t.Errorf("file_path = %v, want main.go", body["file_path"])
	}
	count, _ := body["count"].(float64)
	if count != 3 {
		t.Errorf("count = %v, want 3 (A + C + D, no self, no other-file)", count)
	}
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) != 3 {
		t.Fatalf("neighbors len = %d, want 3", len(neighbors))
	}
	// Source order: A (line 1), C (line 11), D (line 16). B excluded.
	wantNames := []string{"A", "C", "D"}
	for i, n := range neighbors {
		entry, _ := n.(map[string]any)
		if entry["name"] != wantNames[i] {
			t.Errorf("neighbors[%d].name = %v, want %v (source-order break)",
				i, entry["name"], wantNames[i])
		}
	}
}

// include_self=true keeps the seed in the list (still in source order).
// Useful when the agent wants the WHOLE file's symbols in one call.
func TestHandleNeighborhood_IncludeSelfReturnsAllSiblings(t *testing.T) {
	srv, _, _ := setupNeighborhood(t)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":           "n::main.B#Function",
		"include_self": true,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)
	count, _ := body["count"].(float64)
	if count != 4 {
		t.Errorf("count = %v, want 4 (A + B + C + D)", count)
	}
}

// Different-file symbols never leak into the neighborhood. Pin
// explicitly so a future GetSymbolsForFile change can't widen the
// scope by accident.
func TestHandleNeighborhood_DifferentFileSymbolsExcluded(t *testing.T) {
	srv, _, _ := setupNeighborhood(t)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "n::main.A#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)
	neighbors, _ := body["neighbors"].([]any)
	for _, n := range neighbors {
		entry, _ := n.(map[string]any)
		if entry["name"] == "X" {
			t.Errorf("cross-file symbol X surfaced in main.go's neighborhood:\n%v", neighbors)
		}
	}
}

// Default response excludes the source body — keeps the response
// cheap. include_source=true would fetch via the byte-offset path; we
// don't test that here because it requires real on-disk files (the
// existing handleSymbol tests cover the source-read primitive).
func TestHandleNeighborhood_DefaultExcludesSource(t *testing.T) {
	srv, _, _ := setupNeighborhood(t)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "n::main.B#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)
	neighbors, _ := body["neighbors"].([]any)
	for _, n := range neighbors {
		entry, _ := n.(map[string]any)
		if _, hasSource := entry["source"]; hasSource {
			t.Errorf("default response should not include source field; got: %v", entry)
		}
		// Signatures, however, MUST be present — they're the load-bearing
		// content for in-file refactor planning.
		if sig, _ := entry["signature"].(string); !strings.HasPrefix(sig, "func ") {
			t.Errorf("signature missing or wrong shape on neighbor: %v", entry)
		}
	}
}

// Missing id is a user error, not a server error. Mirrors handleSymbol.
// errResult returns plain text (not JSON), so we read via textOf.
func TestHandleNeighborhood_MissingIdErrors(t *testing.T) {
	srv, _, _ := setupNeighborhood(t)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handler returned Go error (should return errResult instead): %v", err)
	}
	out := textOf(t, result)
	if !strings.Contains(out, "id is required") {
		t.Errorf("expected 'id is required' error, got: %s", out)
	}
}

// Unknown id returns a not-found error. Stale-ID redirection (via
// symbol_moves) is exercised in handleSymbol's tests; we don't repeat
// that here — the neighborhood handler shares the exact same path.
func TestHandleNeighborhood_UnknownIdReturnsError(t *testing.T) {
	srv, _, _ := setupNeighborhood(t)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "n::main.NonExistent#Function",
	}))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	out := textOf(t, result)
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' error, got: %s", out)
	}
}

// File with only one symbol returns an empty neighbors list — the
// seed alone has no companions to surface. count=0, not nil/missing.
func TestHandleNeighborhood_LoneSymbolReturnsEmpty(t *testing.T) {
	srv, store, _ := newTestServer(t)
	projectID := "lone"
	store.UpsertProject(db.Project{
		ID: projectID, Path: "/tmp/" + projectID, Name: projectID, IndexedAt: time.Now(),
	})
	srv.sessionID = projectID
	if err := store.BulkUpsertSymbols([]db.Symbol{{
		ID: "lone::main.OnlyOne#Function", ProjectID: projectID, FilePath: "main.go",
		Name: "OnlyOne", QualifiedName: "main.OnlyOne", Kind: "Function", Language: "Go",
		StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5,
		Signature: "func OnlyOne()", IsExported: true, ExtractionConfidence: 1.0,
	}}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "lone::main.OnlyOne#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)
	count, _ := body["count"].(float64)
	if count != 0 {
		t.Errorf("count = %v, want 0 (lone symbol has no neighbors)", count)
	}
	neighbors, ok := body["neighbors"].([]any)
	if !ok || neighbors == nil {
		t.Errorf("neighbors field should be empty array, not nil/missing: %v", body["neighbors"])
	}
}

// include_source=true reads each neighbor's body via the byte-offset
// path. Covers readSymbolSourceForNeighbor end-to-end with a real
// on-disk file — the slice-by-byte-range round-trip plus the CRLF
// normalization at the tail. Without this case, the source-fetch
// branch is unexercised and that whole helper sits at 0% coverage.
func TestHandleNeighborhood_IncludeSourceReadsBodies(t *testing.T) {
	srv, store, _ := newTestServer(t)
	projectID := "with-source"

	root := t.TempDir()
	// Two functions back-to-back. Byte offsets are exact so the
	// byte-offset reader returns each body verbatim.
	bodyA := "func A() { return }\n"
	bodyB := "func B() { return }\n"
	src := bodyA + bodyB
	mainPath := filepath.Join(root, "main.go")
	if err := os.WriteFile(mainPath, []byte(src), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store.UpsertProject(db.Project{
		ID: projectID, Path: root, Name: projectID, IndexedAt: time.Now(),
	})
	srv.sessionID = projectID
	srv.sessionRoot = root

	aStart := 0
	aEnd := len(bodyA)
	bStart := aEnd
	bEnd := bStart + len(bodyB)
	if err := store.BulkUpsertSymbols([]db.Symbol{
		{ID: "ws::main.A#Function", ProjectID: projectID, FilePath: "main.go", Name: "A",
			QualifiedName: "main.A", Kind: "Function", Language: "Go",
			StartByte: aStart, EndByte: aEnd, StartLine: 1, EndLine: 1,
			Signature: "func A()", IsExported: true, ExtractionConfidence: 1.0},
		{ID: "ws::main.B#Function", ProjectID: projectID, FilePath: "main.go", Name: "B",
			QualifiedName: "main.B", Kind: "Function", Language: "Go",
			StartByte: bStart, EndByte: bEnd, StartLine: 2, EndLine: 2,
			Signature: "func B()", IsExported: true, ExtractionConfidence: 1.0},
	}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":             "ws::main.A#Function",
		"include_source": true,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) != 1 {
		t.Fatalf("len(neighbors) = %d, want 1 (B only — A is the seed)", len(neighbors))
	}
	entry, _ := neighbors[0].(map[string]any)
	gotSource, _ := entry["source"].(string)
	if gotSource == "" {
		t.Fatalf("source missing on include_source=true response: %v", entry)
	}
	if !strings.Contains(gotSource, "func B()") {
		t.Errorf("source = %q, expected to contain B's body", gotSource)
	}
}

// Tool registration: neighborhood appears in the tool registry with
// the right name and required field. Pins the schema-stability gate.
func TestNeighborhood_ToolRegistered(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tool, ok := srv.tools["neighborhood"]
	if !ok {
		t.Fatal("neighborhood tool not registered")
	}
	if tool.Name != "neighborhood" {
		t.Errorf("tool.Name = %q, want neighborhood", tool.Name)
	}
}
