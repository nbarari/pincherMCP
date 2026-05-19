# Phase 4 composite-tool roadmap

**Status:** v0.74 first draft ([#1391](https://github.com/kwad77/pincher/issues/1391));
refreshed 2026-05-18 with shipped vs in-flight markers + replanned
sequencing per `.planning-roadmap-to-v1.md`.

Names the composite-tool patterns slated for Phase 4 (v0.81 → v0.90)
so the work has a roadmap to execute against. Pairs with
[`loop-leverage-layers.md`](./loop-leverage-layers.md) (Layer 3 —
composite call) and [`meta-envelope-contract.md`](./meta-envelope-contract.md)
(every composite emits typed `_meta` per the same contract).

## Composite shipping status

| # | Composite | Release | Status |
|---|---|---|---|
| 1 | `investigate_failure` | v0.81 | ✅ shipped (PR #1517) |
| 2 | `plan_change` | v0.82 | ✅ shipped (PR #1519) |
| 3 | `audit_unused` | v0.82 | ✅ shipped (PR #1546) |
| 4 | `onboard_module` | v0.82 | ✅ shipped (PR #1548) |
| 5 | `why_empty` | v0.82 | ✅ shipped (PR #1549) — Phase 4 suite complete |

---

## Why composites

Pincher exposes 23 atomic MCP tools. An investigation typically
fires 5–10 of them in sequence:

```
search(task)         → top-N seeds
context(seed) × N    → source + direct deps
trace(seed) × N      → callers + callees, depth ≤ 2
changes overlap      → recent edits intersecting the seeds
```

Each call is its own round-trip: agent emits → host marshals → MCP
transport → pincher handler → JSON envelope back → agent re-parses →
plans the next call. The atomic shape is correct (composability,
inspectability, audit trail) — but the round-trip overhead compounds.

A composite tool bundles a recognised sequence behind one entry point.
**Same data store, one round-trip, same envelope shape.** The
synthesized answer ships alongside the trace of what was probed so
the agent can verify each step.

The precedent: `context_for_task` ([#1283], v0.66, refined v0.72 #1440
AND→OR fallback). One call replaces the canonical 5–10-call
investigation. Latency drops ~5× on interactive UIs; the agent stops
seeing "thinking…" indicators between every atomic call. Per
real-corpus measurements in `pincher bench`, the median composite
investigation lands inside a single 200ms budget where the atomic
sequence ran 1.0–1.4s of wall-clock.

Phase 4 generalises the pattern: each composite encodes a recognised
investigation shape, ships its own audit suite, evolves the `_meta`
envelope in lockstep.

---

## The five candidate composites

Each entry below names the composite, the today-shape (atomic-call
sequence it replaces), the synthesized output, and the dogfood
provenance that motivated naming it.

### 1. `investigate_failure` — the bug-hunt loop

**Today (atomic).** Agent receives a stack trace or error string,
fires:

```
search(text=stack_top_frame)        → implicated symbols
trace(seed=symbol, direction=in)    → callers up to depth 2
changes(scope=branch)               → recent edits in the implicated files
context(seed=top-ranked symbol)     → source for diff review
```

**Composite.** `investigate_failure(error_text, max_suspects=5)` returns:

```json
{
  "implicated_symbols": [...],     // ranked by stack-frame match + recent-change overlap
  "callers": [...],                // BFS up to depth 2, scoped to the implicated set
  "recent_changes": [...],         // diff hunks intersecting any implicated symbol
  "rank": [{
    "symbol_id": "...",
    "score": 0.87,
    "evidence": ["stack_frame_match", "modified_in_last_5_commits", "high_caller_fan_in"]
  }],
  "_meta": {
    "next_steps": [
      {"tool": "context", "args": {"id": "..."}, "why": "read top-ranked suspect"},
      {"tool": "trace", "args": {"id": "...", "direction": "out"}, "why": "verify callee assumptions"}
    ],
    "diagnosis_v2": [...]          // typed reason codes per the meta-envelope contract
  }
}
```

**Why this one first.** Bug investigation is the highest-frequency
agent loop on a working codebase. Today's atomic sequence requires
the agent to maintain stack-frame-to-symbol mapping state across
4 separate calls; the composite holds that state for one round-trip.

**Replaces:** ~5 atomic calls → 1.

### 2. `audit_unused` — dead-code with suppression rationale

**Today (atomic).** `dead_code` is already a composite-shaped atomic —
it bundles in-graph caller-evidence with the receiver-type /
interface-dispatch / function-value-as-field false-positive
suppression that closed the dead-code FP triangle (v0.57–v0.62).
Coverage gaps: language scoping is a flag (not a default), the
suppression rationale per candidate isn't part of the response, and
the agent must re-run `trace` on each candidate to verify the
"unused" claim.

**Composite.** `audit_unused(language, kind, max_results=20)` returns:

```json
{
  "candidates": [{
    "symbol_id": "...",
    "kind": "Function",
    "language": "Go",
    "in_graph_caller_count": 0,
    "suppression_status": "confirmed_unused",
    "evidence": [
      "no_inbound_CALLS_edges",
      "no_function_value_bindings_in_v0.21_blocklist",
      "no_interface_implementation_via_OVERRIDE",
      "not_an_entry_point_per_arch_aspect"
    ],
    "trace_summary": {"depth_2_callers": 0, "depth_4_callers": 0}
  }, ...],
  "false_positive_audit": {
    "triangle_legs_checked": 3,
    "suppressed_in_this_run": 7,
    "suppression_breakdown": {"receiver_type": 3, "function_value": 2, "interface_dispatch": 2}
  }
}
```

**Why.** The FP triangle close was the v0.57–v0.62 anchor; making the
audit trail first-class is the next polish step. Agents that
currently distrust `dead_code` results (because the triangle close
isn't visible in the response) can see exactly which rules suppressed
which candidates — and which candidates survived the audit.

**Replaces:** 1 `dead_code` + N `trace` confirmation calls → 1.

### 3. `plan_change` — pre-edit blast radius

**Today (atomic).**

```
changes(unstaged=true)              → what's modified locally
trace(seed=changed_symbol, dir=in)  → who calls this symbol
context(seed=top_caller)            → read the call sites
adr(action=list)                    → check stored architectural decisions
```

**Composite.** `plan_change(file_or_symbol, depth=2)` returns:

```json
{
  "target": {"file": "...", "symbols_affected": [...]},
  "blast_radius": {
    "depth_1_callers": [...],     // CRITICAL risk
    "depth_2_callers": [...],     // HIGH risk
    "cross_package": [...],       // explicit list when callers span boundaries
    "test_files_intersecting": [...]
  },
  "related_adrs": [...],           // ADR records matching keywords from the target's package
  "_meta": {
    "warnings_v2": [
      {"code": "blast_radius_high", "depth_1_caller_count": 14, "suggestion": "consider staged refactor"}
    ]
  }
}
```

**Why.** The pre-edit confirmation step today is "fire `changes`,
fire `trace`, fire `adr list`, eyeball each separately." The
composite emits ONE risk-classified payload sized for the agent's
decision: ship or pause for human review.

**Replaces:** ~4 atomic calls → 1.

### 4. `onboard_module` — new-contributor orientation

**Today (atomic).**

```
architecture(aspects=[entry_points,packages])  → orient
search(file_pattern=path/**, label=Function)   → enumerate
trace(seed=entry_point, dir=out, depth=3)      → see what the entry points reach
context(seed=top_call_target) × N              → read the implementation
```

**Composite.** `onboard_module(directory_path, depth=3)` returns:

```json
{
  "scope": {"directory": "...", "file_count": 47, "symbol_count": 312},
  "entry_points_local_to_scope": [...],         // mains, init funcs, public API surface
  "internal_call_graph": {"nodes": [...], "edges": [...]},  // call edges within scope
  "external_dependencies": [...],               // symbols outside scope this module calls
  "external_consumers": [...],                  // symbols outside scope that call into this module
  "module_summary": {
    "language_breakdown": {"Go": 0.92, "YAML": 0.08},
    "test_to_code_ratio": 0.71,
    "exported_surface_count": 18
  }
}
```

**Why.** Today an agent (or a new human contributor) lands in an
unfamiliar directory and has to assemble four separate atomic queries
to answer "what does this module do, what does it depend on, who
depends on it." The composite is the orientation-shaped answer.

**Replaces:** ~5 atomic calls → 1.

### 5. `why_empty` — cross-tool diagnostic

**Today (atomic).** Each tool that returns an empty result stamps
`_meta.empty_reason` ([#1252] enum). The agent reads the per-tool
narrative, but if the same empty result happened across multiple
tools (e.g., search returned nothing AND trace returned nothing AND
neighborhood returned nothing) the agent has to manually correlate
the three diagnostics.

**Composite.** `why_empty(tool_call_log)` accepts a list of
recent tool calls + their empty results, returns:

```json
{
  "correlated_diagnosis": "Project 'foo' is indexed but the symbol you searched for matches only un-extracted bash files (confidence < min_confidence threshold). All three queries hit the same root cause.",
  "root_cause": "min_confidence_filter_too_strict",
  "suggested_recovery": [
    {"tool": "search", "args": {"min_confidence": 0.7}, "why": "lower the floor to include regex-tier matches"},
    {"tool": "doctor", "args": {}, "why": "check whether the language has full extraction coverage"}
  ],
  "per_tool_reasons": [...]        // original empty_reason from each call, preserved
}
```

**Why.** Pure pedagogy composite. Pincher already invests heavily in
per-tool empty-state diagnosis (the silent-confidently-wrong family
drain across v0.59). The composite makes that investment legible
across calls. Especially valuable when an agent is debugging its own
investigation: "I made 4 calls, all empty, what's wrong?"

**Replaces:** N atomic diagnoses (with manual correlation) → 1 (with
machine correlation).

---

## Sequencing across v0.81–v0.90

Phase 4 is the v0.81 → v0.90 block, ending with the v0.90 stable
promotion + v1.0 release-prep.

| Release | Composite | Rationale | Status |
|---|---|---|---|
| v0.81 | `investigate_failure` | Highest-frequency loop; biggest immediate ROI | ✅ shipped |
| v0.82 | `plan_change` | Pre-edit confirmation; complements the existing `changes` atomic | ✅ shipped (PR #1519) |
| v0.82 | `audit_unused` | Polish on existing `dead_code`; lowest implementation risk | ✅ shipped (PR #1546) — bundled into v0.82 megarelease |
| v0.82 | `onboard_module` | Architecture-shaped; refines `architecture` aspect surface; paired with the API freeze checkpoint (FILE-K + FILE-L) | ✅ shipped (PR #1548) — bundled into v0.82 megarelease |
| v0.82 | `why_empty` | Cross-tool diagnostic; depends on every other composite already shipping with structured `_meta.diagnosis_v2`; paired with the failure-mode catalog (FILE-O) | ✅ shipped (PR #1549) — Phase 4 suite complete |
| v0.83+ | Non-composite v1.0 blockers (field data, security, supply chain) per `.planning-roadmap-to-v1.md` | — | 🚧 scaffolded across v0.83–v0.97 |
| v0.89 | HARDENING (no new features) | — | ⏳ |
| v0.90 | STABLE PROMOTION (channel re-tag) | — | ⏳ |

The acceptance criteria from [#1391] require all 5 composites shipped
across the block, each with its own audit suite. The v0.86–v0.88
slots that were originally "iteration" now carry the production-
readiness gaps identified in the v1.0 plan refresh.

---

## Contract invariants every composite must hold

**Additive only.** A composite never breaks an atomic tool's
contract. Atomic tools remain callable; the composite is a NEW
surface, not a replacement.

**No internal MCP round-trips.** Composites are implemented as direct
SQL + in-process orchestration. They do not internally call the MCP
transport. (This is the load-bearing distinction the user pinned
during v0.74 scoping — see
[loop-leverage-layers.md → Common failure modes](./loop-leverage-layers.md).
The Stoa family stack's latency was the original anti-example. A
composite that round-trips MCP defeats its own latency thesis.)

**Single envelope.** One response, one `_meta` block. Nested envelopes
are not allowed. If a composite needs to surface the atomic-call
trace, it embeds the trace as data fields — not as nested
tool-response objects.

**Per-composite `_meta.diagnosis_v2` codes.** Each composite extends
the diagnosis-code enum with reasons specific to its synthesis. New
codes are additive; old codes never get renamed.

**Per-composite audit suite.** Positive + negative + control +
cross-check shape, same as atomic tools. The audit shape is encoded
in tests under `internal/server/<composite>_test.go` and pinned in
the corpus snapshots where the composite's response feeds the
snapshot.

**`empty_reason` mandatory.** A composite that returns zero
synthesized result must stamp `_meta.empty_reason` with a code from
the shared enum (extended with composite-specific reasons as needed).

**Idempotency.** Composites are read-only. They may cache derived
state in process memory but never mutate the SQLite store. The
underlying atomic tools maintain this property; composites inherit
it.

---

## Out of scope for Phase 4

- **Re-platforming atomic tools as Resources** ([#1083]). Separate
  architectural shift; rolls forward in Phase 4 on its own cadence
  but is not gated on composite work.
- **Backwards-incompatible composite responses.** The v1.0 freeze
  applies to ALL tools including future composites. Design them
  additive-only from day one.
- **Cross-project composites.** Phase 4 composites all scope to a
  single project (per the strict-cross-project guard tightened in
  v0.66 / v0.72). Cross-project orchestration is a v1.x story.

---

## Cross-refs

- [#1391] — this roadmap's umbrella issue
- [#1283] — `context_for_task` v0.66 (the precedent)
- [#1252] — `empty_reason` enum substrate
- [#1098] — structured `_meta.warnings_v2` v0.71 (the envelope substrate)
- [#1080] / [#1081] — MCP notifications + roots/list (push-channel for composite diagnostics)
- [#638] — v1.0 launch checklist
- [#667] — Phase 5 umbrella (post-RC launch)

[#1391]: https://github.com/kwad77/pincher/issues/1391
[#1283]: https://github.com/kwad77/pincher/issues/1283
[#1252]: https://github.com/kwad77/pincher/issues/1252
[#1098]: https://github.com/kwad77/pincher/issues/1098
[#1080]: https://github.com/kwad77/pincher/issues/1080
[#1081]: https://github.com/kwad77/pincher/issues/1081
[#1083]: https://github.com/kwad77/pincher/issues/1083
[#638]: https://github.com/kwad77/pincher/issues/638
[#667]: https://github.com/kwad77/pincher/issues/667
