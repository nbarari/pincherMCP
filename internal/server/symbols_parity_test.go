package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #336: handleSymbols must return the same field set as handleSymbol so
// a one-ID batch returns the same shape as a single-symbol call.
// Without parity, callers had to know which tool to use to access
// fields like qualified_name / extraction_confidence.

func TestHandleSymbols_Parity_SameFieldSetAsHandleSymbol(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := t.TempDir()
	store.UpsertProject(db.Project{ID: pid, Path: pid, Name: "parity", IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = pid

	// Write a real source file so byte-offset reads work.
	src := "package main\n\nfunc Foo() string { return \"hi\" }\n"
	if err := os.WriteFile(filepath.Join(pid, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	mustUpsertSymbols(t, store, []db.Symbol{
		{
			ID:                   "p::main.Foo#Function",
			ProjectID:            pid,
			FilePath:             "main.go",
			Name:                 "Foo",
			QualifiedName:        "main.Foo",
			Kind:                 "Function",
			Language:             "Go",
			StartByte:            14,
			EndByte:              50,
			StartLine:            3,
			EndLine:              3,
			Signature:            "func Foo() string",
			ReturnType:           "string",
			Docstring:            "",
			Complexity:           1,
			IsExported:           true,
			ExtractionConfidence: 1.0,
		},
	})

	// Fields that handleSymbol returns. handleSymbols must return all
	// of these (source is included as the actual byte-offset body).
	requiredFields := []string{
		"id", "name", "qualified_name", "kind", "language",
		"file_path", "start_line", "end_line", "start_byte", "end_byte",
		"signature", "return_type", "docstring", "complexity",
		"is_exported", "extraction_confidence", "source",
	}

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids":     []any{"p::main.Foo#Function"},
		"project": pid,
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	body := decode(t, result)
	syms, _ := body["symbols"].([]any)
	if len(syms) != 1 {
		t.Fatalf("symbols length = %d, want 1: %v", len(syms), body)
	}
	entry, _ := syms[0].(map[string]any)

	for _, f := range requiredFields {
		if _, present := entry[f]; !present {
			t.Errorf("symbols entry missing field %q (single-symbol parity gap):\n%v", f, entry)
		}
	}

	// Sanity: the populated fields must match the seeded values, not be
	// zero-value placeholders.
	if got, _ := entry["qualified_name"].(string); got != "main.Foo" {
		t.Errorf("qualified_name = %q, want main.Foo", got)
	}
	if got, _ := entry["extraction_confidence"].(float64); got != 1.0 {
		t.Errorf("extraction_confidence = %v, want 1.0", got)
	}
}

// #317 staleness warning must fire per-entry when the file on disk has
// changed since indexing. Without the per-entry hook, batch consumers
// silently consume stale source bytes.
func TestHandleSymbols_StalenessWarning_PerEntry(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := t.TempDir()
	store.UpsertProject(db.Project{ID: pid, Path: pid, Name: "stale", IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = pid

	srcOriginal := "package main\nfunc Foo(){}\n"
	if err := os.WriteFile(filepath.Join(pid, "main.go"), []byte(srcOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	// Record a hash that corresponds to a DIFFERENT content — simulates
	// the file having been modified since the indexer captured it.
	if err := store.SetFileHash(pid, "main.go", "deadbeefdeadbeef"); err != nil {
		t.Fatal(err)
	}
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p::main.Foo#Function", ProjectID: pid, FilePath: "main.go", Name: "Foo",
			QualifiedName: "main.Foo", Kind: "Function", Language: "Go",
			StartByte: 13, EndByte: 25, StartLine: 2, EndLine: 2, ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids":     []any{"p::main.Foo#Function"},
		"project": pid,
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	body := decode(t, result)
	syms, _ := body["symbols"].([]any)
	if len(syms) != 1 {
		t.Fatalf("symbols length = %d, want 1", len(syms))
	}
	entry, _ := syms[0].(map[string]any)
	meta, ok := entry["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("entry missing _meta when file was modified since index:\n%v", entry)
	}
	warnings, _ := meta["warnings"].([]any)
	if len(warnings) == 0 {
		t.Errorf("expected staleness warning in entry._meta.warnings; got %v", meta)
	}
}
