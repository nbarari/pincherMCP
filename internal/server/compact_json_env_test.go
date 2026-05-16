package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// #1089 escape-hatch: PINCHER_DEBUG_META=1 → pretty-printed across every
// encode site. Pre-fix, three call sites encoded `_meta`-bearing bodies
// independently: jsonResultWithMeta (success), errResultRich (error),
// and the withRequestID middleware re-encode. errResultRich ignored the
// env flag entirely; the middleware re-encode reset success bodies to
// compact. This test pins all three sites through a shared encoder.
//
// NOT parallel — env var is process-global. Each sub-case sets+restores
// the env locally so the suite can be reordered without state leaking.
func TestToolResponse_DebugEnvPretty(t *testing.T) {
	withEnv := func(t *testing.T, val string) {
		t.Helper()
		prev, had := os.LookupEnv("PINCHER_DEBUG_META")
		t.Cleanup(func() {
			if had {
				os.Setenv("PINCHER_DEBUG_META", prev)
			} else {
				os.Unsetenv("PINCHER_DEBUG_META")
			}
		})
		if err := os.Setenv("PINCHER_DEBUG_META", val); err != nil {
			t.Fatalf("setenv: %v", err)
		}
	}

	// prettyMarker is the unambiguous signal that json.MarshalIndent
	// (two-space) ran instead of json.Marshal: a newline followed by
	// the indent + an opening quote. Compact JSON never contains this.
	const prettyMarker = "\n  \""

	t.Run("success_path_via_jsonResultWithMeta", func(t *testing.T) {
		withEnv(t, "1")
		srv, _, _ := newTestServer(t)
		// handleSchema with no project arg resolves through the
		// session-id branch and reaches jsonResultWithMeta's success
		// encode path (with a diagnosis _meta on the empty graph).
		res, err := srv.handleSchema(context.Background(), makeReq(nil))
		if err != nil {
			t.Fatalf("handleSchema: %v", err)
		}
		text := textOf(t, res)
		if !strings.Contains(text, prettyMarker) {
			t.Fatalf("expected pretty JSON from jsonResultWithMeta; got compact\nhead: %.200s", text)
		}
		// Belt-and-suspenders: it must still parse as JSON. A pretty
		// marker without valid JSON would mean we accidentally
		// produced a string-literal containing the bytes.
		var parsed map[string]any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
	})

	t.Run("error_path_via_errResultRich", func(t *testing.T) {
		withEnv(t, "1")
		srv, _, _ := newTestServer(t)
		// neighborhood with no id triggers errResultRich's
		// "id is required" path. Picking a tool that errors
		// deterministically without DB state avoids coupling the
		// test to handleSchema's behaviour.
		res := srv.errResultRich("forced error for test", []map[string]string{
			{"tool": "list", "args": "{}", "why": "see indexed projects"},
		})
		text := textOf(t, res)
		if !strings.Contains(text, prettyMarker) {
			t.Fatalf("expected pretty JSON from errResultRich; got compact\nhead: %.200s", text)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		if parsed["error"] != "forced error for test" {
			t.Errorf("error field mismatch: %v", parsed["error"])
		}
	})

	t.Run("middleware_reencode_via_injectRequestID", func(t *testing.T) {
		withEnv(t, "1")
		srv, _, _ := newTestServer(t)
		res, err := srv.handleSchema(context.Background(), makeReq(nil))
		if err != nil {
			t.Fatalf("handleSchema: %v", err)
		}
		// Simulate the middleware re-encode. Pre-fix this stage
		// always reset the body to compact even when the env flag
		// asked for pretty — caused #1089's escape hatch to be
		// honored only for stdio, not the HTTP path.
		injectRequestID(res, "test-request-id-1234")
		text := textOf(t, res)
		if !strings.Contains(text, prettyMarker) {
			t.Fatalf("expected pretty JSON after middleware re-encode; got compact\nhead: %.200s", text)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		meta, ok := parsed["_meta"].(map[string]any)
		if !ok {
			t.Fatalf("_meta missing or wrong type: %v", parsed["_meta"])
		}
		if meta["request_id"] != "test-request-id-1234" {
			t.Errorf("request_id not injected: %v", meta["request_id"])
		}
	})

	t.Run("env_unset_yields_compact_at_every_site", func(t *testing.T) {
		// Explicit unset — guards against a prior sub-case leaking
		// the env if t.Cleanup ordering surprises us.
		os.Unsetenv("PINCHER_DEBUG_META")
		srv, _, _ := newTestServer(t)

		// Success path.
		res, err := srv.handleSchema(context.Background(), makeReq(nil))
		if err != nil {
			t.Fatalf("handleSchema: %v", err)
		}
		if text := textOf(t, res); strings.Contains(text, prettyMarker) {
			t.Errorf("expected compact JSON with env unset (success); got pretty\nhead: %.200s", text)
		}

		// Error path.
		errRes := srv.errResultRich("forced error", nil)
		if text := textOf(t, errRes); strings.Contains(text, prettyMarker) {
			t.Errorf("expected compact JSON with env unset (error); got pretty\nhead: %.200s", text)
		}

		// Middleware re-encode.
		injectRequestID(res, "compact-test-id")
		if text := textOf(t, res); strings.Contains(text, prettyMarker) {
			t.Errorf("expected compact JSON with env unset (middleware); got pretty\nhead: %.200s", text)
		}
	})
}
