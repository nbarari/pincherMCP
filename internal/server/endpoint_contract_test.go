package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// #528 (umbrella #519): per-endpoint API contract tests for the
// non-MCP-tool /v1/ surface — the ad-hoc dashboard support routes that
// don't go through registerTools and so don't get OpenAPI parity gates
// (#558/#581). Two assertions per endpoint:
//
//  1. Shape: the documented top-level keys are present in the response.
//     Catches accidental key renames and "I changed the field name in
//     the JSON encoder but forgot the dashboard reads the old name"
//     drift.
//
//  2. Negative: write endpoints (POST/DELETE) reject malformed bodies
//     with 4xx, never 500. A 500 leaks an internal stack and confuses
//     proxy retry logic. 4xx is the contract.
//
// The MCP tool endpoints (/v1/search, /v1/architecture, /v1/adr, …)
// are covered by the OpenAPI request+response parity tests; this file
// targets the GET/DELETE/POST routes that DON'T flow through registerTools.

func TestEndpointShape_Health(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/health")
	requireStatus(t, w.Code, 200, "GET /v1/health")
	requireKeys(t, w.Body.Bytes(), "GET /v1/health", "ok", "version", "auth_required")
}

func TestEndpointShape_Stats(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/stats")
	requireStatus(t, w.Code, 200, "GET /v1/stats")
	// session/all_time are present even when their values are nil — the
	// dashboard reads `body.session?.calls` and falls back gracefully on
	// nil, but the keys must exist for the JS optional-chain to be the
	// right shape (vs. throwing on undefined field access).
	requireKeys(t, w.Body.Bytes(), "GET /v1/stats", "session", "all_time")
}

func TestEndpointShape_Sessions(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/sessions")
	requireStatus(t, w.Code, 200, "GET /v1/sessions")
	requireKeys(t, w.Body.Bytes(), "GET /v1/sessions", "sessions")
	// #334-class regression guard: sessions must serialize as [] not null
	// when there are no rows, because the dashboard does `.map(...)` on it.
	body := string(w.Body.Bytes())
	if strings.Contains(body, `"sessions":null`) {
		t.Errorf("GET /v1/sessions: sessions field is null, want [] (regression of #334)\nbody: %s", body)
	}
}

func TestEndpointShape_Projects(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/projects")
	requireStatus(t, w.Code, 200, "GET /v1/projects")
	requireKeys(t, w.Body.Bytes(), "GET /v1/projects", "projects")
}

func TestEndpointShape_OpenAPI(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/openapi.json")
	requireStatus(t, w.Code, 200, "GET /v1/openapi.json")
	// OpenAPI 3.1 minimum structural contract.
	requireKeys(t, w.Body.Bytes(), "GET /v1/openapi.json", "openapi", "info", "paths", "components")
}

func TestEndpointShape_IndexProgress(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpPost(t, srv, "/v1/index-progress", `{"project":"nonexistent"}`)
	requireStatus(t, w.Code, 200, "POST /v1/index-progress")
	requireKeys(t, w.Body.Bytes(), "POST /v1/index-progress",
		"project", "files_done", "files_total", "active")
}

// ── Negative: write endpoints reject malformed bodies with 4xx, not 500 ──

func TestEndpointNegative_DeleteProjects_RejectsEmptyBody(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, body := range []string{"", "{}", `{"id":""}`, "not json", `{"id":null}`} {
		w := httpDelete(t, srv, "/v1/projects", body)
		if w.Code < 400 || w.Code >= 500 {
			t.Errorf("DELETE /v1/projects with body %q: status %d, want 4xx (not 5xx)\nresp: %s",
				body, w.Code, w.Body.String())
		}
		// Error response shape: must be JSON with an "error" key, not a
		// raw string or HTML page.
		requireErrorShape(t, w.Body.Bytes(), "DELETE /v1/projects bad body")
	}
}

