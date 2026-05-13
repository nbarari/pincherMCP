package server

import (
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
		tag: "schema_v24",
		probe: func(t *testing.T, srv *Server) {
			ver := db.CurrentSchemaVersion()
			if ver != 24 {
				t.Errorf("schema_v24 advertised but CurrentSchemaVersion()=%d", ver)
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
}

// TestCapability_EveryAdvertisedTagHasRuntimeProbe is the lockstep
// gate. Every tag in computeCapabilities() must appear in
// capabilityProbes; every probe must pass on a fresh test server.
// Adding a new capability requires adding both — failure to do so
// fails this test loudly.
func TestCapability_EveryAdvertisedTagHasRuntimeProbe(t *testing.T) {
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
		if tag == "http_auth" {
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
		if c == "schema_v24" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("_meta.capabilities missing schema_v24; got %v", caps)
	}
}

// TestCapability_HTTPAuthConditional verifies http_auth advertisement
// is conditional on httpKey being set.
func TestCapability_HTTPAuthConditional(t *testing.T) {
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
