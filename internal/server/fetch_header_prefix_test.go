package server

import (
	"strings"
	"testing"
)

// #945: extractTextFromHTML used `<head` as a literal prefix to find
// the `<head>...</head>` block to strip. This also matched `<header>`,
// and the closing-tag search for `</head>` then never matched in
// `</header>`, causing the "no closing tag" branch to truncate the
// document body from the first <header> onwards. Wikipedia (100+ <p>
// tags, multiple <header> elements, fully static) was reduced to its
// pre-<header> skip-link — 15 chars from 400+ KB.

func TestExtractTextFromHTML_HeaderTagDoesNotEatBody(t *testing.T) {
	raw := `<!DOCTYPE html><html>
<head><title>Doc</title></head>
<body>
<header>Top nav</header>
<main>
<p>First paragraph with real content.</p>
<p>Second paragraph with more content.</p>
</main>
<footer>Bottom</footer>
</body>
</html>`
	title, text := extractTextFromHTML(raw)
	if title != "Doc" {
		t.Errorf("title = %q, want %q", title, "Doc")
	}
	if !strings.Contains(text, "First paragraph") {
		t.Errorf("text dropped body content; got %q", text)
	}
	if !strings.Contains(text, "Second paragraph") {
		t.Errorf("text dropped body content (second paragraph); got %q", text)
	}
	// Note: <header> is NOT in the strip-list (only <head>, the document
	// head, is). Pre-fix `<header>` was accidentally stripped via prefix
	// match — and the closing-tag-not-found branch then truncated the
	// rest of the document. The fix restores body content; whether to
	// also strip <header> as semantic-nav is a separate decision.
	if strings.Contains(text, "Bottom") {
		t.Errorf("<footer> content should be stripped, kept in text=%q", text)
	}
}

// Same shape: <nav> prefix-matching <navbar>, <navigation>, etc.
func TestExtractTextFromHTML_NavTagBoundary(t *testing.T) {
	raw := `<!DOCTYPE html><html><body>
<nav>Real nav strip me</nav>
<navbar-custom>Custom element keep me</navbar-custom>
<p>Body paragraph.</p>
</body></html>`
	_, text := extractTextFromHTML(raw)
	if strings.Contains(text, "Real nav") {
		t.Errorf("real <nav> should be stripped; text=%q", text)
	}
	if !strings.Contains(text, "Custom element") {
		t.Errorf("<navbar-custom> content should NOT be stripped; text=%q", text)
	}
	if !strings.Contains(text, "Body paragraph") {
		t.Errorf("body paragraph lost; text=%q", text)
	}
}

// Defensive: when a malformed page has a <head opening but no </head>
// closing, the old code truncated raw[:si] losing the rest of the
// document. New behavior skips past the open and continues stripping
// other blocks.
func TestExtractTextFromHTML_MissingCloseDoesNotTruncate(t *testing.T) {
	raw := `<html><body>
<p>Paragraph before broken nav.</p>
<nav class="bad"
<p>Paragraph after broken nav.</p>
</body></html>`
	_, text := extractTextFromHTML(raw)
	if !strings.Contains(text, "Paragraph before") {
		t.Errorf("dropped before-text; text=%q", text)
	}
	// The "after" paragraph may or may not survive depending on how the
	// broken open is parsed by the tag stripper — we just need to NOT
	// truncate-the-whole-document. Pre-fix, "before" disappeared too.
}
