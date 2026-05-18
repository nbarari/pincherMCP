package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #649: every capability declared in _meta.capabilities must correspond
// to a real runtime feature. Probes verify presence; the gate prevents
// false advertising. Add a tag here in lockstep with the feature ship.
//
// Probes are intentionally minimal — each one asserts the feature is
// reachable, not that it works correctly (the feature's own tests
// cover that). The point is to detect "we deleted the feature but
// forgot to drop the capability tag."

type capProbe struct {
	tag   string
	probe func(t *testing.T, srv *Server)
}

var capabilityProbes = []capProbe{
	{
		tag: "schema_v33",
		probe: func(t *testing.T, srv *Server) {
			ver := db.CurrentSchemaVersion()
			if ver != 33 {
				t.Errorf("schema_v33 advertised but CurrentSchemaVersion()=%d", ver)
			}
		},
	},
	{
		tag: "hook_check",
		probe: func(t *testing.T, srv *Server) {
			req := httptest.NewRequest("GET", "/v1/hook-stats", nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Errorf("hook_check advertised but /v1/hook-stats returned %d", rr.Code)
			}
		},
	},
	{
		tag: "supervised",
		probe: func(t *testing.T, srv *Server) {
			if srv.persistentSessionID == "" {
				t.Errorf("supervised advertised but persistentSessionID is empty")
			}
		},
	},
	{
		tag: "operator_tools_on_mcp",
		probe: func(t *testing.T, srv *Server) {
			if _, ok := srv.tools["health"]; !ok {
				t.Errorf("operator_tools_on_mcp advertised but `health` not in s.tools")
			}
			if _, ok := srv.tools["doctor"]; !ok {
				t.Errorf("operator_tools_on_mcp advertised but `doctor` not in s.tools")
			}
		},
	},
	{
		tag: "session_persistence",
		probe: func(t *testing.T, srv *Server) {
			if _, err := srv.store.GetSessionByID(srv.persistentSessionID); err != nil {
				if !strings.Contains(err.Error(), "no rows") {
					t.Errorf("session_persistence advertised but sessions read failed: %v", err)
				}
			}
		},
	},
	{
		tag: "binary_drift_warning",
		probe: func(t *testing.T, srv *Server) {
			srv.driftWarningsEmitted.Store("probe", true)
			if _, ok := srv.driftWarningsEmitted.Load("probe"); !ok {
				t.Errorf("binary_drift_warning advertised but driftWarningsEmitted not functional")
			}
			srv.driftWarningsEmitted.Delete("probe")
		},
	},
	{
		tag: "tokens_used_envelope",
		probe: func(t *testing.T, srv *Server) {
			n := db.ApproxTokens("hello world")
			if n <= 0 {
				t.Errorf("tokens_used_envelope advertised but ApproxTokens returned %d", n)
			}
		},
	},
	{
		tag: "tokens_saved_pct",
		probe: func(t *testing.T, srv *Server) {
			result := srv.jsonResultWithMeta(
				map[string]any{"ok": true},
				time.Now(), "search",
				map[string]any{}, 1000,
			)
			if result == nil || len(result.Content) == 0 {
				t.Fatalf("jsonResultWithMeta returned no content")
			}
			text := result.Content[0].(*mcp.TextContent).Text
			var parsed map[string]any
			if err := json.Unmarshal([]byte(text), &parsed); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			meta := parsed["_meta"].(map[string]any)
			if _, ok := meta["tokens_saved_pct"]; !ok {
				t.Errorf("tokens_saved_pct advertised but not present in _meta")
			}
		},
	},
	{
		tag: "complexity_tier",
		probe: func(t *testing.T, srv *Server) {
			if tier := toolComplexityTier("search"); tier == "" {
				t.Errorf("complexity_tier advertised but search has no classification")
			}
			if tier := toolComplexityTier("guide"); tier != "heavy" {
				t.Errorf("complexity_tier advertised but guide=%q, want heavy", tier)
			}
		},
	},
	{
		tag: "closure_tables",
		probe: func(t *testing.T, srv *Server) {
			var n int64
			if err := srv.store.DB().QueryRow("SELECT COUNT(*) FROM closure").Scan(&n); err != nil {
				t.Errorf("closure_tables advertised but closure table missing: %v", err)
			}
			if n == 0 {
				t.Errorf("closure_tables advertised but closure table is empty")
			}
		},
	},
	{
		tag: "streamable_http",
		probe: func(t *testing.T, srv *Server) {
			if srv.mcpHTTPPath == "" {
				t.Errorf("streamable_http advertised but mcpHTTPPath is empty")
			}
			// Hit the mounted handler with a minimal initialize so we
			// exercise the SDK construction + path routing path.
			body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"capability-probe","version":"v0"}}}`)
			req := httptest.NewRequest("POST", srv.mcpHTTPPath, body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Errorf("streamable_http advertised but POST %s returned %d: %s", srv.mcpHTTPPath, rr.Code, rr.Body.String())
			}
		},
	},
	{
		tag: "standardized_error_envelope",
		probe: func(t *testing.T, srv *Server) {
			req := httptest.NewRequest("POST", "/v1/nonexistent-tool", strings.NewReader("{}"))
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != 404 {
				t.Fatalf("expected 404; got %d", rr.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body not JSON: %v", err)
			}
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Errorf("standardized_error_envelope advertised but error key missing/wrong shape; got %v", body)
			}
			if _, ok := errObj["code"]; !ok {
				t.Errorf("error envelope missing 'code' field")
			}
			if _, ok := errObj["message"]; !ok {
				t.Errorf("error envelope missing 'message' field")
			}
		},
	},
	{
		tag: "idempotency_declared",
		probe: func(t *testing.T, srv *Server) {
			// Advertised tag means OpenAPI spec stamps x-pincher-idempotent
			// on every tool endpoint. Hit /v1/openapi.json and check.
			req := httptest.NewRequest("GET", "/v1/openapi.json", nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			var spec map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &spec); err != nil {
				t.Fatalf("openapi.json parse: %v", err)
			}
			paths := spec["paths"].(map[string]any)
			search, ok := paths["/v1/search"].(map[string]any)
			if !ok {
				t.Fatal("/v1/search missing")
			}
			post := search["post"].(map[string]any)
			if _, has := post["x-pincher-idempotent"]; !has {
				t.Errorf("idempotency_declared advertised but x-pincher-idempotent absent from /v1/search")
			}
		},
	},
	{
		tag: "metrics_prometheus",
		probe: func(t *testing.T, srv *Server) {
			// GET /v1/metrics must answer 200 with text/plain content-type
			// and an exposition body that contains at least the
			// schema-and-tool counter family names we declared.
			req := httptest.NewRequest("GET", "/v1/metrics", nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Errorf("metrics_prometheus advertised but GET /v1/metrics returned %d: %s", rr.Code, rr.Body.String())
			}
			ct := rr.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("metrics_prometheus advertised but Content-Type = %q, want text/plain prefix", ct)
			}
			// Body should at minimum carry the gauges we always
			// refresh on scrape (db_size_bytes / wal_size_bytes).
			body := rr.Body.String()
			for _, want := range []string{"pincher_db_size_bytes", "pincher_wal_size_bytes"} {
				if !strings.Contains(body, want) {
					t.Errorf("metrics_prometheus advertised but body missing %q; got:\n%s", want, body)
				}
			}
		},
	},
	{
		// #1163 v0.67 OTLP traces. Conditional capability: advertised
		// only when OTEL_EXPORTER_OTLP_ENDPOINT was set AND the
		// exporter init succeeded. When advertised, the probe must
		// confirm the tracer is actually attached AND reports Enabled
		// (i.e. not the noop fallback). Without this probe a regression
		// that silently demotes the live tracer back to noop would
		// keep advertising traces_otlp while emitting zero spans.
		// #1164 deliverable: every advertised capability has a runtime
		// probe (same pattern as the v0.59 capability audit).
		tag: "traces_otlp",
		probe: func(t *testing.T, srv *Server) {
			if srv.tracer == nil {
				t.Errorf("traces_otlp advertised but srv.tracer is nil — capability/tracer wiring out of lockstep")
				return
			}
			if !srv.tracer.Enabled() {
				t.Errorf("traces_otlp advertised but srv.tracer.Enabled() = false — the capability is gated on a real exporter, not the noop fallback")
			}
		},
	},
	{
		tag: "sse",
		probe: func(t *testing.T, srv *Server) {
			// GET /v1/events must answer 200 with a text/event-stream
			// body. The handler blocks until the request context is
			// done, so use a short-deadline context: it returns cleanly
			// once headers + the drift snapshot are written.
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			req := httptest.NewRequest("GET", "/v1/events", nil).WithContext(ctx)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Errorf("sse advertised but GET /v1/events returned %d: %s", rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
				t.Errorf("sse advertised but Content-Type = %q, want text/event-stream", ct)
			}
		},
	},
}

// TestCapability_EveryAdvertisedTagHasRuntimeProbe is the lockstep
// gate. Every tag in computeCapabilities() must appear in
// capabilityProbes; every probe must pass on a fresh test server.
// Adding a new capability requires adding both — failure to do so
// fails this test loudly.
func TestCapability_EveryAdvertisedTagHasRuntimeProbe(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	advertised := make(map[string]bool)
	for _, tag := range srv.capabilities {
		advertised[tag] = true
	}

	probed := make(map[string]bool)
	for _, p := range capabilityProbes {
		probed[p.tag] = true
	}

	for tag := range advertised {
		if !probed[tag] {
			t.Errorf("capability %q is advertised but has no probe in capabilityProbes — add one or drop the capability", tag)
		}
	}

	for tag := range probed {
		// Skip conditional capabilities not present on this server.
		if tag == "http_auth" || tag == "streamable_http" || tag == "closure_tables" || tag == "traces_otlp" {
			continue
		}
		if !advertised[tag] {
			t.Errorf("probe for %q exists but the capability is no longer advertised — drop the probe or re-add the advertisement", tag)
		}
	}
}

// TestCapability_AllProbesPass runs every probe and reports failures.
// A failing probe means we are advertising a capability the running
// binary doesn't actually support — false advertising. Fix the probe
// or fix the feature.
func TestCapability_AllProbesPass(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	advertised := make(map[string]bool)
	for _, tag := range srv.capabilities {
		advertised[tag] = true
	}

	for _, p := range capabilityProbes {
		if !advertised[p.tag] {
			continue
		}
		t.Run(p.tag, func(t *testing.T) {
			p.probe(t, srv)
		})
	}
}

// TestCapability_PresentInMetaEnvelope verifies _meta.capabilities is
// populated on a normal tool response (the contract every router will
// rely on).
func TestCapability_PresentInMetaEnvelope(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	result := srv.jsonResultWithMeta(
		map[string]any{"ok": true},
		time.Now(), "search",
		map[string]any{}, 0,
	)
	if result == nil || len(result.Content) == 0 {
		t.Fatalf("jsonResultWithMeta returned no content")
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	meta := parsed["_meta"].(map[string]any)
	caps, ok := meta["capabilities"].([]any)
	if !ok {
		t.Fatalf("_meta.capabilities missing or wrong type; got %T: %v", meta["capabilities"], meta["capabilities"])
	}
	if len(caps) == 0 {
		t.Errorf("_meta.capabilities present but empty")
	}
	found := false
	for _, c := range caps {
		if c == "schema_v33" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("_meta.capabilities missing schema_v33; got %v", caps)
	}
}

// TestCapability_HTTPAuthConditional verifies http_auth advertisement
// is conditional on httpKey being set.
func TestCapability_HTTPAuthConditional(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	// Default test server has no httpKey → http_auth must be absent.
	for _, c := range srv.capabilities {
		if c == "http_auth" {
			t.Errorf("http_auth advertised on a server with no httpKey set")
		}
	}

	// SetHTTPKey is a runtime mutation; the cached capabilities slice
	// won't pick it up (deliberate simplification per computeCapabilities
	// docstring). A fresh server with a key set at New time would
	// advertise it — covered by integration tests for the supervised
	// + --http-key combination.
}
