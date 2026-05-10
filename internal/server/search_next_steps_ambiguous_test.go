package server

import (
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #291: when search returns multiple results sharing the top result's
// name (e.g. `New` matches `index.New` and `server.New`), the trace
// recommendation must use `qualified_name` so the agent doesn't blindly
// trace the first match. Bare `name` resolves silently to the first
// hit and leads the agent into the wrong symbol.
func TestSuggestNextStepsForResults_AmbiguousNameUsesQualified(t *testing.T) {
	results := []db.SearchResult{
		{Symbol: db.Symbol{
			ID: "a/a.go::pkg.New#Function", Name: "New",
			QualifiedName: "pkg.New", Kind: "Function", FilePath: "a/a.go",
			ExtractionConfidence: 1.0,
		}},
		{Symbol: db.Symbol{
			ID: "b/b.go::other.New#Function", Name: "New",
			QualifiedName: "other.New", Kind: "Function", FilePath: "b/b.go",
			ExtractionConfidence: 1.0,
		}},
	}
	steps := suggestNextStepsForResults(results)

	var traceStep map[string]string
	for _, s := range steps {
		if s["tool"] == "trace" {
			traceStep = s
			break
		}
	}
	if traceStep == nil {
		t.Fatalf("expected a trace next_step in %v", steps)
	}
	if !strings.Contains(traceStep["args"], "pkg.New") {
		t.Errorf("trace args = %q, want qualified_name pkg.New (bare name New is ambiguous)",
			traceStep["args"])
	}
	if strings.Contains(traceStep["args"], `"name":"New"`) {
		t.Errorf("trace args still uses bare name despite ambiguity: %q", traceStep["args"])
	}
}

// Single-result-name case: keep using bare name. The qualified_name
// switch is only for the ambiguous case.
func TestSuggestNextStepsForResults_UniqueNameUsesBareName(t *testing.T) {
	results := []db.SearchResult{
		{Symbol: db.Symbol{
			ID: "a/a.go::pkg.flushBuffers#Method", Name: "flushBuffers",
			QualifiedName: "pkg.flushBuffers", Kind: "Method", FilePath: "a/a.go",
			ExtractionConfidence: 0.85, // <0.9 so secondary trace step isn't trimmed
		}},
	}
	steps := suggestNextStepsForResults(results)

	var traceStep map[string]string
	for _, s := range steps {
		if s["tool"] == "trace" {
			traceStep = s
			break
		}
	}
	if traceStep == nil {
		t.Fatalf("expected a trace next_step in %v", steps)
	}
	if !strings.Contains(traceStep["args"], `"name":"flushBuffers"`) {
		t.Errorf("trace args = %q, want bare name (unique result)", traceStep["args"])
	}
}

// Even when ambiguous, the why-line should call out the choice so a
// reader of the next_step understands why it diverges from the bare-
// name pattern they see elsewhere.
func TestSuggestNextStepsForResults_AmbiguousWhyLineExplainsChoice(t *testing.T) {
	results := []db.SearchResult{
		{Symbol: db.Symbol{
			ID: "a.go::pkg.X#Function", Name: "X", QualifiedName: "pkg.X",
			Kind: "Function", FilePath: "a.go", ExtractionConfidence: 1,
		}},
		{Symbol: db.Symbol{
			ID: "b.go::other.X#Function", Name: "X", QualifiedName: "other.X",
			Kind: "Function", FilePath: "b.go", ExtractionConfidence: 1,
		}},
	}
	steps := suggestNextStepsForResults(results)
	for _, s := range steps {
		if s["tool"] == "trace" {
			if !strings.Contains(s["why"], "qualified_name") {
				t.Errorf("trace why-line %q should mention qualified_name", s["why"])
			}
			return
		}
	}
	t.Fatal("no trace step in result")
}
