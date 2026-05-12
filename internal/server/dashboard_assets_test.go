package server

import (
	"bytes"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #522 + #523 (umbrella #519): regression tests for the dashboard's
// non-HTML assets — /v1/dashboard.css and /v1/dashboard.js. Pre-tests
// these endpoints had no coverage at all: an accidental empty body, a
// Cache-Control flip, or a basepath-substitution regression would slip
// to production undetected.
//
// CSS strategy: full byte snapshot under testdata/dashboard/dashboard.css.
// Stylesheets don't move often; a snapshot is the cheapest possible
// insurance. To regenerate after intentional CSS edits:
//
//	go test ./internal/server -run TestDashboardCSS -update-dashboard-css-snapshot

var updateDashboardCSSSnapshot = flag.Bool("update-dashboard-css-snapshot", false,
	"overwrite testdata/dashboard/dashboard.css with the current renderer output")

func TestDashboardCSS_RegressionSnapshot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/dashboard.css", nil)
	srv.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("dashboard.css: status %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css*", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc == "" {
		t.Errorf("Cache-Control header missing — caches will treat the asset as no-store")
	}

	body := w.Body.Bytes()
	// Body length bound: catches accidental empty body AND accidental
	// inflation past a sane upper bound. Tuned generously around the
	// current ~7 KB stylesheet.
	if len(body) < 1000 {
		t.Errorf("dashboard.css body suspiciously short: %d bytes", len(body))
	}
	if len(body) > 100_000 {
		t.Errorf("dashboard.css body suspiciously long: %d bytes — did inline content slip in?", len(body))
	}

	fixture := "testdata/dashboard/dashboard.css"
	if *updateDashboardCSSSnapshot {
		if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(fixture, body, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Logf("updated %s (%d bytes)", fixture, len(body))
		return
	}
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture %s: %v\nRun with -update-dashboard-css-snapshot to generate.", fixture, err)
	}
	if !bytes.Equal(normalizeNewlines(body), normalizeNewlines(want)) {
		t.Errorf("dashboard.css drifted from %s.\n"+
			"If intentional, regenerate:\n"+
			"  go test ./internal/server -run TestDashboardCSS -update-dashboard-css-snapshot",
			fixture)
	}
}

// #523: the /v1/dashboard.js endpoint substitutes the reverse-proxy
// basepath into a `const BP = "..."` declaration. The substitution is
// load-bearing — every fetch in the dashboard goes through a wrapper
// that prepends BP to /v1/* URLs. A bad basepath (unescaped quote,
// trailing slash double-rewriting paths, drift between the HTML's
// link/script tags and the JS BP) silently breaks every data fetch.
//
// Table-driven across the cases the issue calls out: empty, trailing
// slash, URL-special chars, BP that itself contains /v1/, deep paths.
func TestDashboardJS_BasepathSubstitution(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		// wantBP is the literal string that should appear inside the
		// emitted `const BP = "..."` declaration after substitution.
		// Capturing the post-substitution value (rather than the input)
		// documents the normalization contract: a trailing slash on the
		// input must NOT survive into BP, otherwise the wrapper rewrite
		// `BP + '/v1/...'` produces `/foo//v1/...` (double slash) which
		// reverse proxies route as a different path.
		wantBP string
	}{
		{name: "empty", prefix: "", wantBP: ""},
		{name: "simple", prefix: "/pincher", wantBP: "/pincher"},
		{name: "trailing-slash-normalized", prefix: "/pincher/", wantBP: "/pincher"},
		{name: "deep-path", prefix: "/foo/bar/baz", wantBP: "/foo/bar/baz"},
		{name: "deep-path-trailing-slash", prefix: "/foo/bar/baz/", wantBP: "/foo/bar/baz"},
		// BP that itself contains /v1/ — the wrapper's `if input.startsWith('/v1/')`
		// check fires on the ORIGINAL input only, so a BP containing /v1/
		// shouldn't get re-rewritten on the second pass. We just verify
		// the substitution preserves it; the no-double-rewrite contract
		// is exercised in the wrapper-logic tests elsewhere.
		{name: "bp-contains-v1", prefix: "/proxy/v1/passthrough", wantBP: "/proxy/v1/passthrough"},
		// URL-encoded chars in the prefix should pass through verbatim;
		// the proxy that set the prefix already encoded them.
		{name: "url-encoded-chars", prefix: "/team%20one", wantBP: "/team%20one"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			js := renderDashboardJS(tc.prefix)

			// Find the BP declaration. It's a single line: `const BP = "...";`
			needle := `const BP = "` + tc.wantBP + `";`
			if !strings.Contains(js, needle) {
				t.Errorf("renderDashboardJS(%q): missing %q in output\nfirst 200 chars after BP:\n%s",
					tc.prefix, needle, snippetAroundBP(js))
			}

			// Hard guard: no unresolved __PINCHER_BASEPATH__ tokens left.
			if strings.Contains(js, "__PINCHER_BASEPATH__") {
				t.Errorf("renderDashboardJS(%q): unresolved __PINCHER_BASEPATH__ token in output", tc.prefix)
			}
		})
	}
}

// snippetAroundBP returns the line containing `const BP` for diagnostics.
func snippetAroundBP(js string) string {
	for _, line := range strings.Split(js, "\n") {
		if strings.Contains(line, "const BP") {
			return line
		}
	}
	return "(no `const BP` line found)"
}

// TestDashboardJS_BasepathSubstitution_HTMLAndJSAgree pins the contract
// that the HTML's <link>/<script> tags use the SAME prefix substitution
// as the JS body's BP constant. Drift between the two is the exact
// failure mode that motivated splitting renderDashboard from
// renderDashboardJS — if the HTML normalizes a trailing slash but the
// JS doesn't, the browser fetches dashboard.css from /pincher/v1/...
// while the JS wrapper rewrites to /pincher//v1/...
func TestDashboardJS_BasepathSubstitution_HTMLAndJSAgree(t *testing.T) {
	for _, prefix := range []string{"", "/pincher", "/pincher/", "/a/b/c"} {
		html := renderDashboard(prefix)
		js := renderDashboardJS(prefix)

		// Extract whatever appears in the script-src tag: `<script src="<X>/v1/dashboard.js"`
		const marker = `<script src="`
		i := strings.Index(html, marker)
		if i < 0 {
			t.Fatalf("renderDashboard(%q): no <script src=...> found", prefix)
		}
		j := strings.Index(html[i+len(marker):], `/v1/dashboard.js"`)
		if j < 0 {
			t.Fatalf("renderDashboard(%q): no /v1/dashboard.js suffix on script src", prefix)
		}
		htmlPrefix := html[i+len(marker) : i+len(marker)+j]

		// Extract BP value from the JS.
		const bpMarker = `const BP = "`
		k := strings.Index(js, bpMarker)
		if k < 0 {
			t.Fatalf("renderDashboardJS(%q): no `const BP` declaration", prefix)
		}
		end := strings.Index(js[k+len(bpMarker):], `"`)
		if end < 0 {
			t.Fatalf("renderDashboardJS(%q): unterminated BP string", prefix)
		}
		jsBP := js[k+len(bpMarker) : k+len(bpMarker)+end]

		if htmlPrefix != jsBP {
			t.Errorf("prefix %q: HTML <script src> uses %q but JS BP is %q — divergent rewriting will produce mismatched fetch URLs",
				prefix, htmlPrefix, jsBP)
		}
	}
}
