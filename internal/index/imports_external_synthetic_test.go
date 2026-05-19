package index

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/ast"
	"github.com/kwad77/pincher/internal/db"
)

// #1340 v0.71: option (a) — when an IMPORTS edge's to_name can't bind
// to an in-project symbol, resolveImports synthesizes a Module symbol
// at file_path "@external/<sanitized-qn>" and binds the edge to it.
// Pre-fix every `import os`, `require('fs')`, etc. silently dropped.

// Direct resolveImports test: seed a real Function as the FromQN,
// then feed an IMPORTS edge whose ToName is non-resolvable. Asserts
// synthetic external is created and the edge persists. Bypasses the
// extractor so this test pins the resolver-side option (a) plumbing
// regardless of which IMPORTS shapes the per-language extractors emit.
func TestResolveImports_NonResolvableTarget_SynthesizesExternal_1340(t *testing.T) {
	idx, store := newTestIndexer(t)

	projectID := "test-proj"
	if err := store.UpsertProject(db.Project{ID: projectID, Path: "/test", Name: "test", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	fromSym := db.Symbol{
		ID:                   db.MakeSymbolID("app.py", "app", "Module"),
		ProjectID:            projectID,
		FilePath:             "app.py",
		Name:                 "app",
		QualifiedName:        "app",
		Kind:                 "Module",
		Language:             "Python",
		ExtractionConfidence: 1.0,
	}
	if err := store.BulkUpsertSymbols([]db.Symbol{fromSym}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	pending := []ast.ExtractedEdge{
		{FromQN: "app", FromFile: "app.py", ToName: "os", Kind: "IMPORTS", Confidence: 1.0},
	}
	n := idx.resolveImports(projectID, pending, nil, nil, nil)
	if n != 1 {
		t.Fatalf("resolveImports inserted %d edges; want 1 (synthetic binding)", n)
	}

	syms, err := store.GetSymbolsByQN(projectID, "os")
	if err != nil {
		t.Fatalf("GetSymbolsByQN: %v", err)
	}
	var external db.Symbol
	for _, s := range syms {
		if s.Kind == "Module" && strings.HasPrefix(s.FilePath, "@external/") {
			external = s
			break
		}
	}
	if external.ID == "" {
		t.Fatalf("expected synthetic Module at @external/...; got %d candidates: %+v", len(syms), syms)
	}
	if external.Language != "External" {
		t.Errorf("synthetic Language = %q, want External", external.Language)
	}
	if external.QualifiedName != "os" {
		t.Errorf("synthetic QN = %q, want os", external.QualifiedName)
	}

	edges, err := store.EdgesFrom(fromSym.ID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	var found bool
	for _, e := range edges {
		if e.Kind == "IMPORTS" && e.ToID == external.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected IMPORTS edge to synthetic %s; got edges: %+v", external.ID, edges)
	}
}

// Negative control: an in-project IMPORTS that DOES resolve must
// continue to bind to the real symbol, not the synthetic one.
func TestResolveImports_GoIntraProject_BindsToReal_1340(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "go.mod", "module example.com/test\n\ngo 1.22\n")
	writeFile(t, dir, "internal/lib/lib.go", "package lib\n\nfunc Helper() {}\n")
	writeFile(t, dir, "cmd/app/main.go", `package main

import "example.com/test/internal/lib"

func main() { lib.Helper() }
`)

	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// No synthetic @external/example.com_test... should exist for the
	// in-project lib package.
	syms, err := store.GetSymbolsByQN(res.ProjectID, "example.com/test/internal/lib")
	if err != nil {
		t.Fatalf("GetSymbolsByQN: %v", err)
	}
	for _, s := range syms {
		if strings.HasPrefix(s.FilePath, "@external/") {
			t.Errorf("in-project import should bind to the real Module symbol, not a synthetic external one: %+v", s)
		}
	}
}

// Cross-check: sanitizeExternalPath rejects Windows-illegal characters
// so the synthetic file_path doesn't render as control bytes in
// doctor/list/search displays.
func TestSanitizeExternalPath_StripsWindowsIllegalChars_1340(t *testing.T) {
	cases := []struct{ in, want string }{
		{"os", "os"},
		{"./modules/vpc", "./modules/vpc"},
		{"hashicorp/consul/aws", "hashicorp/consul/aws"},
		{"weird<name>", "weird_name_"},
		{`scheme://foo`, "scheme_//foo"}, // : sanitized; / is legal path-sep, preserved
		{"", "unknown"},
	}
	for _, c := range cases {
		if got := sanitizeExternalPath(c.in); got != c.want {
			t.Errorf("sanitizeExternalPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Cross-check: externalModuleSymbol's QN preserves the literal to_name
// (path-shaped, including slashes), so the symbol is addressable by
// the exact string the extractor emitted.
func TestExternalModuleSymbol_QNPreservesToName_1340(t *testing.T) {
	for _, toName := range []string{"os", "./modules/vpc", "hashicorp/consul/aws", "node:child_process"} {
		s := externalModuleSymbol("proj", toName)
		if s.QualifiedName != toName {
			t.Errorf("QN for %q = %q, want %q", toName, s.QualifiedName, toName)
		}
		if s.Kind != "Module" {
			t.Errorf("Kind for %q = %q, want Module", toName, s.Kind)
		}
		if !strings.HasPrefix(s.FilePath, "@external/") {
			t.Errorf("FilePath for %q = %q, want @external/ prefix", toName, s.FilePath)
		}
	}
}
