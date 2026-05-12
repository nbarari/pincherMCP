package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #579: extractTextFromHTML ran on every fetched URL regardless of
// Content-Type and consumed `>` unconditionally even outside tags.
// Markdown documents with arrows (`=>`), generics (`Vec<T>`), or
// blockquotes (`> note`) silently lost characters; the title fell
// back to the URL because there's no `<title>` tag in markdown.

func TestHandleFetch_MarkdownContent_PreservesArrowsAndGenericSyntax(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true

	body := `# Project Title

Here's an arrow function: ` + "`const f = () => x + 1`" + ` and a generic ` + "`Vec<T>`" + `.

> Important: the literal ` + "`>`" + ` belongs in the body.

Pincher's HTTP path: ` + "`/v1/<tool>`" + `.
`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte(body))
	}))
	defer upstream.Close()

	result, err := srv.handleFetch(context.Background(), makeReq(fetchArgs(upstream.URL)))
	if err != nil {
		t.Fatalf("handleFetch: %v", err)
	}
	m := decode(t, result)

	// H1 → title (NOT the URL fallback).
	if m["title"] != "Project Title" {
		t.Errorf("title = %v; want %q (H1 of markdown body)", m["title"], "Project Title")
	}

	text, _ := m["text"].(string)
	for _, want := range []string{"=>", "<T>", "<tool>", "> Important"} {
		if !strings.Contains(text, want) {
			t.Errorf("fetched markdown text missing %q — markdown corruption regression (#579)\nfull text:\n%s", want, text)
		}
	}
}

func TestExtractTextFromHTML_PreservesLiteralAngleBracketsOutsideTags(t *testing.T) {
	// Direct unit test of the scanner — pre-fix it consumed `>`
	// even outside any tag.
	in := `<p>Use Vec&lt;T&gt; or write x > 0 inline.</p>`
	_, text := extractTextFromHTML(in)
	if !strings.Contains(text, "x > 0") {
		t.Errorf("scanner ate the `>` outside the tag context: %q", text)
	}
}

func TestFirstMarkdownH1_SkipsFrontMatter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "# Hello\n\nbody", "Hello"},
		{"yaml-front-matter", "---\ntitle: x\n---\n# Real\n\nbody", "Real"},
		{"toml-front-matter", "+++\ntitle = \"x\"\n+++\n# Real\n\nbody", "Real"},
		{"no-h1", "Just text", ""},
		{"h1-after-blanks", "\n\n\n# Late\n", "Late"},
	}
	for _, c := range cases {
		got := firstMarkdownH1(c.in)
		if got != c.want {
			t.Errorf("firstMarkdownH1(%q) = %q; want %q", c.name, got, c.want)
		}
	}
}
