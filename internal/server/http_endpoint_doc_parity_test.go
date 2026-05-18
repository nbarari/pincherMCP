package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// #672 workstream 4 (v0.79 capability-advertisement audit, HTTP-route
// half). Caught at audit: `/v1/ready` (#660 — k8s-style readiness
// probe, sibling of `/v1/health`) was implemented in server.go and
// declared in the OpenAPI spec, but missing from the canonical HTTP
// REST API table in docs/REFERENCE.md → ### Additional HTTP endpoints.
// Users wiring Kubernetes readiness probes against pincher would not
// know the endpoint exists from REFERENCE.md alone — they'd have to
// read the OpenAPI JSON or the source.
//
// This test pins forward parity: every non-tool HTTP route the server
// answers must appear in REFERENCE.md's HTTP REST API table. Tool
// routes (`/v1/search`, `/v1/symbol`, etc.) are covered separately by
// the OpenAPI golden-file + tool-contract tests; they're documented
// as a class ("23 tool endpoints") rather than per-route.

// nonToolHTTPRoutes is the explicit allowlist of platform routes the
// HTTP server exposes — health/readiness, dashboard, stats, metrics,
// OpenAPI, event-stream, and the dashboard data-feed endpoints.
// Maintained by hand because the routes are wired through varied
// dispatch paths (some via `path == X`, some via `URL.Path == X`,
// dashboard.css/js via a static-asset router). Adding a new platform
// route here without adding a doc row is the drift this test catches.
var nonToolHTTPRoutes = []string{
	"/v1/health",
	"/v1/ready",
	"/v1/dashboard",
	"/v1/dashboard.css",
	"/v1/dashboard.js",
	"/v1/openapi.json",
	"/v1/stats",
	"/v1/sessions",
	"/v1/projects",
	"/v1/index-progress",
	"/v1/events",
	"/v1/hook-stats",
	"/v1/tool-call-stats",
	"/v1/tool-tier-stats",
	"/v1/tool-payload-stats",
	"/v1/metrics",
	"/v1/bench-results",
	"/v1/capabilities",
}

func TestHTTPRoutes_AllNonToolEndpointsDocumented(t *testing.T) {
	t.Parallel()

	refBytes, err := os.ReadFile("../../docs/REFERENCE.md")
	if err != nil {
		t.Fatalf("read REFERENCE.md: %v", err)
	}
	ref := string(refBytes)

	// Slice the doc to the HTTP REST API section to avoid matching
	// path-shaped strings in code samples elsewhere (e.g. tutorials
	// embedded later in REFERENCE.md).
	startIdx := strings.Index(ref, "## HTTP REST API")
	if startIdx < 0 {
		t.Fatal("could not find ## HTTP REST API section in REFERENCE.md")
	}
	rest := ref[startIdx:]
	// End at next H2 heading.
	endRel := regexp.MustCompile(`(?m)^## `).FindAllStringIndex(rest, -1)
	var section string
	if len(endRel) >= 2 {
		// First match is the section's own heading; second is the next H2.
		section = rest[:endRel[1][0]]
	} else {
		section = rest
	}

	// Parse documented endpoints from markdown table rows. The shape
	// is: `| \`/v1/<path>\` | METHOD | Auth | ... |` — pick the path
	// out of the first backticked code-span on the line.
	rowRE := regexp.MustCompile("(?m)^\\|\\s+`(/v1/[^`]+)`")
	documented := make(map[string]bool)
	for _, m := range rowRE.FindAllStringSubmatch(section, -1) {
		documented[m[1]] = true
	}
	if len(documented) == 0 {
		t.Fatalf("no /v1/* endpoint rows parsed from HTTP REST API section — regex / section shape drifted")
	}

	for _, route := range nonToolHTTPRoutes {
		if !documented[route] {
			t.Errorf("HTTP route %q is exposed by the server but no row in docs/REFERENCE.md → ### Additional HTTP endpoints table — add a row or drop the route", route)
		}
	}

	// Reverse: every documented /v1/* row should either be a known
	// non-tool route OR a tool route (handled generically). We don't
	// gate this direction since the doc may name future routes for
	// pre-announcement. But we DO want to know if a row exists that
	// no allowlist entry covers (a doc-only ghost), so log it as a
	// soft signal.
	allowed := make(map[string]bool)
	for _, r := range nonToolHTTPRoutes {
		allowed[r] = true
	}
	for docRoute := range documented {
		if allowed[docRoute] {
			continue
		}
		// Could be a tool route (`/v1/search`) — check against the
		// registered tool names.
		base := strings.TrimPrefix(docRoute, "/v1/")
		base = strings.TrimSuffix(base, "/")
		srv, _, _ := newTestServer(t)
		if _, ok := srv.tools[base]; ok {
			continue
		}
		t.Logf("documented endpoint %q has no allowlist entry and no matching tool — verify it's a real route or stale doc", docRoute)
	}
}

// TestHTTPRoutes_ReadyEndpointActuallyServes is the runtime-probe
// half of the audit: documenting `/v1/ready` is one half; the route
// must actually respond. Pin against silent removal — if someone
// drops the handler, this fails before the doc-parity check does.
func TestHTTPRoutes_ReadyEndpointActuallyServes(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/v1/ready", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK && rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/v1/ready returned %d; expected 200 (ready) or 503 (not ready), not anything else", rr.Code)
	}
}
