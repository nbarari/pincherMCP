package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/ast"
)

// #1693 (#1389 cross-language sweep): pincher's qualified name is
// `Class.Method` with no arity/signature component, so every
// method-overload set in an overload-capable language (C#, Java,
// C++, Swift, Kotlin) collides on QN. disambiguateDuplicates
// suffixes the duplicates `~<line>` so all symbols survive, but the
// qualified_name_collision diagnostic still false-fired on every
// overloaded method — noise that erodes trust in `pincher doctor`.
//
// recordExtractionHeuristics now skips a collision that is a
// legitimate overload set: every colliding symbol a Method/Function
// at a distinct, non-overlapping byte range. A true extractor
// duplication (overlapping / identical ranges, or a non-method
// sharing the QN) still flags.

// Positive: a C# class with overloaded methods records no
// qualified_name_collision row.
func TestIndex_CSharpOverloads_NoCollisionDiagnostic_1693(t *testing.T) {
	t.Parallel()
	idx, _ := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "Calc.cs", `namespace M
{
    public class Calc
    {
        public int Add(int a, int b) { return a + b; }
        public int Add(int a, int b, int c) { return a + b + c; }
        public float Add(float a, float b) { return a + b; }
    }
}
`)
	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	var n int
	if err := idx.store.DB().QueryRow(
		`SELECT COUNT(*) FROM extraction_failures
		   WHERE project_id=? AND reason='qualified_name_collision'`,
		res.ProjectID,
	).Scan(&n); err != nil {
		t.Fatalf("count extraction_failures: %v", err)
	}
	if n != 0 {
		t.Errorf("C# class with 3 Add(...) overloads recorded %d qualified_name_collision row(s) — "+
			"method overloading is legitimate C#, the diagnostic must skip it (#1693)", n)
	}
}

// Cross-check: the collisionIsLegitimateOverload discriminator.
func TestCollisionIsLegitimateOverload_Discriminator_1693(t *testing.T) {
	t.Parallel()
	mk := func(syms ...ast.ExtractedSymbol) *ast.FileResult {
		return &ast.FileResult{Symbols: syms}
	}
	method := func(qn string, start, end int) ast.ExtractedSymbol {
		return ast.ExtractedSymbol{QualifiedName: qn, Kind: "Method", StartByte: start, EndByte: end}
	}

	// Distinct non-overlapping ranges, all methods, ~line-suffixed —
	// a legitimate overload set.
	overloads := mk(
		method("M.Calc.Add", 10, 40),
		method("M.Calc.Add~6", 50, 90),
		method("M.Calc.Add~8", 100, 140),
	)
	if !collisionIsLegitimateOverload("M.Calc.Add", overloads) {
		t.Error("3 distinct-range Method symbols not recognized as a legitimate overload set")
	}

	// Overlapping ranges — a true extractor-duplication bug; must
	// NOT be treated as a clean overload set.
	overlapping := mk(
		method("M.Calc.Add", 10, 80),
		method("M.Calc.Add~6", 50, 120),
	)
	if collisionIsLegitimateOverload("M.Calc.Add", overlapping) {
		t.Error("overlapping-range collision wrongly classified as a legitimate overload (true duplication must still flag)")
	}

	// A non-method shares the QN — not a pure overload set.
	mixedKind := mk(
		method("M.Calc.Add", 10, 40),
		ast.ExtractedSymbol{QualifiedName: "M.Calc.Add~6", Kind: "Field", StartByte: 50, EndByte: 60},
	)
	if collisionIsLegitimateOverload("M.Calc.Add", mixedKind) {
		t.Error("a Field sharing the QN must disqualify the overload carve-out")
	}

	// Fewer than 2 symbols at the QN — not a collision at all.
	single := mk(method("M.Calc.Add", 10, 40))
	if collisionIsLegitimateOverload("M.Calc.Add", single) {
		t.Error("a single symbol is not an overload set")
	}
}

func TestAllASCIIDigits_1693(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"4", "127", "0"} {
		if !allASCIIDigits(s) {
			t.Errorf("allASCIIDigits(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "4a", "line6", "-3", " 5"} {
		if allASCIIDigits(s) {
			t.Errorf("allASCIIDigits(%q) = true, want false", s)
		}
	}
}