func TestEndpointNegative_DeleteProjects_NonexistentID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// A well-formed but nonexistent ID currently returns a 200 with
	// `{"deleted": "<id>"}` because `DeleteProject` is idempotent — no
	// error from a no-op delete. That's the documented contract; pin it
	// here so we don't accidentally start returning 404 (which would
	// break dashboard "delete" buttons that don't preflight existence).
	w := httpDelete(t, srv, "/v1/projects", `{"id":"definitely-not-a-real-project"}`)
	if w.Code != http.StatusOK {
		t.Errorf("DELETE /v1/projects nonexistent id: status %d, want 200 (idempotent delete)\nresp: %s",
			w.Code, w.Body.String())
	}
}

func TestEndpointNegative_DeleteProjectsEmpty_NoBodyRequired(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// /v1/projects/empty takes no body. Sending one anyway must not
	// 500 — it should be ignored.
	for _, body := range []string{"", "{}", `{"junk":true}`} {
		w := httpDelete(t, srv, "/v1/projects/empty", body)
		if w.Code != http.StatusOK {
			t.Errorf("DELETE /v1/projects/empty with body %q: status %d, want 200\nresp: %s",
				body, w.Code, w.Body.String())
		}
		requireKeys(t, w.Body.Bytes(), "DELETE /v1/projects/empty", "deleted")
	}
}

func TestEndpointNegative_IndexProgress_MalformedBody(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// Malformed JSON should not 500 — handler currently ignores decode
	// errors and falls back to the session project. Pin that behavior
	// so a future refactor that adds strict decoding still returns 4xx
	// instead of leaking a stack.
	for _, body := range []string{"not json", "[]", "{"} {
		w := httpPost(t, srv, "/v1/index-progress", body)
		if w.Code >= 500 {
			t.Errorf("POST /v1/index-progress with body %q: status %d (5xx leak)\nresp: %s",
				body, w.Code, w.Body.String())
		}
	}
}

func TestEndpointNegative_MethodNotAllowed_StaysJSON(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// Wrong-method requests on the ad-hoc routes (the ones in the if path == "..." chain)
	// should fall through to the tool-dispatch path which returns 405
	// with a JSON error envelope, not an HTML error page.
	w := httpPost(t, srv, "/v1/health", "{}")
	if w.Code == http.StatusInternalServerError {
		t.Errorf("POST /v1/health: 5xx on wrong method, want 4xx\nresp: %s", w.Body.String())
	}
	if w.Code != 200 {
		// Either the route accepts POST (current: yes, since the health
		// handler doesn't gate on Method) OR returns 4xx with JSON error.
		// Reject HTML error pages either way.
		ct := w.Header().Get("Content-Type")
		if strings.HasPrefix(ct, "text/html") {
			t.Errorf("POST /v1/health: Content-Type %q, want JSON error envelope", ct)
		}
	}
}

// ── helpers ──

// requireStatus is a tiny helper because t.Fatalf with a stable message
// makes test failure scan-readable in CI. Inlined would work too; this
// just keeps the table-tests above tidy.
func requireStatus(t *testing.T, got, want int, label string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status %d, want %d", label, got, want)
	}
}

// requireKeys decodes JSON and asserts every named key is present at
// the top level. Doesn't inspect values — the schema gates handle types
// — just the presence contract that the dashboard's destructuring
// `const {projects} = await r.json()` depends on.
func requireKeys(t *testing.T, body []byte, label string, keys ...string) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("%s: response not JSON object: %v\nbody: %s", label, err, string(body))
	}
	for _, k := range keys {
		if _, ok := resp[k]; !ok {
			t.Errorf("%s: missing top-level key %q\nbody: %s", label, k, string(body))
		}
	}
}

// requireErrorShape: error responses must be `{"error": "..."}` JSON,
// not a bare string or HTML. The dashboard's auth-prompt + toast logic
// reads `body.error` to display the message; a different shape silently
// shows nothing.
func requireErrorShape(t *testing.T, body []byte, label string) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Errorf("%s: error response not JSON: %v\nbody: %s", label, err, string(body))
		return
	}
	if _, ok := resp["error"]; !ok {
		t.Errorf("%s: error response missing \"error\" key\nbody: %s", label, string(body))
	}
}
