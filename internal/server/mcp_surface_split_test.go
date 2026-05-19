package server

import (
	"testing"
)

// MCP surface contract — v0.52 final shape: every registered tool is
// agent-callable via MCP. The v0.35 #624 narrowing (and the v0.51.1
// #644 redirect-stub that compensated for it) were both reversed
// after aggregator-deployment shapes (zelos, bifrost, detour) made
// the original "fewer tools = lower agent decision tax" argument
// obsolete. Under aggregator deployment the agent already faces N
// backends × M tools each; pincher having 22 vs 11 is invisible noise.
//
// API parity (#558 phase 3) is preserved: every tool still has an HTTP
// route at /v1/<name>. CLI ↔ HTTP parity gate is unaffected.

// Expected MCP-visible set. Additions go through design review; the
// test fails until the list is consciously updated.
//
// Tools that were operator-only between v0.35 and v0.51.1 are noted.
var expectedMCPTools = map[string]bool{
	// Working set since v0.35 (always MCP-visible)
	"search":           true,
	"symbol":           true,
	"symbols":          true,
	"context":          true,
	"context_for_task":    true, // #1259 v0.67 composite-context tool
	"investigate_failure": true, // #1391 v0.81 Phase 4 composite — bug-hunt from stack trace
	"plan_change":         true, // #1391 v0.82 Phase 4 composite — pre-edit blast radius
	"audit_unused":        true, // #1391 v0.83 Phase 4 composite — dead-code + deep-trace confirmation
	"trace":            true,
	"query":   true,
	"guide":   true,
	"changes": true,
	"fetch":   true,

	// Restored v0.51 (#645) — core agent tools that v0.35 mistakenly hid
	"index": true,
	"adr":   true,

	// Restored v0.52 (full reversal of #624) — agent-callable under
	// aggregator-shaped deployment for "user opens new repo via Cursor +
	// zelos → agent fires init/index/architecture" workflows
	"architecture": true,
	"dead_code":    true,
	"neighborhood": true,
	"health":       true,
	"stats":        true,
	"schema":       true,
	"list":         true,
	"doctor":       true,
	"rebuild_fts":  true,
	"init":         true,
	"self_test":    true,
}

func TestMCPSurface_AllRegisteredToolsAgentCallable(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	// Every registered tool must be in expectedMCPTools. Test fails
	// when a new tool is added without consciously updating the contract.
	for name := range srv.tools {
		if _, ok := expectedMCPTools[name]; !ok {
			t.Errorf("tool %q is registered but not in expectedMCPTools — add it to the contract list (intentional MCP-visible additions only)", name)
		}
	}

	// Inverse: every name in expectedMCPTools must actually be registered.
	for name := range expectedMCPTools {
		if _, ok := srv.tools[name]; !ok {
			t.Errorf("expectedMCPTools[%q] is not registered — registration removed without test update?", name)
		}
	}
}

func TestMCPSurface_AllToolsHaveHTTPRoute(t *testing.T) {
	t.Parallel()
	// API parity: every tool must be reachable via POST /v1/<tool>. The
	// HTTP dispatcher reads from s.handlers; addTool populates that map.
	// If a future refactor accidentally skips s.handlers, monitoring
	// dashboards / ops pollers / aggregator-fan-out routes break silently.
	srv, _, _ := newTestServer(t)
	for name := range expectedMCPTools {
		if _, ok := srv.handlers[name]; !ok {
			t.Errorf("tool %q missing from s.handlers — HTTP /v1/%s would 404", name, name)
		}
	}
}
