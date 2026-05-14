package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #609: POST/PUT/DELETE on a known GET-only path must return 405 with
// `Allow: GET, HEAD`, not the misleading "unknown tool" 404. HEAD on a
// GET path must return identical headers with no body, per RFC 7231.

func httpDo(t *testing.T, srv *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// readBody normalises the response body, transparently un-gzipping when
// the response is gzip-encoded. The dispatcher gzips eagerly when the
// request advertises Accept-Encoding: gzip; httptest.NewRequest doesn't
// set that header by default so this is a no-op for these tests, but
// keep the helper defensive in case the default ever changes.
func readBody(t *testing.T, rr *httptest.ResponseRecorder) []byte {
	t.Helper()
	if rr.Header().Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		defer gr.Close()
		out, err := io.ReadAll(gr)
		if err != nil {
			t.Fatalf("gzip read: %v", err)
		}
		return out
	}
	return rr.Body.Bytes()
}

func TestHTTP_PostOnGetOnlyDashboard_Returns405(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	rr := httpDo(t, srv, http.MethodPost, "/v1/dashboard")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/dashboard status = %d, want 405; body=%s", rr.Code, readBody(t, rr))
	}
	if got := rr.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q, want %q", got, "GET, HEAD")
	}
	body := readBody(t, rr)
	if !bytes.Contains(body, []byte("requires GET")) {
		t.Errorf("body should explain GET-only; got %s", body)
	}
	// The bug we're fixing: must NOT claim the tool doesn't exist.
	if bytes.Contains(body, []byte("unknown tool")) {
		t.Errorf("body still says 'unknown tool' — pre-#609 regression: %s", body)
	}
	if bytes.Contains(body, []byte("available_tools")) {
		t.Errorf("body still leaks tool registry — pre-#609 regression: %s", body)
	}
}

func TestHTTP_DeleteOnGetOnlyStats_Returns405(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	rr := httpDo(t, srv, http.MethodDelete, "/v1/stats")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /v1/stats status = %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q, want %q", got, "GET, HEAD")
	}
}

func TestHTTP_PutOnGetOnlyOpenAPI_Returns405(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	rr := httpDo(t, srv, http.MethodPut, "/v1/openapi.json")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /v1/openapi.json status = %d, want 405", rr.Code)
	}
}

func TestHTTP_HeadDashboard_MirrorsGetHeaders_NoBody(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	getRR := httpDo(t, srv, http.MethodGet, "/v1/dashboard")
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET /v1/dashboard status = %d, want 200", getRR.Code)
	}
	headRR := httpDo(t, srv, http.MethodHead, "/v1/dashboard")
	if headRR.Code != http.StatusOK {
		t.Fatalf("HEAD /v1/dashboard status = %d, want 200", headRR.Code)
	}

	// HEAD body must be empty (RFC 7231 §4.3.2).
	if headRR.Body.Len() != 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", headRR.Body.Len())
	}

	// HEAD headers must mirror GET. Compare the Content-Type at minimum
	// — the rest (CSP, etc.) flows from the same handler.
	if headCT, getCT := headRR.Header().Get("Content-Type"), getRR.Header().Get("Content-Type"); headCT != getCT {
		t.Errorf("HEAD Content-Type %q != GET %q", headCT, getCT)
	}
	if got := headRR.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("HEAD must still carry the CSP header that GET sets")
	}
}

func TestHTTP_HeadHealth_NoBody(t *testing.T) {
	t.Parallel()
	// /v1/health is a public probe (no auth) — the most common HEAD
	// target for k8s/docker liveness. Must not 404 on HEAD just because
	// the handler was authored only for GET.
	srv, _, _ := newTestServer(t)

	rr := httpDo(t, srv, http.MethodHead, "/v1/health")
	if rr.Code != http.StatusOK {
		t.Fatalf("HEAD /v1/health status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD /v1/health body should be empty, got %d bytes", rr.Body.Len())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("HEAD /v1/health Content-Type = %q, want application/json", ct)
	}
}

