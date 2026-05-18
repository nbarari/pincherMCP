# The `_meta` envelope — integrator contract

**Status:** v0.73 first draft (#1394). Pairs with
[`loop-leverage-layers.md`](loop-leverage-layers.md) — the
conceptual frame. This page is the **field-by-field contract**.

The `_meta` block is pincher's planning-loop input surface. Every
tool response carries one. Agent loops branch on it. Host UIs
render off it. Observability pipelines correlate on it.

> **Stability commitment:** Pre-v1.0, the envelope evolves. New
> fields are pure additions; existing fields keep emitting. Once
> v1.0 ships, the field names AND their value taxonomy freeze —
> any change is a major bump. This page is the contract that
> freezes.

---

## Anatomy of a response

```jsonc
{
  "result": { /* tool-specific payload */ },
  "_meta": {
    // — Identification —
    "request_id": "01HM3K2X9V7QY8R4PNZTH5W6BC",  // ULID

    // — Cost accounting —
    "tokens_used": 1247,
    "tokens_saved": 11583,
    "tokens_saved_pct": 90.3,
    "baseline_method": "full_file_read",   // or "grep_recursive" etc.
    "complexity_tier": "lite",             // "lite" | "standard" | "heavy"
    "latency_ms": 38,

    // — Result diagnosis (when empty / partial / surprising) —
    "empty_reason": "query_too_narrow",
    "diagnosis": "tightly-bounded query returned no rows; widen by …",
    "diagnosis_v2": [ { "code": "...", "severity": "warning", ... } ],

    // — Steering signals —
    "next_steps": [
      { "tool": "search", "args": { "q": "broader term" },
        "why": "current query was AND-joined; OR retry usually wins" }
    ],
    "warnings": ["legacy string form"],
    "warnings_v2": [ { "code": "...", "severity": "info", ... } ],

    // — Capability advertisement —
    "capabilities": ["schema_v33", "hook_check", "supervised", "..."],

    // — Drift / staleness —
    "binary_drift_warning": null,          // or { stored_version, running_version }
    "schema_version": 33,
    "schema_version_at_index": 33,         // per-project (may differ from server)

    // — Project + session context —
    "project_id": "d:claudecodepincher-repo",
    "project_name": "pincher-repo",
    "session_id": "..."
  }
}
```

Not every call emits every field. Tool-specific surfaces stamp the
fields relevant to that tool's shape; missing fields mean "not
applicable to this call." Agents should treat a missing field as
absent, never as "value = 0/false/empty".

---

## Fields that drive next-call decisions

### `next_steps[]`

Concrete, parameterized suggestions for the agent's next call.
Each entry has `tool` (an MCP tool name pincher serves), `args`
(an object the agent passes through to the next call), and `why`
(prose explaining the suggestion — for logs / debugging, not for
LLM consumption).

The agent's loop should:

```
for step in response.meta.next_steps:
    if step.tool in trusted_tools:
        schedule_call(step.tool, **step.args)
```

`next_steps` is the most reliable steering signal in the envelope.
Pincher only emits suggestions it would itself execute given the
context — there's no "and maybe try X" speculation. If the field
is empty, no steering is needed.

### `empty_reason`

When a tool returns no results, `empty_reason` names *why*. Stable
enum (`internal/server/empty_reason.go`):

| Value | Meaning | Recommended agent response |
|---|---|---|
| `no_project_indexed` | No project at this path is indexed | Call `index` with the cwd |
| `stale_index` | Project is indexed but `schema_version_at_index` < server's | Suggest re-index to the user; the result may be missing recent symbols |
| `unsupported_language` | Query targets a language pincher doesn't extract | Don't retry; surface to the user |
| `low_confidence_extractor` | Result set was filtered by `min_confidence` | Retry with lower `min_confidence` |
| `same_file_only` | Cross-file resolution unavailable for the dominant language | Surface the limitation; the result is honest |
| `cross_file_unavailable` | Edge type requested doesn't ship for this language | Same as above |
| `query_too_narrow` | All predicates AND'd too tight | Retry with broader predicate or OR composition |
| `no_results_in_corpus` | Query syntactically valid but matched nothing in the corpus | Don't retry the same query; widen or pivot |
| `cap_dropped_all` | Result set exceeded a cap that dropped all rows | Lower the cap arg or refine the query |
| `incremental_no_change` | `index` ran but had nothing to do | Not an error; signal to the user that no re-extraction occurred |
| `all_files_blocked` | Every file at the path is on the blocklist (lockfiles, binaries) | Pass an explicit path to a non-blocked subdirectory |
| `extractor_emitted_nothing` | Extractor ran successfully but emitted zero symbols | Check `health` for parser-tier confidence on the language |

`empty_reason` is the single most important field for getting
non-trivial loops right. An agent that branches on it correctly
will recover from misqueries; an agent that ignores it will loop
on the same failing call indefinitely.

### `baseline_method`

What pincher would have asked the agent to do absent this tool.
Drives the `tokens_saved` calculation. Values: `full_file_read`,
`grep_recursive`, `walk_directory`, `read_all_changed`. The agent
loop usually ignores this field; observability pipelines consume it
to break down savings by replacement pattern.

### `complexity_tier`

`lite` / `standard` / `heavy`. The tier the call belongs to per
pincher's complexity rubric. Multi-agent routers use this to route
cheap calls to cheap models and expensive calls to expensive models
in the same session.

The tier is **declared at tool-registration time** and stable across
releases for a given tool — `search` is always `lite`, `query` is
always `standard`, `architecture` is always `heavy`. The OpenAPI
`x-pincher-tier` extension exposes this for SDK-time inspection.

---

## Fields that drive UI surfaces

### `warnings_v2[]`

Typed warnings for the host UI to surface. Structured shape (v0.71
#1098):

```jsonc
{
  "code": "ambiguous_match",       // stable, machine-actionable
  "severity": "warning",           // "info" | "warning" | "error"
  "message": "human-readable text",
  "data": {                        // typed payload, code-specific
    "candidates": ["...", "..."]
  }
}
```

Severity aligns with MCP `notifications/message` so hosts can pipe
warnings straight through the notification channel when one is
available.

Legacy `warnings: [string]` ships alongside for one release cycle
during v0.71 → v0.74. Consumers can read either; new integrations
should read `warnings_v2`.

### `diagnosis_v2[]`

Same shape as `warnings_v2` but specific to **why a query failed
or returned surprising results**. `warnings_v2` is "something to
flag about this response"; `diagnosis_v2` is "what went wrong
specifically."

Common codes: `unknown_arg` (typo'd parameter — `data.accepted`
lists the valid keys), `cypher_parse_error` (pinchQL didn't
tokenize — `data.position` names the offset), `corpus_mismatch`
(query targets `code` but the corpus is `docs`).

### `capabilities[]`

Stable string tags advertising what features the running server
supports. Hosts gate on these instead of probing each surface.

Always-on tags emitted by every server:

| Tag | Meaning |
|---|---|
| `schema_v33` | The DB schema version (current at the time of this release) |
| `hook_check` | `pincher hook-check` subcommand available |
| `supervised` | `pincher supervised` MCP transport available |
| `operator_tools_on_mcp` | Restricted operator-tier tools surfaced over MCP |
| `session_persistence` | Per-session counters survive supervised respawn |
| `binary_drift_warning` | `_meta.binary_drift_warning` populated when DB schema_version > binary |
| `tokens_used_envelope` | Every response carries `tokens_used` |
| `tokens_saved_pct` | `tokens_saved_pct` is computed honestly per call |
| `complexity_tier` | Per-tool `complexity_tier` is stamped |
| `standardized_error_envelope` | Errors share a single typed envelope shape |
| `idempotency_declared` | OpenAPI `x-pincher-idempotent` extension populated |
| `sse` | `GET /v1/events` SSE endpoint available |
| `metrics_prometheus` | `GET /v1/metrics` Prometheus scrape endpoint available |

Conditional tags (appear only when the corresponding mode is on):

| Tag | When emitted |
|---|---|
| `closure_tables` | `PINCHER_CLOSURE_TABLES=1` set |
| `streamable_http` | Streamable-HTTP MCP transport bound |
| `traces_otlp` | OTLP tracer wired up |
| `http_auth` | `--http-key` / `PINCHER_HTTP_KEY` set |

The agent loop reads capabilities to know which advertised paths
are real on this specific server. Hosts SHOULD NOT hardcode an
assumption that any capability tag is present — read the list and
branch.

Opt-out: `PINCHER_META_CAPABILITIES=off` drops the per-call stamp
for heavy-traffic aggregators that want to fetch the list once
via `GET /v1/capabilities`.

---

## Fields that drive observability

### `request_id`

A ULID stamped on every response. Also exposed in the HTTP layer as
the `X-Request-ID` response header (echoed from any client-supplied
`X-Request-ID` request header — supports end-to-end correlation
across the host, pincher, and any downstream OTLP-instrumented
service).

Pipe this into your log correlation ID and into OTLP span IDs —
when something goes wrong, the request_id is the join key between
host logs, pincher logs, and the trace.

### `latency_ms`

Server-measured. Excludes network. Useful for SLO tracking; the
client's wall-clock measurement is the right number for end-to-end
latency budgets.

### `tokens_used` / `tokens_saved` / `tokens_saved_pct`

Per-call savings accounting. `tokens_used` is the response payload
in tokens. `tokens_saved` is `baseline_method`'s token count minus
`tokens_used`. `tokens_saved_pct` is the percentage savings vs
baseline.

By default (since v0.69), `tokens_used` is computed via a `char/4`
heuristic. For exact `cl100k_base` BPE accounting, set
`PINCHER_TOKEN_ACCOUNTING=exact`. The heuristic is within ±5% of
exact for typical pincher payloads (technical English + code
snippets); use it unless you're publishing externally-visible
numbers.

### OTLP integration

When `traces_otlp` capability is advertised, every tool call emits
an OTLP span with attributes mirroring the `_meta` envelope:
`request_id`, `tokens_used`, `tokens_saved_pct`, `complexity_tier`,
`empty_reason` (when non-empty), `binary_drift_warning` (when
non-null). Host-side tracers can build a per-session leverage view
without parsing payloads.

---

## Drift and staleness fields

### `schema_version`

The server's compiled-in schema version. Constant for the lifetime
of a server process.

### `schema_version_at_index`

The schema version active **when the project was last indexed.**
Per-project. May be older than `schema_version` if the project was
indexed by a prior server and hasn't been re-indexed since.

When `schema_version_at_index < schema_version`:
- Symbol byte-offsets may still be valid (most migrations don't
  re-extract content).
- New columns added by intermediate migrations default to "" / 0
  on the old rows.
- `empty_reason: stale_index` may fire on queries that rely on the
  new columns.

The agent's response: usually noop; suggest re-index to the user if
the staleness leads to a wrong-looking result.

### `binary_drift_warning`

Non-null when the DB has been migrated past what the running binary
understands. Shape:

```jsonc
{
  "stored_version": 34,
  "running_version": 33,
  "remediation": "supervised mode will respawn on next call; if not running supervised, restart pincher"
}
```

Supervised mode (`PINCHER_AUTO_RESTART_ON_DRIFT=1` or `pincher
supervised`) automatically respawns. Non-supervised installs: the
host SHOULD surface this to the user since further calls run against
shape the binary doesn't fully understand.

The `StartSchemaDriftWatcher` background goroutine (#1374, v0.71)
exits the process on drift so supervised mode brings up a fresh
instance — `binary_drift_warning` is the inline signal that a
restart is coming on the next supervised respawn.

---

## Pseudocode — minimal `_meta`-aware host loop

```python
def call_pincher(tool, **args):
    resp = mcp.call(tool, args)
    meta = resp.get("_meta", {})

    # Capability gating (one-time per session is enough)
    if "schema_v33" not in meta.get("capabilities", []):
        log.warn("server schema older than this integration was built for")

    # Drift handling
    if meta.get("binary_drift_warning"):
        notify_user("pincher binary stale; restart pending")

    # Diagnose empty / partial
    er = meta.get("empty_reason")
    if er == "no_project_indexed":
        return call_pincher("index", path=current_dir())
    if er == "query_too_narrow":
        # Look in diagnosis_v2 for the OR-retry suggestion
        ...

    # Surface warnings
    for w in meta.get("warnings_v2", []):
        host_ui.banner(w["code"], w["message"], severity=w["severity"])

    # Schedule next steps
    for step in meta.get("next_steps", []):
        host_loop.enqueue(step["tool"], step["args"])

    return resp["result"]
```

That's the contract in working code. Three branches and a loop —
that's the entire integration on the agent side.

---

## What CANNOT change post-v1.0

Once v1.0 ships, the following are frozen — any change is a major
version bump:

1. **Field NAMES.** `next_steps`, `empty_reason`, `capabilities`,
   `request_id`, etc. The keys themselves.
2. **`empty_reason` enum.** Adding values is allowed (existing
   consumers should default-branch on unknown); removing or
   renaming is a major bump.
3. **`capabilities` always-on tag list.** Removing an always-on
   tag is a major bump. Adding new tags is allowed.
4. **The structured `warnings_v2` / `diagnosis_v2` shape.**
   `{ code, severity, message, data }`. Adding sibling keys to
   the entry object is allowed.
5. **`request_id` semantics.** ULID, stable per response, echoed
   via `X-Request-ID`.
6. **`schema_version` / `schema_version_at_index` semantics.**
   Always integer, always reflects the DB's `schema_version` table.

What is NOT frozen:
- The exact `tokens_used` algorithm (heuristic vs exact is
  configurable).
- The contents of `data` payloads inside `warnings_v2[]` /
  `diagnosis_v2[]` — those are per-code and can evolve.
- The complete list of `capabilities` (new tags land; conditional
  tags vary by server config).

---

## Cross-refs

- [`loop-leverage-layers.md`](loop-leverage-layers.md) — the
  three-layer leverage frame this contract enables
- [`docs/REFERENCE.md`](../REFERENCE.md) — per-tool reference;
  every tool's response shape is documented there
- [`empty_reason.go`](../../internal/server/empty_reason.go) —
  source of truth for the `empty_reason` enum
- [`capability_test.go`](../../internal/server/capability_test.go) —
  runtime probes for every advertised capability tag
- [#1098](https://github.com/kwad77/pincher/issues/1098) — the
  structured-`_meta` umbrella
- [#1163](https://github.com/kwad77/pincher/issues/1163) — OTLP
  trace integration
- [#638](https://github.com/kwad77/pincher/issues/638) — v1.0
  launch checklist; the freeze gate
