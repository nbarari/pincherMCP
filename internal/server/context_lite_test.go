package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #623: context lite=true returns just {id, source}. Used by the
// PreToolUse hook (#625) when redirecting a Read of a large indexed
// file — the agent gets exactly the bytes Read would have returned,
// minimum envelope, no edge walks. These tests pin:
//   - response shape: id + source only, no imports, no callees, no next_steps
//   - default (no lite arg) preserves the full shape
//   - lite still wires through tokens_saved tracking

func newLiteTestSymbol(t *testing.T, srv *Server) (db.Symbol, string) {
	t.Helper()
	dir := t.TempDir()
	src := `package foo

import "fmt"

// Foo is the seed function.
func Foo() {
	fmt.Println("hello")
	helper()
}

func helper() {
	fmt.Println("world")
}
`
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := srv.store.UpsertProject(db.Project{
		ID: "litepr", Path: dir, Name: "litepr",
	}); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	sym := db.Symbol{
		ID:                   "litepr::foo.Foo#Function",
		ProjectID:            "litepr",
		FilePath:             "f.go",
		Name:                 "Foo",
		QualifiedName:        "foo.Foo",
		Kind:                 "Function",
		Language:             "Go",
		Signature:            "func Foo()",
		StartByte:            53,
		EndByte:              108,
		StartLine:            6,
		EndLine:              9,
		ExtractionConfidence: 1.0,
	}
	if err := srv.store.BulkUpsertSymbols([]db.Symbol{sym}); err != nil {
		t.Fatalf("upsert symbol: %v", err)
	}
	return sym, dir
}

func TestHandleContext_Lite_ReturnsIDAndSourceOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	sym, _ := newLiteTestSymbol(t, srv)

	req := makeReq(map[string]any{"id": sym.ID, "lite": true})
	req.Params.Name = "context"
	result, err := srv.handleContext(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("handleContext: err=%v isErr=%v body=%v", err, result.IsError, decode(t, result))
	}
	m := decode(t, result)
	// Expected keys: id, source, _meta. Anything else means the lite
	// short-circuit fell through to the full response builder.
	for _, forbidden := range []string{"symbol", "imports", "callees"} {
		if _, present := m[forbidden]; present {
			t.Errorf("lite=true should not include %q field; got keys %v", forbidden, mapKeys(m))
		}
	}
	if id, _ := m["id"].(string); id != sym.ID {
		t.Errorf("id = %v, want %v", m["id"], sym.ID)
	}
	if src, _ := m["source"].(string); src == "" {
		t.Error("source missing from lite response")
	}
	// next_steps should be absent — pedagogy stripped on success in
	// non-verbose mode (#622).
	meta, _ := m["_meta"].(map[string]any)
	if _, present := meta["next_steps"]; present {
		t.Errorf("next_steps should be absent in lite mode; got %v", meta["next_steps"])
	}
}

func TestHandleContext_DefaultModeStillReturnsFullShape(t *testing.T) {
	// Sanity: dropping `lite` (or setting it to false) preserves the
	// existing context contract — symbol + imports + callees.
	srv, _, _ := newTestServer(t)
	sym, _ := newLiteTestSymbol(t, srv)

	req := makeReq(map[string]any{"id": sym.ID})
	req.Params.Name = "context"
	result, err := srv.handleContext(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("handleContext: err=%v isErr=%v", err, result.IsError)
	}
	m := decode(t, result)
	for _, expected := range []string{"symbol", "imports", "callees"} {
		if _, present := m[expected]; !present {
			t.Errorf("default mode should include %q; got keys %v", expected, mapKeys(m))
		}
	}
}

func TestHandleContext_Lite_StillTracksTokensSaved(t *testing.T) {
	// Lite mode is the small-envelope shape, but the savings tracker
	// still wires through — the agent's stats accumulate from lite calls
	// the same as regular ones. Catches a refactor where lite skips
	// jsonResultWithMeta and silently zeroes savings.
	srv, _, _ := newTestServer(t)
	sym, _ := newLiteTestSymbol(t, srv)

	req := makeReq(map[string]any{"id": sym.ID, "lite": true})
	req.Params.Name = "context"
	result, err := srv.handleContext(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("handleContext: err=%v isErr=%v", err, result.IsError)
	}
	m := decode(t, result)
	meta, _ := m["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing — lite mode broke savings wiring")
	}
	// tokens_saved must be present (may be 0 on a tiny seed file, but
	// the field must exist with a real number, not nil).
	if v, present := meta["tokens_saved"]; !present {
		t.Errorf("tokens_saved missing from lite response; got %v", meta)
	} else if _, isFloat := v.(float64); !isFloat {
		t.Errorf("tokens_saved should be a number; got %T", v)
	}
}