func TestHTTP_PostOnUnknownTool_StillReturns404(t *testing.T) {
	t.Parallel()
	// Sanity: the #609 fix must not over-broaden. A genuine unknown
	// tool (POST /v1/never_existed) must still 404 with the
	// available_tools list — that's the correct behaviour for non-
	// GET-only paths.
	srv, _, _ := newTestServer(t)

	rr := httpDo(t, srv, http.MethodPost, "/v1/never_existed")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("POST /v1/never_existed status = %d, want 404", rr.Code)
	}
	body := readBody(t, rr)
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj["code"] != "not_found" {
		t.Errorf("error.code = %v, want not_found; body=%s", errObj["code"], body)
	}
	if !strings.Contains(errObj["message"].(string), "never_existed") {
		t.Errorf("message should mention the missing tool name; got %v", errObj["message"])
	}
}

// #714: GET on a path that isn't even a known tool must 404, not 405.
// Pre-fix, the non-POST branch checked method before tool existence, so
// `GET /v1/never_existed` returned "method not allowed — use POST",
// sending the caller into a round-trip that would also fail.
func TestHTTP_GetOnUnknownTool_Returns404(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	rr := httpDo(t, srv, http.MethodGet, "/v1/never_existed")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/never_existed status = %d, want 404; body=%s", rr.Code, readBody(t, rr))
	}
	body := readBody(t, rr)
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj["code"] != "not_found" {
		t.Errorf("error.code = %v, want not_found; body=%s", errObj["code"], body)
	}
	// Must NOT tell the caller to "use POST" — POSTing would also 404.
	if msg, _ := errObj["message"].(string); strings.Contains(msg, "use POST") {
		t.Errorf("404 for unknown tool must not advise 'use POST'; got %q", msg)
	}
	// Should still surface the available_tools list like the POST path.
	if errObj["details"] == nil {
		t.Errorf("expected available_tools in details; body=%s", body)
	}
}

// #714: a POST with a malformed JSON body must return a distinct
// invalid_json_body 400 — not be silently coerced to empty args and
// surface as a misleading "<field> is required" tool error.
func TestHTTP_MalformedJSONBody_Returns400InvalidJSON(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader("{bad json"))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400; body=%s", rr.Code, readBody(t, rr))
	}
	body := readBody(t, rr)
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj["code"] != "invalid_json_body" {
		t.Errorf("error.code = %v, want invalid_json_body; body=%s", errObj["code"], body)
	}
	// The pre-fix bug: must NOT surface as a phantom "query is required".
	if msg, _ := errObj["message"].(string); strings.Contains(msg, "query is required") {
		t.Errorf("malformed JSON masked as missing-field error — pre-#714 regression: %q", msg)
	}
}

// #714 guard: a well-formed empty body still works (defaults to {}).
func TestHTTP_EmptyBody_StillDefaultsToEmptyArgs(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/list", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	// list with no args is valid — should not 400 on the JSON check.
	if rr.Code == http.StatusBadRequest {
		body := readBody(t, rr)
		var resp map[string]any
		json.Unmarshal(body, &resp)
		if errObj, _ := resp["error"].(map[string]any); errObj["code"] == "invalid_json_body" {
			t.Errorf("empty body must not trip the invalid_json_body check; body=%s", body)
		}
	}
}

// errResultRich (#709/#712) emits a JSON envelope through TextContent.
// The HTTP dispatcher must UNWRAP it — lift the inner `error` string to
// `message`, carry next_steps as `details` — not stuff the whole JSON
// blob verbatim into the standardized envelope's `message` field.
func TestHTTP_RichErrorEnvelope_IsUnwrapped(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	// search with a well-formed but empty body → handleSearch returns
	// errResultRich ("query is required" + next_steps).
	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader("{}"))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, readBody(t, rr))
	}
	body := readBody(t, rr)
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}
	errObj, _ := resp["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	// The bug: message must be the human string, NOT a JSON blob.
	if strings.HasPrefix(strings.TrimSpace(msg), "{") {
		t.Errorf("message is a JSON blob — rich envelope not unwrapped: %q", msg)
	}
	if !strings.Contains(msg, "query is required") {
		t.Errorf("message should be the inner error string; got %q", msg)
	}
	// next_steps must survive as structured details, not be lost.
	details, _ := errObj["details"].(map[string]any)
	if details == nil || details["next_steps"] == nil {
		t.Errorf("rich envelope's next_steps should carry through to details; got details=%v", details)
	}
}
