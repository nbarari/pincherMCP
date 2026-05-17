package server

import (
	"os"
	"strings"
)

// #1088: opt-in short tool descriptions. Default-off preserves the
// pedagogically-dense descriptions every consumer reading tools/list
// today gets. Operators running heavy-traffic aggregators that spawn
// fresh MCP sessions (zelos / bifrost / detour / CI bots) can opt
// in via PINCHER_TOOL_DESCRIPTIONS=short to trim ~3 KB of repeated
// boilerplate off every session-start handshake.
//
// Short forms intentionally cover only the 5 longest descriptions
// (trace / search / neighborhood / query / changes — measured in
// #1088 as ~3.5 KB of the 12 KB total). The other ~18 tools have
// already-tight descriptions; trimming them further is below the
// noise floor and would erode pedagogy for marginal savings.
//
// Full pedagogical content stays in docs/REFERENCE.md per-tool
// sections (the #638 v0.40 docs split makes this lossless — agents
// running with short descriptions can still fetch the full guidance
// via `pincher_fetch` or by reading the REFERENCE page directly).

// shortToolDescriptions maps tool name → trimmed one-sentence form.
// Only the heavy hitters from #1088 are listed; anything not in the
// map keeps its original description even when the env opt-in fires.
var shortToolDescriptions = map[string]string{
	"trace":        "Find callers (inbound) or callees (outbound) of a symbol via callgraph BFS. Pass `name` or `id`; default kinds=CALLS; risk-labels CRITICAL/HIGH/MEDIUM by depth. See REFERENCE.md for full options.",
	"search":       "FTS5 BM25 search for code by name or content (use before Grep/Read). Returns signature + query-aware snippet. Filter by kind/language/corpus. See REFERENCE.md for full query syntax.",
	"neighborhood": "Returns same-file symbols (NOT graph adjacency — for that use `trace`). One round-trip vs N `symbol` calls or a whole-file Read. See REFERENCE.md.",
	"query":        "pinchQL graph queries — Cypher-shaped subset (MATCH/WHERE/RETURN/LIMIT, single-hop joins, bounded BFS). Use for structural relationships. See REFERENCE.md for syntax + examples.",
	"changes":      "Maps `git diff` to affected symbols + BFS-traces impact + ranks `tests_to_run`. Scopes: unstaged/staged/all/base:<branch>. Use before final response after edits.",
}

// parseToolDescriptionsEnv reads PINCHER_TOOL_DESCRIPTIONS and reports
// whether short descriptions are requested. Default false (long-form
// preserved). Mirrors parseCapabilitiesEnv's case + whitespace
// tolerance and unknown-value safety: only the canonical "short"
// triggers the opt-in; anything else (typo, alternate spelling,
// blank) keeps the long-form default. Failure-as-pedagogy: a typo'd
// "shortt" doesn't silently swap descriptions; operator notices when
// their tools/list payload measurement shows no change.
func parseToolDescriptionsEnv(v string) bool {
	return strings.ToLower(strings.TrimSpace(v)) == "short"
}

// applyShortDescriptionsIfRequested overwrites the in-memory tool
// descriptions on `s.tools` when PINCHER_TOOL_DESCRIPTIONS=short is
// set at server start. Called once from New() after registerTools.
// Idempotent: re-running with the same env produces the same result.
// No-op when env unset or set to anything other than "short".
//
// Tools not present in shortToolDescriptions are left untouched —
// only the 5 heavy hitters from #1088 are trimmed.
func (s *Server) applyShortDescriptionsIfRequested() {
	if !parseToolDescriptionsEnv(os.Getenv("PINCHER_TOOL_DESCRIPTIONS")) {
		return
	}
	for name, short := range shortToolDescriptions {
		if tool, ok := s.tools[name]; ok && tool != nil {
			tool.Description = short
		}
	}
}
