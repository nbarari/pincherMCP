package server

import (
	"testing"
)

// #624: MCP exposes Tier 1+2 (search/symbol/symbols/context/trace/
// query/guide/changes/fetch). Operator tools (architecture/health/
// schema/list/index/adr/neighborhood/stats/doctor/rebuild_fts/
// self_test/dead_code) stay reachable via /v1/<tool> HTTP but are
// off the agent-facing MCP surface. Cuts agent decision-tax of "which
// tool do I reach for" from 22 entries to 9.
//
// API parity (#558 phase 3) is preserved: every operator handler still
// has an HTTP route. CLI ↔ HTTP parity gate is unaffected — that gate
// asserts CLI subcommands have HTTP endpoints, not that they're MCP-
// visible.

// Expected MCP-visible set per the v0.35 design conversation. Update
// this whenever the agent-facing surface changes (additions go through
// design review; the test fails until the list is consciously updated).
var expectedMCPTools = map[string]bool{
	"search":   true,
	"symbol":   true,
	"symbols":  true,
	"context":  true,
	"trace":    true,
	"query":    true,
	"guide":    true,
	"changes":  true,
	"fetch":    true,
}

// Expected operator-only set (HTTP route exists, MCP doesn't expose).
var expectedOperatorTools = map[string]bool{
	"index":        true,
	"dead_code":    true,
	"architecture": true,
	"schema":       true,
	"list":         true,
	"adr":          true,
	"health":       true,
	"stats":        true,
	"neighborhood": true,
	"init":         true,
	"doctor":       true,
	"rebuild_fts":  true,
	"self_test":    true,
}

func TestMCPSurface_OnlyAgentFacingToolsExposedToMCP(t *testing.T) {
	// We can't directly introspect the MCP server's registered tools
	// (the SDK doesn't expose a listing API on *mcp.Server). What we
	// CAN check: every tool in s.tools that's NOT in expectedMCPTools
	// must have been registered via addOperatorTool, which means it
	// won't be on the MCP side.
	//
	// The boundary check below catches the regression we're guarding
	// against: a future tool added via s.addTool that should have been
	// addOperatorTool. Failure mode is loud — list mismatch.

	srv, _, _ := newTestServer(t)

	for name := range srv.tools {
		_, isAgent := expectedMCPTools[name]
		_, isOperator := expectedOperatorTools[name]
		if !isAgent && !isOperator {
			t.Errorf("tool %q is registered but unclassified — add it to expectedMCPTools or expectedOperatorTools and pick the matching addTool/addOperatorTool registration site", name)
		}
		if isAgent && isOperator {
			t.Errorf("tool %q is in both agent and operator sets — pick one", name)
		}
	}

	// Inverse check: every name in our expected sets must actually be
	// registered (catches typos in the test sets, or removals without
	// updating the list).
	for name := range expectedMCPTools {
		if _, ok := srv.tools[name]; !ok {
			t.Errorf("expectedMCPTools[%q] is not registered — registration removed without test update?", name)
		}
	}
	for name := range expectedOperatorTools {
		if _, ok := srv.tools[name]; !ok {
			t.Errorf("expectedOperatorTools[%q] is not registered — registration removed without test update?", name)
		}
	}
}

func TestMCPSurface_AllOperatorToolsHaveHTTPRoute(t *testing.T) {
	// API parity: every operator tool — even though not on MCP — must
	// still be reachable via POST /v1/<tool>. The HTTP dispatcher reads
	// from s.handlers; addOperatorTool populates that map. If a future
	// refactor accidentally skips s.handlers for operator tools,
	// monitoring dashboards / ops pollers break silently.
	srv, _, _ := newTestServer(t)
	for name := range expectedOperatorTools {
		if _, ok := srv.handlers[name]; !ok {
			t.Errorf("operator tool %q missing from s.handlers — HTTP /v1/%s would 404", name, name)
		}
	}
}

func TestMCPSurface_AgentToolsHaveHandlersToo(t *testing.T) {
	// Sanity: agent-facing tools also live in s.handlers (so they're
	// reachable via both MCP AND HTTP — the dual-protocol contract).
	srv, _, _ := newTestServer(t)
	for name := range expectedMCPTools {
		if _, ok := srv.handlers[name]; !ok {
			t.Errorf("agent tool %q missing from s.handlers — HTTP /v1/%s would 404", name, name)
		}
	}
}
