package server

import (
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #319: trace's ambiguous-name resolution must prefer the most
// useful seed:
//   1. Production files over scratch / test files
//   2. Callable kinds (Function, Method) over Module/Setting/etc.
//      since the latter have no CALLS edges and yield 0-hop traces.

// The repro: name "main" matches a scratch-file Module first, a
// scratch-file Function second, and a real cmd/pinch Function
// third. Resolution must land on cmd/pinch.
func TestSortTraceCandidates_PrefersProductionFunction(t *testing.T) {
	syms := []db.Symbol{
		{ID: ".scratch_lang_test.go::main#Module", Name: "main",
			FilePath: ".scratch_lang_test.go", Kind: "Module"},
		{ID: ".scratch_lang_test.go::main.main#Function", Name: "main",
			FilePath: ".scratch_lang_test.go", Kind: "Function"},
		{ID: "scratch_lang.go::main#Module", Name: "main",
			FilePath: "scratch_lang.go", Kind: "Module"},
		{ID: "cmd/pinch/main.go::main.main#Function", Name: "main",
			FilePath: "cmd/pinch/main.go", Kind: "Function"},
	}
	sortTraceCandidates(syms)
	if syms[0].ID != "cmd/pinch/main.go::main.main#Function" {
		t.Errorf("top candidate = %q, want cmd/pinch/main.go::main.main#Function (production Function)",
			syms[0].ID)
	}
}

// Module shouldn't outrank Function in the same file.
func TestSortTraceCandidates_FunctionBeatsModuleSameFile(t *testing.T) {
	syms := []db.Symbol{
		{ID: "a.go::main#Module", Name: "main", FilePath: "a.go", Kind: "Module"},
		{ID: "a.go::main.main#Function", Name: "main", FilePath: "a.go", Kind: "Function"},
	}
	sortTraceCandidates(syms)
	if syms[0].Kind != "Function" {
		t.Errorf("top kind = %q, want Function", syms[0].Kind)
	}
}

// Production beats test even when test has the same kind.
func TestSortTraceCandidates_ProductionBeatsTest(t *testing.T) {
	syms := []db.Symbol{
		{ID: "internal/foo/foo_test.go::pkg.Compute#Function", Name: "Compute",
			FilePath: "internal/foo/foo_test.go", Kind: "Function"},
		{ID: "internal/foo/foo.go::pkg.Compute#Function", Name: "Compute",
			FilePath: "internal/foo/foo.go", Kind: "Function"},
	}
	sortTraceCandidates(syms)
	if syms[0].FilePath != "internal/foo/foo.go" {
		t.Errorf("top file = %q, want internal/foo/foo.go (production)", syms[0].FilePath)
	}
}

// #398: testdata/ fixtures must rank below production code AND below
// real test files. The motivating bug: `trace name=Open` resolved to
// `testdata/corpus/go-project/internal/auth/auth.go::auth.Open` instead
// of the real `db.Open`, because the fixture and the real symbol had
// the same kind (Function) and the path filter only knew about
// scratch + test, not testdata fixtures.
func TestSortTraceCandidates_ProductionBeatsFixture(t *testing.T) {
	syms := []db.Symbol{
		{ID: "testdata/corpus/go-project/internal/auth/auth.go::auth.Open#Function",
			Name: "Open", FilePath: "testdata/corpus/go-project/internal/auth/auth.go",
			Kind: "Function"},
		{ID: "internal/db/db.go::db.Open#Function",
			Name: "Open", FilePath: "internal/db/db.go", Kind: "Function"},
	}
	sortTraceCandidates(syms)
	if syms[0].FilePath != "internal/db/db.go" {
		t.Errorf("top file = %q, want internal/db/db.go (production beats fixture)", syms[0].FilePath)
	}
}

// All-test result still works (the test helpers ARE the only
// matches; resolve to one of them, in stable order).
func TestSortTraceCandidates_AllTestStableOrder(t *testing.T) {
	syms := []db.Symbol{
		{ID: "a_test.go::pkg.helper#Function", Name: "helper",
			FilePath: "a_test.go", Kind: "Function"},
		{ID: "b_test.go::pkg.helper#Function", Name: "helper",
			FilePath: "b_test.go", Kind: "Function"},
	}
	sortTraceCandidates(syms)
	if syms[0].FilePath != "a_test.go" {
		t.Errorf("top = %q, want a_test.go (stable original order)", syms[0].FilePath)
	}
}

// Callable kind ladder: Function > Method > Class > everything else.
func TestSortTraceCandidates_KindLadder(t *testing.T) {
	syms := []db.Symbol{
		{ID: "a::Setting", Name: "x", FilePath: "a.yaml", Kind: "Setting"},
		{ID: "a::Class", Name: "x", FilePath: "a.go", Kind: "Class"},
		{ID: "a::Function", Name: "x", FilePath: "a.go", Kind: "Function"},
		{ID: "a::Method", Name: "x", FilePath: "a.go", Kind: "Method"},
	}
	sortTraceCandidates(syms)
	wantOrder := []string{"Function", "Method", "Class", "Setting"}
	for i, want := range wantOrder {
		if syms[i].Kind != want {
			t.Errorf("syms[%d].Kind = %q, want %q (kind ladder)", i, syms[i].Kind, want)
		}
	}
}

// Single-element slice — sortTraceCandidates must not panic.
func TestSortTraceCandidates_SingleElementOk(t *testing.T) {
	syms := []db.Symbol{
		{ID: "x", Name: "x", FilePath: "x.go", Kind: "Function"},
	}
	sortTraceCandidates(syms)
	if syms[0].ID != "x" {
		t.Errorf("single-element sort changed identity: %v", syms[0])
	}
}
