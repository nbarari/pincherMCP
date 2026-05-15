package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #660: GET /v1/ready — k8s-style readiness probe distinct from
// /v1/health (liveness). Returns 200 when the server can serve
// traffic, 503 with structured `reasons` when a dependency is
// missing. Used by orchestrator manifests to gate request routing.

func TestHTTP_ReadyProbe_HealthyServer_Returns200(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/ready", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, w.Body.String())
	}
	if ready, _ := body["ready"].(bool); !ready {
		t.Errorf("ready = %v, want true", body["ready"])
	}
	if _, ok := body["schema_version"]; !ok {
		t.Errorf("schema_version missing from ready response: %v", body)
	}
}

// /v1/ready must accept GET only (RFC 7231 + #609). POSTs return
// 405 Method Not Allowed, not 404 unknown-tool.
func TestHTTP_ReadyProbe_POST_Returns405(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/ready", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to /v1/ready: status = %d, want 405", w.Code)
	}
}

// /v1/ready must be a public probe (no auth required) — k8s probes
// can't carry a bearer token without significant config gymnastics,
// same as /v1/health and /v1/openapi.json (#588).
func TestHTTP_ReadyProbe_NoAuthRequired_EvenWithHTTPKey(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.SetHTTPKey("test-key-12345")

	req := httptest.NewRequest(http.MethodGet, "/v1/ready", nil)
	// Note: no Authorization header.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no-auth probe); body=%s", w.Code, w.Body.String())
	}
}
