# Three layers of agent-loop leverage

**Status:** v0.73 first draft (#1392). The conceptual frame the rest of
the integration docs build on. Cross-linked from
[`meta-envelope-contract.md`](meta-envelope-contract.md) and the
[`migration guide`](../migration/v0.4-to-v1.0.md).

---

## TL;DR

pincher exposes **three distinct leverage layers** that compose. Each
plugs into a different point of the host's agent loop:

| Layer | Where it fires | What it does | Round-trip cost |
|---|---|---|---|
| **Hook** (pre-call) | BEFORE the agent decides to call a tool | Intercepts `Read` / `Grep` requests; suggests the pincher tool that answers the same question for ~1% of the tokens | 0 — runs inside the host's tool-dispatch hook |
| **`_meta`** (mid-loop) | AFTER any tool call, ON the response | Surfaces typed signals the agent's NEXT call branches on (`next_steps`, `empty_reason`, `warnings_v2`, `capabilities`) | 0 — rides on the response the agent was reading anyway |
| **Composite** (round-trip) | Replaces a typical investigation sequence | Bundles the canonical 5–10 atomic calls behind one tool (e.g. `context_for_task`) | 1 call instead of N |

These layers **aren't alternatives**. They stack. A well-integrated
host uses all three:

- **Hook** prevents bad tool selection.
- **`_meta`** steers good tool selection.
- **Composite** shortens the steered path.

Skip a layer and you leave value on the table. Most integrators wire
up the atomic tools (the obvious surface), miss the hook (the highest-
leverage layer per token saved), and never branch on `_meta` (so they
re-prompt the user for things the envelope already told them).

---

## Why this framing matters

pincher is not "23 MCP tools + an HTTP API." That undersells the
architecture and pushes integrators toward a wiring pattern that
extracts ~20% of the available leverage.

The right mental model is:

> **pincher is the foundation. `_meta` is the enabler. The agent (in
> the host's loop) is the router.**

That ordering matters. pincher exposes data; `_meta` makes the
consequences of each call legible; the LLM in the host's loop reads
both and picks the next call. Pincher itself does not route — there
is no internal traffic cop, no envelope wrapping atomic calls inside
composite ones. Compositions are explicit, named tools whose
implementation runs directly against the same store as the atomics
they would have replaced. No extra round-trips, no nested envelopes.

This framing has two consequences for integration design:

1. **Don't build a routing layer on top of pincher.** Pincher's
   atomic surface + `_meta` is already the routing input. A wrapper
   that consumes pincher's `_meta` and re-routes to pincher tools
   adds latency without adding signal.
2. **Don't expect pincher to grow infra-layer features.** Multi-
   process orchestration, model routing, prompt caching — those are
   host concerns. Pincher's job is to be the cheapest possible
   substrate for the host's loop to consume.

---

## Layer 1 — Hook leverage (pre-call)

### What it is

A host-side intercept that runs BEFORE the agent's tool-dispatch
decision. When the agent reaches for `Read /path/file.go` or
`Grep "func Foo"`, the hook fires, asks pincher whether the same
question can be answered cheaper, and (if yes) substitutes the
suggested pincher call.

Pincher ships `pincher hook-check` as a stateless CLI for this. The
host wires it into its pre-tool hook system; the hook returns either
"proceed with the original call" or "consider this pincher tool
instead" plus a `_meta`-shaped justification.

### Where it works today

| Host | Hook target | Status |
|---|---|---|
| Claude Code CLI | `PreToolUse` hook | Production — installed by `pincher init --target=claude --hooks` |
| Cursor | `.cursor/rules/*.mdc` rule injection | Production — installed by `pincher init --target=cursor` |
| Codex CLI | (TBD per Codex hooks) | Tracked in #1336, v0.77 |
| Zed | (TBD — slash-commands?) | Tracked in #1336, v0.77 |
| VSCode + Copilot | `.github/copilot-instructions.md` | Production — `pincher init --target=vscode-copilot` |
| JetBrains AI Assistant | `.idea/.junie/guidelines.md` | Production — `pincher init --target=jetbrains` |
| Google Antigravity | `.antigravity/rules.md` | Production — `pincher init --target=antigravity` |

`pincher init --target=detect` walks the local filesystem, detects
which hosts are installed, and writes the appropriate hook/rules
file per host in one invocation.

### Why it's the highest-leverage layer

The hook converts before the agent commits. A pincher response that
saves 95% of the tokens of the equivalent `Read` is great. A hook
that prevents the `Read` from being attempted at all is **infinite**
percent saving — the cost of the avoided call is zero.

Measured locally: across pincher's own dogfood corpus, the hook
catches ~60-70% of would-be `Read` / `Grep` calls and redirects
them. Of the redirected calls, the average token savings vs the
original is ~93%. The compound effect is what makes pincher feel
infrastructure-shaped rather than tool-shaped.

Field-data thread for hook conversion-rate measurement lives at
[#640](https://github.com/kwad77/pincher/issues/640).
`pincher hook-stats --export-7d` (v0.71, #662) emits anonymized
trailing-7d metrics for contribution.

### Failure mode if you skip the hook

You ship a working pincher integration where every `search` /
`symbol` / `context` call is a deliberate decision by the agent.
That's fine — but the agent will still reach for `Read` and `Grep`
on the warm-up half of every investigation. You lose the entire
top-of-funnel.

---

## Layer 2 — `_meta` leverage (mid-loop)

### What it is

Every pincher response carries a `_meta` block with typed signals
the agent's NEXT call should branch on. The agent reads these inline
with the response payload — no extra round-trip, no separate query.

Full field-by-field reference at
[`meta-envelope-contract.md`](meta-envelope-contract.md). The
short list of what an agent loop should branch on:

| Field | Decision it drives |
|---|---|
| `next_steps[]` | "what tool should I call next, with what args" — concrete, parameterized suggestions |
| `empty_reason` | "this returned nothing because of X — should I widen the query, switch corpus, or accept the empty result" |
| `warnings_v2[]` | "what should the host UI surface (banner / badge / refresh prompt)" |
| `diagnosis_v2[]` | "what's wrong with this query specifically — typo, missing predicate, wrong taxonomy" |
| `capabilities[]` | "what features does this server actually support — which advertised paths are real" |
| `complexity_tier` | "should the host route this call through a cheaper/expensive model" |
| `tokens_saved_pct` | "is this tool actually saving money on this call, or is it not earning its keep here" |
| `request_id` (also `X-Request-ID` header) | "wire this call into my distributed trace / log correlation" |
| `binary_drift_warning` | "the running pincher binary is older than its DB — auto-restart on next call" |

### Why structured signals beat reading payloads

Pre-`_meta`, an agent had to read the response, infer intent from
free text, and guess next steps. That fails non-deterministically:
the model interprets the same payload differently across turns.

With `_meta`, the agent's branch logic becomes:

```python
response = pincher_call(...)
if response.meta.empty_reason == "no_project_indexed":
    return pincher_call("index", path=cwd)
if response.meta.empty_reason == "query_too_narrow":
    return pincher_call(retry_with_broader_query_from_meta)
for step in response.meta.next_steps:
    schedule(step)
```

That's deterministic, testable, and small — three lines of host code
replace a paragraph of model reasoning per turn.

### Where it works today

Every pincher tool emits `_meta`. The capabilities surface lets the
host gate on advertised features:

```
schema_v34 · hook_check · supervised · operator_tools_on_mcp ·
session_persistence · binary_drift_warning · tokens_used_envelope ·
tokens_saved_pct · standardized_error_envelope · complexity_tier ·
sse · idempotency_declared · metrics_prometheus
```

Plus conditional tags (`closure_tables`, `streamable_http`,
`traces_otlp`, `http_auth`) that appear only when the server is
configured to support them.

### Failure mode if you skip `_meta`

You ship a working pincher integration where the agent reads
payloads and re-derives next steps every turn. Works for one-off
investigations. Fails on the second turn of any multi-call
sequence — `next_steps` and `empty_reason` were the signals you
were supposed to consume, and without them every turn re-explores
ground the previous turn already covered.

This is the most subtle failure mode. It looks fine. The host
appears to be working. But the savings curve plateaus.

---

## Layer 3 — Composite leverage (round-trip)

### What it is

A single MCP tool that bundles the typical N-call investigation
sequence behind one entry point. Today: `context_for_task` (v0.66).

The pattern: an agent investigating "why is this test failing"
typically issues `search` → `symbol` → `context` → `trace` →
`changes` in some combination — 5 to 10 atomic calls, ~50% of which
return information the next call's args could have been derived from.

`context_for_task` accepts the prose task description and returns
the same bundle of information in one round trip. Per-call
implementation runs against the same store the atomics would have —
no nested envelopes, no internal MCP traffic.

### Where it works today

| Composite | Replaces | Status |
|---|---|---|
| `context_for_task` | The typical 5–10 atomic investigation sequence | Production (v0.66, refined v0.72 #1440 AND→OR fallback) |
| Phase 4 successors | Other recurring sequences (audit-shape queries, dead-code workflows) | Roadmap — [#1391](https://github.com/kwad77/pincher/issues/1391) |

`context_for_task` is intentionally the ONLY composite shipped at
v0.73. The Phase 4 roadmap (#1391) names successor patterns but
gates them on real usage data — composites that bundle the wrong
sequences are worse than no composite at all.

### Why composites can't replace `_meta`

A composite is one shot. The agent picks a sequence and pincher runs
it. If the picked sequence was wrong, the composite returns the
wrong bundle. There's no inline correction.

`_meta` keeps every call self-correcting: `next_steps[]` lets the
NEXT call refine. The composite is the steady-state acceleration;
`_meta` is the correction loop. A host that has only composites is
brittle. A host that has only `_meta` is slow. A host that has both
is what v1.0 ships as the recommended integration.

### Failure mode if you skip composites

You ship a working pincher integration that does the right thing
but spends 5–10 round trips per investigation. Latency stacks.
For interactive UIs (Cursor inline chat, Claude Code session), the
user sees 5-10x more "thinking…" indicators than necessary.

---

## Diagram — where each layer fires in a generic agent loop

```
┌─────────────────────────────────────────────────────────────────┐
│  Agent loop (host-controlled)                                   │
│                                                                 │
│   ┌──────────┐    ┌─────────┐    ┌─────────┐    ┌──────────┐    │
│   │ Decide   │ →  │  Call   │ →  │ Observe │ →  │ Re-decide│ ── │
│   │ what to  │    │  tool   │    │ result  │    │  next    │  │ │
│   │   do     │    │         │    │         │    │  call    │  │ │
│   └──────────┘    └─────────┘    └─────────┘    └──────────┘  │ │
│        ▲              │              │              │         │ │
└────────│──────────────│──────────────│──────────────│─────────┘ │
         │              │              │              │           │
         │              │              │              │           │
         │              ▼              ▼              ▼           │
         │       ┌──────────┐   ┌─────────┐   ┌─────────────┐    │
         │       │ Composite │   │  _meta  │   │  next_steps  │    │
         │       │ tool ─── 1│   │ envelope│   │   guide      │    │
         │       │ round-trip│   │ on every│   │  the agent's │    │
         │       │ for the   │   │ response│   │  next pick   │    │
         │       │ whole     │   │         │   │              │    │
         │       │ sequence  │   └─────────┘   └─────────────┘    │
         │       └──────────┘                                      │
         │                                                         │
         └──────── Hook (pre-call) ──────────────────────────────  │
                  Fires BEFORE the loop's first call decision      │
                  Intercepts Read/Grep, suggests pincher tool      │
                                                                   ▼
```

---

## Worked example — same investigation, three integration shapes

**Task:** "Why does the `TestExtractGoCalls_Closure` test sometimes
emit phantom CALLS edges?"

### Shape A — atomic-only (no hook, no `_meta` branching, no composite)

The agent reaches for what it knows: `Read`, `Grep`, `LS`.

```
1. Grep "TestExtractGoCalls_Closure"        → 12 hits across 3 files
2. Read internal/ast/go_callable_shadow_test.go (entire file)
3. Read internal/ast/extractor.go (entire file — 6000 lines)
4. Read internal/index/indexer.go (relevant pass — 800 lines)
5. Grep "phantom CALLS"                      → 0 hits
6. Grep "shadow"                             → 47 hits across 8 files
7. Read each of the 8 files                  → ~30k tokens
```

**Total: 7 calls, ~45,000 tokens read.** Agent is now oriented and
can start reasoning. None of the reads were "wrong" — they were
just bigger than necessary.

### Shape B — atomic pincher + `_meta` branching (no hook, no composite)

The agent skips `Read` / `Grep` because it knows pincher is available.

```
1. search "TestExtractGoCalls_Closure"
   → 1 match. _meta.next_steps suggests context(id).
2. context id="..."
   → returns function + its imports. _meta.warnings_v2 names
     #1429 as the related fix. _meta.next_steps suggests
     trace(id, direction=inbound).
3. trace id="..." direction=inbound depth=1
   → 1 caller (the parent test). _meta.empty_reason="" (real
     answer). _meta.next_steps suggests query for shadow-pattern
     audit.
4. query "MATCH (n:Function) WHERE n.qualified_name CONTAINS
        'shadow' RETURN n.name"
   → 6 matches. Agent now sees the surrounding pattern.
```

**Total: 4 calls, ~3,200 tokens read.** 14× cheaper than Shape A.
Each call's `_meta` shaped the next.

### Shape C — hook + atomic pincher + `_meta` + composite

Same task. The agent's first instinct (which was `Grep` in Shape A)
fires the **hook**:

```
0. [hook intercepts]
   Agent intended: Grep "TestExtractGoCalls_Closure"
   Hook suggests: search "TestExtractGoCalls_Closure"
   → agent accepts the substitution
1. context_for_task task="why does
   TestExtractGoCalls_Closure emit phantom CALLS edges"
   → returns: top-3 ranked symbols, their context + imports,
     inbound traces, related audit-query results, related ADRs.
     _meta.next_steps suggests follow-up traces if needed.
2. (agent has enough — reasons over the bundle and answers)
```

**Total: 1 call (the hook converted the Grep), ~2,800 tokens read.**
~16× cheaper than Shape A. Latency-wise: 1 round-trip vs Shape A's
7, vs Shape B's 4.

---

## Recommended integration order

If you're integrating pincher into a new host, do this:

1. **Wire the atomic tools first.** Get `search` / `symbol` /
   `context` working over MCP. Confirm with `pincher doctor`.
2. **Install the hook.** Run `pincher init --target=<your-host>`.
   Verify with `pincher hook-stats --export-7d` after a few sessions
   that the conversion rate is non-zero.
3. **Wire `_meta` branching.** At minimum: read `empty_reason` and
   `next_steps[]`. Add `warnings_v2` / `capabilities` consumers as
   your host's UI surfaces them.
4. **Add the composite.** Use `context_for_task` for any sequence
   where the agent today fires 3+ atomic calls. Measure the savings
   with `_meta.tokens_saved_pct`.

Skip a step and you still have a working integration — just one
that leaves leverage on the table.

---

## Cross-refs

- [`meta-envelope-contract.md`](meta-envelope-contract.md) — the
  per-field contract for layer 2
- [Migration guide](../migration/v0.4-to-v1.0.md) — version-by-version
  history of when each layer's surface stabilized
- [`docs/REFERENCE.md`](../REFERENCE.md) — the per-tool reference
- [`docs/tutorials/claude-code.md`](../tutorials/claude-code.md) —
  end-to-end walkthrough for one specific host
- [#1391](https://github.com/kwad77/pincher/issues/1391) —
  Phase 4 composite-tool roadmap
- [#640](https://github.com/kwad77/pincher/issues/640) — hook
  conversion-rate field-data thread
