package ast

import (
	"strings"
	"testing"
)

// TestHTML_BasicHeadings covers the canonical case: an HTML file with
// h1-h3 produces Section symbols with hierarchical dotted-path QNs,
// matching the Markdown extractor's contract.
func TestHTML_BasicHeadings(t *testing.T) {
	src := []byte(`<!DOCTYPE html>
<html>
<head><title>Install</title></head>
<body>
<h1>Installation</h1>
<p>How to install pincher.</p>
<h2>From Source</h2>
<p>Clone the repo.</p>
<h3>Windows</h3>
<p>Use go build.</p>
<h3>Linux</h3>
<p>Same.</p>
<h2>From Release</h2>
<p>Download the binary.</p>
</body>
</html>
`)
	ext := &htmlExtractor{}
	got := ext.Extract(src, "HTML", "docs/install.html", ExtractOptions{})

	wantQNs := map[string]bool{
		"title":                                    false,
		"installation":                             false,
		"installation.from_source":                 false,
		"installation.from_source.windows":         false,
		"installation.from_source.linux":           false,
		"installation.from_release":                false,
	}
	for _, sym := range got.Symbols {
		if sym.Kind != "Section" {
			t.Errorf("kind=%q, want Section for %q", sym.Kind, sym.QualifiedName)
		}
		if _, ok := wantQNs[sym.QualifiedName]; ok {
			wantQNs[sym.QualifiedName] = true
		}
	}
	for qn, found := range wantQNs {
		if !found {
			t.Errorf("expected QN %q not produced; got: %v", qn, qnList(got.Symbols))
		}
	}
}

func qnList(syms []ExtractedSymbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.QualifiedName)
	}
	return out
}

// TestHTML_TitleStandalone covers an SPA-style page with no headings —
// only a <title>. The title should produce one Section symbol so the
// page is still searchable.
func TestHTML_TitleStandalone(t *testing.T) {
	src := []byte(`<!DOCTYPE html>
<html>
<head><title>Pincher Dashboard</title></head>
<body><div id="root"></div></body>
</html>
`)
	got := (&htmlExtractor{}).Extract(src, "HTML", "index.html", ExtractOptions{})
	if len(got.Symbols) == 0 {
		t.Fatal("expected at least the title Section symbol")
	}
	titleFound := false
	for _, sym := range got.Symbols {
		if sym.QualifiedName == "title" {
			if sym.Name != "Pincher Dashboard" {
				t.Errorf("title name=%q, want %q", sym.Name, "Pincher Dashboard")
			}
			titleFound = true
		}
	}
	if !titleFound {
		t.Errorf("expected QN=title, got: %v", qnList(got.Symbols))
	}
}

// TestHTML_ImportsEdges covers script/link/local-anchor → IMPORTS edges,
// and confirms external URLs / anchor-only / mailto / javascript: are
// skipped.
func TestHTML_ImportsEdges(t *testing.T) {
	src := []byte(`<!DOCTYPE html>
<html>
<head>
  <link rel="stylesheet" href="styles/main.css">
  <link rel="icon" href="favicon.ico">
  <link rel="stylesheet" href="https://cdn.example.com/external.css">
  <script src="js/app.js"></script>
  <script src="//cdn.example.com/lib.js"></script>
</head>
<body>
  <a href="page2.html">Next page</a>
  <a href="#section-1">Anchor</a>
  <a href="mailto:foo@bar.com">Email</a>
  <a href="https://example.com/external">External</a>
  <a href="docs/intro.html?v=1#start">Intro</a>
</body>
</html>
`)
	got := (&htmlExtractor{}).Extract(src, "HTML", "site/index.html", ExtractOptions{})

	wantImports := map[string]bool{
		"styles/main.css":  false,
		"favicon.ico":      false,
		"js/app.js":        false,
		"page2.html":       false,
		"docs/intro.html":  false, // query + fragment stripped
	}
	for _, edge := range got.Edges {
		if edge.Kind != "IMPORTS" {
			t.Errorf("edge kind=%q, want IMPORTS", edge.Kind)
		}
		if _, ok := wantImports[edge.ToName]; ok {
			wantImports[edge.ToName] = true
		}
	}
	for ref, found := range wantImports {
		if !found {
			t.Errorf("expected import %q not produced; got edges: %v", ref, edgeList(got.Edges))
		}
	}

	// External / anchor / mailto / javascript should NOT appear.
	for _, edge := range got.Edges {
		switch edge.ToName {
		case "https://cdn.example.com/external.css",
			"//cdn.example.com/lib.js",
			"#section-1",
			"mailto:foo@bar.com",
			"https://example.com/external":
			t.Errorf("expected %q to be skipped, but it was emitted", edge.ToName)
		}
	}
}

func edgeList(edges []ExtractedEdge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.Kind+":"+e.ToName)
	}
	return out
}

// TestHTML_DedupImports pins the file-path dedup invariant: a page
// linking to the same .css file three times should produce one
// IMPORTS edge, not three.
func TestHTML_DedupImports(t *testing.T) {
	src := []byte(`<!DOCTYPE html>
<html><head>
  <link rel="stylesheet" href="main.css">
  <link rel="stylesheet" href="main.css">
  <link rel="alternate" href="main.css">
</head><body></body></html>
`)
	got := (&htmlExtractor{}).Extract(src, "HTML", "x.html", ExtractOptions{})
	count := 0
	for _, edge := range got.Edges {
		if edge.ToName == "main.css" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 dedup'd IMPORTS edge for main.css, got %d", count)
	}
}

