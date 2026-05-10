package server

import (
	"strings"
	"testing"
)

// #292: when `changes` finds only Section/Heading/Document changes,
// next_steps must NOT recommend a search call with the section title
// as the query — that title has em-dashes / colons / dot-prefixed
// numbers and either errors or BM25-matches unrelated noise. Emit a
// short note instead.
func TestSuggestChangesNextSteps_DocOnlyDiffSkipsSearch(t *testing.T) {
	changedSyms := []map[string]any{
		{"id": "doc.md::a#Section", "name": "Foo — bar", "kind": "Section"},
		{"id": "doc.md::a.b#Section", "name": "Baz", "kind": "Section"},
	}
	steps := suggestChangesNextSteps(nil, changedSyms, map[string]int{})
	if len(steps) != 1 {
		t.Fatalf("expected exactly 1 step (the doc-only note), got %d: %v", len(steps), steps)
	}
	step := steps[0]
	if step["tool"] != "" {
		t.Errorf("doc-only step should have empty tool (note-only), got %q", step["tool"])
	}
	if !strings.Contains(step["note"], "documentation-only") {
		t.Errorf("note should mention documentation-only, got %q", step["note"])
	}
	for _, s := range steps {
		if s["tool"] == "search" {
			t.Errorf("doc-only diff produced a search next_step: %v", s)
		}
	}
}

// Mixed code + doc — code symbol is preferred for the search query.
func TestSuggestChangesNextSteps_MixedDiffPrefersCodeName(t *testing.T) {
	changedSyms := []map[string]any{
		{"id": "doc.md::a#Section", "name": "Foo — bar", "kind": "Section"},
		{"id": "a.go::pkg.Compute#Function", "name": "Compute", "kind": "Function"},
	}
	steps := suggestChangesNextSteps(nil, changedSyms, map[string]int{})
	if len(steps) == 0 {
		t.Fatal("expected at least 1 step for mixed diff")
	}
	for _, s := range steps {
		if s["tool"] == "search" {
			if !strings.Contains(s["args"], "Compute") {
				t.Errorf("mixed-diff search should query the code name 'Compute', got %q", s["args"])
			}
			if strings.Contains(s["args"], "Foo") {
				t.Errorf("mixed-diff search must not use the Section title: %q", s["args"])
			}
		}
	}
}

// Code-only diff — keep the existing behaviour.
func TestSuggestChangesNextSteps_CodeOnlyDiffStillSuggestsSearch(t *testing.T) {
	changedSyms := []map[string]any{
		{"id": "a.go::pkg.Compute#Function", "name": "Compute", "kind": "Function"},
	}
	steps := suggestChangesNextSteps(nil, changedSyms, map[string]int{})
	if len(steps) != 1 || steps[0]["tool"] != "search" {
		t.Errorf("code-only diff should produce a search step, got %v", steps)
	}
}

// allDocKinds helper coverage.
func TestAllDocKinds(t *testing.T) {
	cases := []struct {
		in   []map[string]any
		want bool
	}{
		{[]map[string]any{{"kind": "Section"}}, true},
		{[]map[string]any{{"kind": "Section"}, {"kind": "Heading"}}, true},
		{[]map[string]any{{"kind": "Document"}}, true},
		{[]map[string]any{{"kind": "Section"}, {"kind": "Function"}}, false},
		{[]map[string]any{{"kind": "Function"}}, false},
		{nil, false},
		{[]map[string]any{}, false},
	}
	for i, c := range cases {
		if got := allDocKinds(c.in); got != c.want {
			t.Errorf("case %d: allDocKinds(%v) = %v, want %v", i, c.in, got, c.want)
		}
	}
}
