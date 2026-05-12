package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #617: when the fetched response is large but extracted text is
// near-empty (typical of JS-rendered SPAs like GitHub/Twitter), the
// response previously looked successful — `stored: true`, realistic
// `raw_bytes`, real `title`, but `text` was just the inert
// accessibility skip-link. Agents acted on the empty text as if
// it were the page content. Fix surfaces a `_meta.warnings` entry.

// jsShellHTML simulates a JS-rendered SPA: ~30 KB of script tags +
// inline JSON + minified CSS, ~10 chars of visible text. The body
// contains "Skip to content" — the GitHub-style accessibility hop.
func jsShellHTML(t *testing.T, padBytes int) string {
	t.Helper()
	pad := strings.Repeat("a", padBytes)
	return `<!DOCTYPE html><html><head><title>SPA</title>` +
		`<script>window.__INITIAL_STATE__=` + `"` + pad + `"</script>` +
		`<style>` + pad + `</style></head>` +
		`<body><a class="skip-link" href="#content">Skip to content</a>` +
		`<div id="root"></div></body></html>`
}

func TestHandleFetch_EmitsWarning_WhenJSRenderedShell(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true

	body := jsShellHTML(t, 30000) // ~60 KB raw, ~15 visible chars
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(body))
	}))
	defer upstream.Close()

	result, err := srv.handleFetch(context.Background(), makeReq(fetchArgs(upstream.URL)))
	if err != nil {
		t.Fatalf("handleFetch: %v", err)
	}
	m := decode(t, result)
	if m["stored"] != true {
		t.Fatalf("expected stored=true (fetch should still succeed); got %v", m["stored"])
	}
	meta, ok := m["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("expected _meta map; got %T", m["_meta"])
	}
	warnings, ok := meta["warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected at least one _meta.warnings entry on JS-shell response; got %v", meta["warnings"])
	}
	first, _ := warnings[0].(string)
	if !strings.Contains(first, "JS-rendered") && !strings.Contains(first, "extracted only") {
		t.Errorf("warning should explain the JS-render heuristic; got %q", first)
	}
}

func TestHandleFetch_NoWarning_OnNormalHTMLPage(t *testing.T) {
	// Sanity: a real text-heavy HTML page must NOT trigger the warning.
	// Pre-#617 fix shouldn't have over-broadened to normal pages.
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true

	body := `<!DOCTYPE html><html><head><title>Article</title></head>
<body><h1>Article Title</h1>
<p>This is a moderately sized article with paragraphs of real content. ` +
		strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ", 200) +
		`</p></body></html>`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(body))
	}))
	defer upstream.Close()

	result, err := srv.handleFetch(context.Background(), makeReq(fetchArgs(upstream.URL)))
	if err != nil {
		t.Fatalf("handleFetch: %v", err)
	}
	m := decode(t, result)
	meta, _ := m["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, ok := w.(string); ok && (strings.Contains(s, "JS-rendered") || strings.Contains(s, "extracted only")) {
			t.Errorf("normal HTML page should NOT trigger JS-render warning; got %q", s)
		}
	}
}

func TestHandleFetch_NoWarning_OnSmallPage(t *testing.T) {
	// The heuristic is gated on raw size > 10 KB so trivial pages
	// (welcome screens, healthchecks) don't get false-positive warnings.
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true

	body := `<html><head><title>Tiny</title></head><body>ok</body></html>`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(body))
	}))
	defer upstream.Close()

	result, err := srv.handleFetch(context.Background(), makeReq(fetchArgs(upstream.URL)))
	if err != nil {
		t.Fatalf("handleFetch: %v", err)
	}
	m := decode(t, result)
	meta, _ := m["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, "JS-rendered") {
			t.Errorf("tiny page (raw < 10KB) should not trip the heuristic; got %q", s)
		}
	}
}

func TestHandleFetch_NoWarning_OnMarkdownInput(t *testing.T) {
	// Markdown extraction is verbatim copy — text/raw_bytes ratio is
	// always ~1, no risk of the JS-render heuristic firing. Belt-and-
	// suspenders: the heuristic explicitly skips text/markdown and
	// text/plain content types.
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true

	body := strings.Repeat("# Heading\n\nParagraph content here.\n\n", 1000) // ~30 KB markdown
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
	meta, _ := m["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, "JS-rendered") {
			t.Errorf("markdown input should not trip heuristic; got %q", s)
		}
	}
}