// TestHTML_MalformedTolerates pins the permissiveness invariant:
// missing closing tags, mismatched nesting, etc. should produce a
// partial result rather than a panic or empty output.
func TestHTML_MalformedTolerates(t *testing.T) {
	src := []byte(`<html><body>
<h1>Top
<h2>Nested with no closing h1
<p>Body
<h1>Another top
</body></html>
`)
	got := (&htmlExtractor{}).Extract(src, "HTML", "x.html", ExtractOptions{})
	if len(got.Symbols) == 0 {
		t.Errorf("expected symbols from malformed input (parser is permissive); got none")
	}
}

// TestHTML_EmptyInput: nothing should crash, empty result.
func TestHTML_EmptyInput(t *testing.T) {
	got := (&htmlExtractor{}).Extract([]byte(""), "HTML", "x.html", ExtractOptions{})
	if got == nil {
		t.Fatal("Extract returned nil result")
	}
	if len(got.Symbols) != 0 {
		t.Errorf("expected no symbols from empty input, got %d", len(got.Symbols))
	}
}

// TestHTML_ConfidenceIs100 pins the parser-backed confidence. HTML
// joins Markdown / Go / YAML / HCL / Bash / TOML / Jinja2 in the 1.0
// tier.
func TestHTML_ConfidenceIs100(t *testing.T) {
	if c := (&htmlExtractor{}).Confidence(); c != 1.0 {
		t.Errorf("HTML extractor confidence = %v, want 1.0", c)
	}
}

// TestHTML_RegisteredForExtensions: registry must dispatch .html and
// .htm to the HTML extractor.
func TestHTML_RegisteredForExtensions(t *testing.T) {
	for _, ext := range []string{".html", ".htm"} {
		lang := DetectLanguage("/some/path/x" + ext)
		if lang != "HTML" {
			t.Errorf("DetectLanguage(%q) = %q, want HTML", ext, lang)
		}
	}
}

// TestHTML_HierarchyAcrossLevelGaps covers the case where a heading
// jumps levels (h1 → h3 with no h2). Stack semantics should still
// produce a correct hierarchy with the h3 nested under the h1.
// Duplicate (level, title) headings — common in long pages with
// repeating sub-sections like "Phase 2: Configuration" under multiple
// h1 parents — used to all resolve to the same first-occurrence
// byte offset. Pre-fix, the section-emit loop computed end_byte=
// start_byte=N for every section after the first dup, and
// recordExtractionHeuristics caught these as byte_range_negative
// extraction failures on real HTML docs. bytesFindHeadingAfter
// advances the search bound past each previously-located heading.
func TestHTML_DuplicateHeadingsHaveDistinctByteRanges(t *testing.T) {
	src := []byte(`<!DOCTYPE html>
<html><body>
<h1>Honcho</h1>
<h2>Phase 1: Spec</h2>
<p>Body of phase 1.</p>
<h2>Phase 2: Configuration</h2>
<p>Body of phase 2 first occurrence.</p>
<h1>Hermes</h1>
<h2>Phase 1: Spec</h2>
<p>Body of phase 1 (hermes).</p>
<h2>Phase 2: Configuration</h2>
<p>Body of phase 2 second occurrence.</p>
</body></html>
`)
	got := (&htmlExtractor{}).Extract(src, "HTML", "docs/dup.html", ExtractOptions{})

	for _, sym := range got.Symbols {
		if sym.Kind != "Section" {
			continue
		}
		if sym.EndByte <= sym.StartByte {
			t.Errorf("Section %q has empty byte range start=%d end=%d — duplicate-heading byte-find regression",
				sym.QualifiedName, sym.StartByte, sym.EndByte)
		}
	}

	// Each duplicate heading should produce a distinct Section. The QN
	// hierarchy gives us the disambiguator since the two "Phase 2:
	// Configuration" sections live under different h1 parents.
	seen := map[int]bool{}
	for _, sym := range got.Symbols {
		if sym.Kind != "Section" {
			continue
		}
		if seen[sym.StartByte] {
			t.Errorf("two Sections share the same StartByte=%d (%q) — duplicate-heading byte-find regression",
				sym.StartByte, sym.QualifiedName)
		}
		seen[sym.StartByte] = true
	}
}

func TestHTML_HierarchyAcrossLevelGaps(t *testing.T) {
	src := []byte(`<!DOCTYPE html>
<html><body>
<h1>Top</h1>
<h3>Sub-sub</h3>
<h1>Top2</h1>
</body></html>
`)
	got := (&htmlExtractor{}).Extract(src, "HTML", "x.html", ExtractOptions{})

	qns := []string{}
	for _, sym := range got.Symbols {
		qns = append(qns, sym.QualifiedName)
	}
	joined := strings.Join(qns, ",")
	if !strings.Contains(joined, "top.sub_sub") {
		t.Errorf("expected `top.sub_sub` (h3 nested under h1 across the gap); got: %s", joined)
	}
	if !strings.Contains(joined, "top2") {
		t.Errorf("expected `top2` as a sibling top-level; got: %s", joined)
	}
}
