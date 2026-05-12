<div align="center">
  <img src="assets/banner.png" alt="pincherMCP — pixel-art mascot Pinchy the crab holding a copper penny, wordmark, and tagline" width="900"/>
</div>

<div align="center">

[![CI](https://github.com/kwad77/pincher/actions/workflows/ci.yml/badge.svg)](https://github.com/kwad77/pincher/actions/workflows/ci.yml)
[![Go 1.25](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/license-MIT-22c55e.svg)](LICENSE)
[![Coverage](https://img.shields.io/badge/coverage-85%25-22c55e.svg)](docs/REFERENCE.md#test-coverage)

**Codebase intelligence server for LLM agents.**
Single binary · No cloud dependencies · Any LLM · MCP stdio or HTTP REST

[What it does](#what-it-does) · [Quick Start](#quick-start) · [Self-healing connections](#self-healing-connections) · [Why it's fast](#why-its-fast) · [Token savings](#token-savings) · [Staying current](#staying-current) · [Roadmap](#roadmap) · [Limitations](#known-limitations)

</div>

---

## What it does

Sourcegraph, OpenGrok, and IntelliJ index a codebase for humans browsing it; pincherMCP indexes the same codebase for an LLM agent calling tools. The agent-shaped surface is the whole point — responses sized for a context window rather than a UI pane, runtime interception of Read and Grep calls before the agent opens the file, and a local-only binary so neither the index nor the code leaves the machine.

Underneath, it is a single Go binary that indexes the codebase into three co-located layers — byte-offset symbol store, knowledge graph, and FTS5 full-text search — and exposes all three through **9 agent-facing MCP tools** plus 13 operator/diagnostic tools on the HTTP REST API.

Every tool response includes a `_meta` envelope with real BPE token counts (cl100k_base — exact for Claude and OpenAI families, approximate for Gemini/Llama) and latency:

```json
{
  "name": "processPayment",
  "source": "func processPayment(amount float64) error { ... }",
  "_meta": {
    "tokens_used":       312,
    "tokens_saved":      14500,
    "tokens_saved_pct":  97.9,
    "latency_ms":        2
  }
}
```

Token savings accumulate in SQLite across sessions — every reconnect adds to a running all-time total. All three indexes are populated in a **single AST parse pass** from one shared `symbols` table; no duplication, no sync overhead.

> **Looking for the manual?** → [`docs/REFERENCE.md`](docs/REFERENCE.md) is the long-form reference: every tool, every flag, every endpoint, schema history, performance numbers, project layout. This README sticks to pitch + quickstart.

---

## Quick Start

```bash
# 1. Install
go install github.com/kwad77/pincher/cmd/pinch@latest      # if Go 1.25+ on PATH
# or download a release binary:
#   https://github.com/kwad77/pincher/releases/latest
# or build from source:
#   git clone https://github.com/kwad77/pincher && cd pincher
#   go build -o pincher ./cmd/pinch/      # or pincher.exe on Windows

# 2. Drop the usage policy into your client's config (one-time, idempotent)
pincher init                             # ./CLAUDE.md (Claude Code, current dir)
pincher init --global                    # ~/.claude/CLAUDE.md (Claude Code, all projects)
pincher init --target=cursor             # .cursor/rules/pincher.mdc
pincher init --target=codex              # ~/.codex/config.toml (writes MCP server block)
pincher init --target=detect             # auto-detect from marker files in cwd

# 3. Index your project
pincher index /path/to/your/project

# 4. Point your MCP client at the binary (Claude Code / Cursor / Codex / Zed below)
#    Or open the dashboard: pincher web
```

### Client configuration

pincher speaks the standard JSON-RPC 2.0 MCP protocol over stdio. The `command` field is the same everywhere — only the file location and key name change.

<details>
<summary><b>Claude Code</b> — <code>~/.claude/mcp.json</code></summary>

```json
{
  "mcpServers": {
    "pincher": { "type": "stdio", "command": "/path/to/pincher", "args": ["supervised"] }
  }
}
```

`args: ["supervised"]` is the v0.11.0 recommended invocation — see [Self-healing connections](#self-healing-connections) below. Drop the `args` to run pincher bare.
</details>

<details>
<summary><b>Cursor</b> — <code>~/.cursor/mcp.json</code></summary>

```json
{
  "mcpServers": {
    "pincher": { "command": "/path/to/pincher", "args": ["supervised"] }
  }
}
```
</details>

<details>
<summary><b>Codex</b> — <code>~/.codex/config.toml</code> (run <code>pincher init --target=codex</code>)</summary>

```toml
[mcp_servers.pincher]
command = "/path/to/pincher"
args = ["supervised"]

[mcp_servers.pincher.env]
PINCHER_DATA_DIR = "/codex-isolated/data/dir"
```

`pincher init --target=codex` writes this block (with a Codex-isolated `PINCHER_DATA_DIR`) wrapped in idempotent markers, so re-running it never duplicates.
</details>

<details>
<summary><b>Zed</b> — <code>settings.json</code> under <code>context_servers</code></summary>

```json
{
  "context_servers": {
    "pincher": {
      "command": { "path": "/path/to/pincher", "args": ["supervised"] }
    }
  }
}
```
</details>

Continue, Windsurf, Aider, and any MCP-compatible client follow the same pattern. For editors without MCP, use the [HTTP REST API](docs/REFERENCE.md#http-rest-api).

For managed installs (Homebrew, systemd, launchd, Windows service, Docker), see [`packaging/README.md`](packaging/README.md).

### Tutorials

End-to-end walkthroughs (~10 min each):

- **[Claude Code](docs/tutorials/claude-code.md)** — install → index → `pincher init` → wire MCP → first query.
- **[Cursor](docs/tutorials/cursor.md)** — same flow with `pincher init --target=cursor` and Cursor's `.mdc` rules format.
- **[HTTP dashboard](docs/tutorials/http-dashboard.md)** — `pincher --http`, dashboard panels, REST API with `curl`, reverse-proxy notes.

---

## Self-healing connections

Binary upgrades (and the rare panic) used to require a manual `/mcp` reconnect. v0.11.0 closes that loop with a thin supervisor process you put in front of the inner pincher MCP server:

```
   MCP client                  Supervisor                      Inner pincher
   (Claude / Codex / Cursor)   (long-lived stdio bridge)       (the actual MCP server)

         stdio  ◄────────────►  captures `initialize` ◄─────►  exits on:
                                replays it on respawn           • binary upgrade (PINCHER_AUTO_RESTART_ON_DRIFT=1)
                                liveness probe + circuit        • probe timeout (process hung)
                                breaker on flaps                • crash / panic / OS kill
```

**Three pieces work together:**

- **`pincher supervised`** — wraps an inner pincher with auto-respawn + initialize-replay so the client's stdio session looks unbroken across inner exits. Pass it as `args: ["supervised"]` in your MCP config (see Client configuration above).
- **`PINCHER_AUTO_RESTART_ON_DRIFT=1`** — opt-in env var that makes the inner exit cleanly when it detects a freshly-built binary on disk (a `go build` while the server is running). Combined with the supervisor, this hot-swaps you onto the new binary on the next tool call without a `/mcp` dance. `pincher init --target=codex` sets this for you.
- **`pincher health-check`** — non-interactive liveness probe (cron / launchd / k8s readinessProbe). Spawns a short-lived MCP client, completes the handshake, runs `tools/list`, exits 0 on success.

```bash
pincher health-check                              # probe self via os.Executable
pincher health-check --supervised                 # probe through `pincher supervised`
pincher health-check --binary /path/to/pincher    # probe a specific binary
```

The supervisor also exposes a `pincher.supervisor.status` MCP tool that returns `{alive, uptime_sec, restarts, probes_sent, probes_answered, probes_timed_out, last_restart_reason, tools_list_changed_emitted, tools_list_changed_emit_failed, last_tools_list_changed_emit_at}` — useful when an agent wants to know why pincher cycled mid-session or confirm the supervisor emitted a `tools/list_changed` notification after a binary swap.

---

## Why it's fast

**Three indexes, one AST pass.** A single `ast.Extract()` call per file populates all three layers. No background sync. No drift between graph and search.

```
   Source File                 ┌───────────────┐    ┌──────────────┐    ┌─────────────────┐
        │                      │  Layer 1      │    │  Layer 2     │    │  Layer 3 — FTS5 │
   ast.Extract()  ─────────►   │  Byte-offset  │    │  Knowledge   │    │  BM25 search    │
        │                      │  symbol store │    │  graph       │    │  per-corpus     │
        ▼                      │  O(1), <1 ms  │    │  <2 ms       │    │  routing        │
   one symbols row             └───────────────┘    └──────────────┘    └─────────────────┘
```

**Per-corpus FTS5.** Source-code identifiers, config keys, and Markdown sections live in three separate BM25 indexes (`symbols_{code,config,docs}_fts`). The `search` tool defaults to `corpus=code` so identifier searches aren't diluted by lockfile keys.

**Per-symbol confidence.** Lockfile keys score ~0.4–0.6, real config ~0.95–1.0. `search` defaults to `min_confidence=0.7` so noise drops out automatically.

**Reader pool.** SQLite WAL gives concurrent readers; pincher uses a separate read-only connection pool (`--db-readers`, capped at 32) so a busy MCP session can't block the writer.

Measured on this codebase (221 files, 3,769 symbols, 5,920 edges): cold index ~900 ms, single-hop pinchQL 2 ms, BFS depth 3 <5 ms, FTS5 search 1 ms. Re-index after small edits (incremental, content-hash skip) is typically <50 ms. Full benchmark + methodology in [REFERENCE.md → Performance](docs/REFERENCE.md#performance).

---

## Token savings

The `stats` tool renders a session summary directly in chat:

```
┌────────────────────────────────────────────┐
│                  SESSION                   │
│  Tool calls:          5                    │
│  Without pincher:   ~45,200 tokens         │
│  With pincher:        1,200 tokens         │
│  Saved:             ~44,000 tokens  (97%)  │
│  Avg latency:         2 ms                 │
└────────────────────────────────────────────┘
```

**Without pincher** is the estimated baseline (whole file reads). **With pincher** is the actual BPE token count of what was returned. Savings persist in SQLite across reconnects, process restarts, and binary upgrades — the dashboard at `/v1/dashboard` shows the all-time total.

Every `_meta` envelope carries `tokens_saved` (absolute) and `tokens_saved_pct` (the same number as a bounded percentage — capped at 100%, can go negative when the response envelope cost more than the savings). The bounded form is easier to reason about per-call than a compounding ratio.

### What to expect per workflow shape

Savings vary by what pincher is actually replacing. The tool breakdown:

| Workflow / tool | Typical saved % | What's happening |
|---|---|---|
| **Reading a function in a large file** — `context`, `symbol`, `symbols` | 95-99% | Byte-offset retrieval skips the rest of the file entirely. The bigger the source file, the larger the absolute saving. |
| **Tracing callers or call-graph traversal** — `trace`, `query` | 80-95% | Returns just the matching paths in one call instead of a multi-step grep-and-Read workflow. |
| **Conceptual / BM25 search** — `search` | 60-90% | Ranked snippets in 2KB beat unranked grep output across many files. On exact-token greps where Grep already returns one line, savings approach zero. |
| **Project orientation** — `architecture`, `health`, `schema`, `list` | reported as `null` | No honest file-read baseline to compare against. Useful but not measured as "saved." |
| **First fetch of an external doc** — `fetch` | ~0% on first call | Real value is persistence + cross-session search re-use. Second-and-Nth access to the same stored Document is 85-90% saved. |

### Best / typical / break-even

- **Large Go (or JS) project, > 500 files, > 50KB average file size** — workflows hit Tier 1 retrieval often. Aggregate session savings commonly land at **70-90%**.
- **Mixed-language mid-size project** (Go + JS + config + docs) — fewer big-file reads, more orientation calls. Expect **40-70%** aggregate.
- **Small project (< 50 files) or one dominated by stub-tier languages** (Scala, Lua, Zig, Elixir, Haskell, Dart, R) — Read/Grep is genuinely competitive on small files. Expect **break-even to ~30%**.

The point of breaking it out per tier is that you can match the tool to the workflow you actually have. If your day is mostly small-file edits in a polyglot repo with thin language support, pincher's value is concentrated in the structural-query and search tools rather than per-call retrieval — and that's the honest framing rather than an aggregate ratio that doesn't apply to your shape.

---

## Staying current

Three subcommands keep pincher fresh and discoverable on the same machine:

```bash
# Auto-update in place — git pull + rebuild from this checkout, or fetch the
# latest GitHub release asset when run from outside the source tree.
./pincher update                  # apply if behind
./pincher update --check          # report status only

# Print the running HTTP dashboard URL; auto-spawn one if none is bound.
./pincher web                     # prints http://localhost:7777/v1/dashboard
./pincher web --json              # {url, base, pid, started_by}

# Inject the pincher usage policy into CLAUDE.md (idempotent — re-runs replace
# the marker block in place, never duplicating).
./pincher init                    # ./CLAUDE.md
./pincher init --global           # ~/.claude/CLAUDE.md
```

Other CLI subcommands ([`pincher index`](docs/REFERENCE.md#pincher-index), [`pincher doctor`](docs/REFERENCE.md#pincher-doctor), [`pincher rebuild-fts`](docs/REFERENCE.md#pincher-rebuild-fts), [`pincher self-test`](docs/REFERENCE.md#pincher-self-test)) and the full HTTP surface live in [REFERENCE.md](docs/REFERENCE.md).

---

## Roadmap

| Release | Theme | Status |
|---|---|---|
| **v0.2 → v0.10** | Index quality at scale, trust + observability, multi-client `pincher init`, HTML/XML extractors, schema migrations through v18, single-binary release pipeline. Per-version notes in [`CHANGELOG.md`](CHANGELOG.md). | ✅ shipped |
| **v0.11.0** | **Self-healing MCP connections.** `pincher supervised` (auto-respawn + initialize-replay), `pincher init --target=codex`, `pincher health-check` CLI, `pincher.supervisor.status` tool, bidirectional binary-version drift detection, single-source build versioning with CI assertion gate. | ✅ shipped |
| **v0.11.1** | Patch — fix in-flight response loss during supervised respawn ([#371](https://github.com/kwad77/pincher/issues/371)). | ✅ shipped |
| **v0.12.0** | **pinchQL parens + dogfood-driven cleanup.** Parenthesized `WHERE` groups + `NOT (...)` ([#362](https://github.com/kwad77/pincher/issues/362)) via a recursive-descent expression tree. Five fixes from a single full-surface dogfood pass: `corpus=docs` no longer floors out Markdown sections by default ([#379](https://github.com/kwad77/pincher/issues/379)); `architecture` hotspots filter non-code kinds ([#380](https://github.com/kwad77/pincher/issues/380)); the watcher walks recursively so subdirectory edits trigger re-index ([#377](https://github.com/kwad77/pincher/issues/377)); `list prune_dead` is orthogonal to `include_dead` ([#378](https://github.com/kwad77/pincher/issues/378)); `context` returns in-file callees alongside imports ([#381](https://github.com/kwad77/pincher/issues/381)). | ✅ shipped |
| **v0.13.0** | **JS AST + tool surface expansion + dogfood-driven precision.** Pure-Go JS AST extractor behind `PINCHER_EXPERIMENTAL_JS_AST=1` ([#266](https://github.com/kwad77/pincher/issues/266)); two new MCP tools — `changes scope=base:<branch>` for pre-PR blast-radius preview ([#394](https://github.com/kwad77/pincher/pull/394)) and `dead_code` for surfacing unreachable internal symbols ([#396](https://github.com/kwad77/pincher/pull/396)); cross-repo pinchQL via `query project=*` ([#395](https://github.com/kwad77/pincher/pull/395)); architecture + trace stop polluting orientation views with `testdata/` fixtures ([#392](https://github.com/kwad77/pincher/issues/392), [#398](https://github.com/kwad77/pincher/issues/398)); supervisor flake hardened ([#383](https://github.com/kwad77/pincher/issues/383)); Windows CI ~50% faster ([#391](https://github.com/kwad77/pincher/pull/391)). Tool count: 18 → 20. | ✅ shipped |
| **v0.14.0** | **Token-savings + performance focus.** Field projection across every read tool ([#400](https://github.com/kwad77/pincher/issues/400)); project-ID resolution cache + reader-pool warming ([#401](https://github.com/kwad77/pincher/issues/401)); adaptive trace depth that auto-trims to the smallest depth with ≥5 hops ([#402](https://github.com/kwad77/pincher/issues/402)); two precision fixes from the post-v0.13 dogfood pass — `changes.changed_files` emits `[]` not `null` on empty diffs ([#408](https://github.com/kwad77/pincher/issues/408)) and the receiver-method call resolver no longer over-binds stdlib calls (`strings.Index(...)` → `*Indexer.Index`) to local methods sharing the leaf name ([#410](https://github.com/kwad77/pincher/issues/410)). | ✅ shipped |
| **v0.15.0** | **Autoresearcher dogfood loop enablers.** Supervised mode pushes `notifications/tools/list_changed` after respawn so clients re-list new tools live ([#407](https://github.com/kwad77/pincher/issues/407)); pinchQL `WHERE n.id="X"` pushes to SQL instead of post-filtering — fixes silent undercounts ([#412](https://github.com/kwad77/pincher/issues/412)); `guide` learns 9 pincher-domain concepts (\"MCP tool\", \"schema migration\", \"language extractor\", …) and routes \"why does X\" to shapeUnderstand ([#397](https://github.com/kwad77/pincher/issues/397)). | ✅ shipped |
| **v0.15.1** | Patch — FTS5 sanitizer hardening for multi-character query operators ([#424](https://github.com/kwad77/pincher/issues/424)). | ✅ shipped |
| **v0.15.2** | Patch — pinchQL OR / paren / NOT trees push to SQL past the LIMIT clamp ([#430](https://github.com/kwad77/pincher/issues/430)); `changes scope=` validates input instead of silently re-interpreting unknown values ([#437](https://github.com/kwad77/pincher/issues/437)). | ✅ shipped |
| **v0.15.3** | Patch — pinchQL comparison operators (`>`, `<`, `>=`, `<=`, `<>`) push to SQL ([#434](https://github.com/kwad77/pincher/issues/434)); third silent-undercount fix in the pushdown gate. | ✅ shipped |
| **v0.15.4** | Patch — five pinchQL fixes from the v0.15.0 dogfood loop: bool column coercion ([#421](https://github.com/kwad77/pincher/issues/421)), in-Go filter LIMIT clamp scaling ([#435](https://github.com/kwad77/pincher/issues/435)), naked bool predicate support + helpful operator error ([#431](https://github.com/kwad77/pincher/issues/431)), `AVG`/`MIN`/`MAX`/`SUM` aggregations ([#432](https://github.com/kwad77/pincher/issues/432)), explicit rejection of `WITH` and chained-edge patterns ([#433](https://github.com/kwad77/pincher/issues/433)). | ✅ shipped |
| **v0.15.5** | Patch — indexer `READS` / `WRITES` name lookups now scope to source language, eliminating ~8% cross-language false-positive edges on mixed corpora ([#436](https://github.com/kwad77/pincher/issues/436)). Re-index recommended on upgrade. | ✅ shipped |
| **v0.15.6** | Patch — dogfood-driven hygiene haul. `binary_stale_message` UX ([#449](https://github.com/kwad77/pincher/issues/449)); IMPORTS edge dedup via deterministic Module lookup ([#428](https://github.com/kwad77/pincher/issues/428)); search AND→OR fallback on 0-result multi-token queries ([#453](https://github.com/kwad77/pincher/issues/453)); preservation of explicit FTS5 operators in identifier queries ([#452](https://github.com/kwad77/pincher/issues/452)); watcher one-hop referencer invalidation ([#427](https://github.com/kwad77/pincher/issues/427), partial — full fix tracked in [#457](https://github.com/kwad77/pincher/issues/457)); `changes scope=unstaged` no longer includes untracked ([#422](https://github.com/kwad77/pincher/issues/422)); `list min_edges=1` default hides empty-graph worktree noise ([#419](https://github.com/kwad77/pincher/issues/419)). | ✅ shipped |
| **v0.16.0** | **Structural perf + dogfood haul.** Schema v19 `pending_edges` table — persisted per-file deferred edges close #427's transitive watcher edge-loss ([#457](https://github.com/kwad77/pincher/issues/457)); pinchQL exposes `docstring` / `signature` / `return_type` / `is_test` so the canonical "find undocumented exported APIs" query works ([#438](https://github.com/kwad77/pincher/issues/438)); BFS planner inverts walk when only the end predicate is selective — 10s timeout → milliseconds ([#426](https://github.com/kwad77/pincher/issues/426)); polymorphic-method-name blocklist drops false-positive `.String()` / `.Error()` resolutions ([#465](https://github.com/kwad77/pincher/issues/465)); supervisor.status surfaces `tools/list_changed` delivery counters ([#429](https://github.com/kwad77/pincher/issues/429)); session counters survive supervised respawn ([#420](https://github.com/kwad77/pincher/issues/420)); `guide` routes structural-audit tasks to pinchQL `query` ([#467](https://github.com/kwad77/pincher/issues/467)); `index` diagnosis splits three zero-symbol cases ([#425](https://github.com/kwad77/pincher/issues/425)). | ✅ shipped |
| **v0.17.0** | **Honest savings + failure-as-pedagogy.** Tokens-saved counter dedup'd per-session and de-fabricated (real file-path baseline, no `count × avgFileSize` heuristic) ([#476](https://github.com/kwad77/pincher/issues/476), [#478](https://github.com/kwad77/pincher/issues/478), [#479](https://github.com/kwad77/pincher/issues/479)); `cost_avoided` $-figures dropped from every surface (we don't know the user's model or pricing); `trace` gains an `id` arg as the disambiguation escape hatch the hint promised ([#474](https://github.com/kwad77/pincher/issues/474)); pinchQL surfaces unknown-property warnings instead of silently returning 0 rows ([#473](https://github.com/kwad77/pincher/issues/473)). | ✅ shipped |
| **v0.18.0** | **Failure-as-pedagogy v2 + dopamine + tool-output trust.** Schema v20→v21 — `edges.source` tag for atomic resolve-pass replace ([#475](https://github.com/kwad77/pincher/issues/475)) plus the new `celebrations` table for tier-milestone one-shots. The pinchQL pedagogy from v0.17 (#473) extended across the entire tool surface: unknown args surface in `_meta.warnings` ([#499](https://github.com/kwad77/pincher/issues/499)); enum-shaped property values + MATCH-pattern label typos ([#501](https://github.com/kwad77/pincher/issues/501)); search regex meta-patterns redirected to `query` instead of leaking SQL ([#509](https://github.com/kwad77/pincher/issues/509)). Plus dopamine: occasional `_meta.celebration` on cumulative-tokens-saved milestones, exactly once per tier per installation ([#494](https://github.com/kwad77/pincher/issues/494)). Plus tool-output trust: `dead_code` filters Go runtime-invoked symbols (init/TestMain/main) so it stops crying wolf ([#492](https://github.com/kwad77/pincher/issues/492)); `changes` intersects diff hunks with symbol line ranges so a 3-function PR no longer balloons to 240 changed_symbols and a 345 KB payload ([#502](https://github.com/kwad77/pincher/issues/502)); `neighborhood` description leads with "NOT graph adjacency" so agents stop reaching for it expecting `trace` semantics ([#498](https://github.com/kwad77/pincher/issues/498)); `list filtered_out` splits per reason with diagnosis hints ([#505](https://github.com/kwad77/pincher/issues/505)); `guide` recognizes "audit tool X" as an empirical investigation ([#497](https://github.com/kwad77/pincher/issues/497)). | ✅ shipped |
| **v0.19.0** | **Receiver-type tracking + savings honesty.** Schema v22. Go receiver-type tracking — `recv.field.method` calls now resolve precisely via the new `struct_fields` table + `pending_edges.receiver_type` column; the polymorphic-method blocklist (Close/String/Run) no longer over-drops calls when receiver type is known ([#423](https://github.com/kwad77/pincher/issues/423), four-piece stack: #514/#517/#518). `_meta.baseline_method` on every tool response — `"full_file_read"` / `"partial_read"` / `"none"` — admin tools emit `tokens_saved: null` instead of fabricating zeros ([#477](https://github.com/kwad77/pincher/issues/477)). | ✅ shipped |
| **v0.20.0** | **JS AST default-on + interface-dispatch dead_code precision + parity foundation.** Schema v23. Pure-Go JS AST (#266) promoted from `PINCHER_EXPERIMENTAL_JS_AST=1` to default-on, with four polish bugs fixed (arrow→Function promotion, IsExported semantics, object-literal arrows, const-object descent). Interface-dispatch satisfaction analysis closes [#493](https://github.com/kwad77/pincher/issues/493) — Methods reachable only via interface dispatch stop showing as dead_code (third leg of the dead_code FP triangle, after #423 and #492). `openAPISpec` now derives from `s.handlers` with a parity gate test — newly-added MCP tools can't silently disappear from `/v1/openapi.json` ([#558](https://github.com/kwad77/pincher/issues/558) phase 1). | ✅ shipped |
| **v0.21.0** | **Dead_code FP triangle's last leg + parity finish-line.** Function values bound to struct fields no longer false-flag the bound function as dead — binding-pass emits low-confidence (0.4) CALLS edges from `s.handler = fn` patterns ([#565](https://github.com/kwad77/pincher/issues/565)). Build-tag duplicate-implementation siblings (`web_windows.go` / `web_unix.go` pattern) all surface as inbound-reachable instead of just the lex-smallest variant — fixes cross-platform Go `dead_code` reports ([#566](https://github.com/kwad77/pincher/issues/566)). `Run` added to polymorphic-method blocklist — `cmd.Run()` (`*exec.Cmd`) and `srv.Run()` (`*http.Server`) stop false-binding to in-project Methods named `Run` ([#567](https://github.com/kwad77/pincher/issues/567)). Plus [#558](https://github.com/kwad77/pincher/issues/558) phases 2+3: `doctor` / `rebuild_fts` / `self_test` graduate from CLI-only to MCP+HTTP via the dynamic `/v1/<tool>` dispatcher; CLI↔MCP parity gate prevents future user-facing CLI commands from being silently CLI-only. Tool count: 21 → 24. | ✅ shipped |
| **v0.22.0** | **Dogfood haul + OpenAPI contracts.** Five fixes and one feature, all from a single ~3-hour dogfood probe of v0.21. OpenAPI response schemas with shared `_meta`/`Error` components ([#581](https://github.com/kwad77/pincher/issues/581)) — generated SDKs from `/v1/openapi.json` get typed models per endpoint. Plus four precision fixes: file-scope composite-literal binding ([#576](https://github.com/kwad77/pincher/issues/576), retroactively makes the v0.21 README claim true), fetch corrupts markdown ([#579](https://github.com/kwad77/pincher/issues/579)), doctor handler caps projects/failures globally ([#575](https://github.com/kwad77/pincher/issues/575)), pinchQL rejects unknown function calls ([#578](https://github.com/kwad77/pincher/issues/578)). | ✅ shipped |
| **v0.23.0** | **HTTP gateway hardening + pinchQL data integrity.** Four fixes from a continuous v0.22 dogfood probe of the HTTP gateway and pinchQL deep queries. Container orchestrators can liveness-probe pincher behind `--http-key` ([#588](https://github.com/kwad77/pincher/issues/588)); bare URL routes to dashboard ([#590](https://github.com/kwad77/pincher/issues/590)); pinchQL stops silently inflating result sets via multi-sourced edges ([#591](https://github.com/kwad77/pincher/issues/591)) and column-vs-column comparisons ([#593](https://github.com/kwad77/pincher/issues/593)). | ✅ shipped |
| **v0.24.0** | **Dashboard test foundation.** Four test additions closing umbrella [#519](https://github.com/kwad77/pincher/issues/519)'s "no test coverage" gap: CSS regression snapshot ([#522](https://github.com/kwad77/pincher/issues/522)), JS basepath substitution edge cases + HTML/JS prefix-agreement gate ([#523](https://github.com/kwad77/pincher/issues/523)), 11 per-endpoint shape + negative tests for the ad-hoc `/v1/` routes ([#528](https://github.com/kwad77/pincher/issues/528)), large-dataset fixture (1k projects + 1k sessions + 5k symbols) with per-endpoint wallclock + payload-size guards ([#527](https://github.com/kwad77/pincher/issues/527)). `renderDashboard*` now normalize trailing slashes off basepaths so reverse-proxy `/pincher/` no longer produces `/pincher//v1/...` fetch URLs. | ✅ shipped |
| **v0.25.0** | **Dashboard API hardening.** Pagination on `/v1/projects` (default 50, max 200) ([#530](https://github.com/kwad77/pincher/issues/530)), `/v1/sessions` (default 90, max 500) ([#531](https://github.com/kwad77/pincher/issues/531)), and `search` (default 20, max 500, offset up to 5000) ([#532](https://github.com/kwad77/pincher/issues/532)) — every paginated endpoint returns `{rows, total, has_more}`. `/v1/index-progress` adds `started_at` + `elapsed_ms` + `files_per_sec` + `eta_ms` ([#535](https://github.com/kwad77/pincher/issues/535)). `/v1/health` adds `dashboard_version` so the JS can detect stale-cache after a server upgrade ([#536](https://github.com/kwad77/pincher/issues/536)). **BREAKING:** every 4xx/5xx response now uses the standardized `{error: {code, message, details?}}` envelope instead of `{error: <string>}` — generated SDKs against `/v1/openapi.json` need a regen ([#537](https://github.com/kwad77/pincher/issues/537)). Part of [#519](https://github.com/kwad77/pincher/issues/519). | ✅ shipped |
| **v0.26.0** | **Dashboard reliability.** ADR field length limits + live counter ([#534](https://github.com/kwad77/pincher/issues/534)). Per-tab error state — failed fetches surface in the tab body with a Retry button instead of leaving "loading…" forever ([#538](https://github.com/kwad77/pincher/issues/538), closes #526). XHR abort on tab switch — `AbortController`-backed `tabFetch` cancels in-flight requests on tab switch so rapid clicks can't race stale responses onto the wrong tab ([#539](https://github.com/kwad77/pincher/issues/539)). Part of [#519](https://github.com/kwad77/pincher/issues/519). | ✅ shipped |
| **v0.27.0** | **Dashboard search polish.** Search-as-you-type with 200ms debounce ([#547](https://github.com/kwad77/pincher/issues/547)). In-snippet match highlighting via `<mark>` — XSS-safe escape-then-wrap ([#548](https://github.com/kwad77/pincher/issues/548)). Sparkline per-point tooltip with mouse + touch + viewport-clip handling ([#555](https://github.com/kwad77/pincher/issues/555)). Architecture detail "Show all" toggle — pre-fix entry-points + hotspots silently truncated at 8/10 ([#533](https://github.com/kwad77/pincher/issues/533)). Part of [#519](https://github.com/kwad77/pincher/issues/519). | ✅ shipped |
| **v0.28.0** | **Dashboard auto-refresh polish.** Projection banner guards against insufficient data — 7-day floor + NaN/Infinity protection + 100M-tokens/mo cap ([#544](https://github.com/kwad77/pincher/issues/544)). Freshness indicator + visibility-aware `pollManager` — pollers pause when the tab is hidden, resume + immediate-refresh on visible ([#545](https://github.com/kwad77/pincher/issues/545), [#546](https://github.com/kwad77/pincher/issues/546)). Three-state theme toggle — auto (system query) / light / dark, persisted in localStorage with no first-paint dark-flash ([#549](https://github.com/kwad77/pincher/issues/549)). Part of [#519](https://github.com/kwad77/pincher/issues/519). | ✅ shipped |
| **v0.29.0** | **Dashboard interactive polish.** Empty-state CTAs replacing bare "No X" text ([#540](https://github.com/kwad77/pincher/issues/540)). Loading skeletons with pulse animation ([#541](https://github.com/kwad77/pincher/issues/541)). Toast variants (success/error/info) + ARIA live region ([#542](https://github.com/kwad77/pincher/issues/542)). Custom confirm dialog with focus management — replaces native `window.confirm()` at all three sites ([#543](https://github.com/kwad77/pincher/issues/543)). Configurable refresh interval (5s/30s/1m/5m/off), persisted; composes with v0.28's visibility-aware `pollManager` ([#552](https://github.com/kwad77/pincher/issues/552)). ADR values render in `<pre>` with pre-wrap for multi-line content ([#553](https://github.com/kwad77/pincher/issues/553)). Part of [#519](https://github.com/kwad77/pincher/issues/519). | ✅ shipped |
| **v0.30.0** | **Dashboard E2E essentials + #519 umbrella close.** Closes umbrella [#519](https://github.com/kwad77/pincher/issues/519) after a 10-release march (v0.21→v0.30, all in one continuous session): keyboard shortcuts (`/`, `g s/p/o/a/h`, `j`/`k`, `Esc`) ([#550](https://github.com/kwad77/pincher/issues/550)); CSV/JSON export buttons on Projects + Sessions ([#551](https://github.com/kwad77/pincher/issues/551)); deep links — `#projects/<projectID>` opens the detail panel directly ([#554](https://github.com/kwad77/pincher/issues/554)); ETag short-circuit on dashboard.js/css for 304 cache validation, gzip already in place via the transparent dispatcher middleware ([#556](https://github.com/kwad77/pincher/issues/556)). E2E harness ([#520](https://github.com/kwad77/pincher/issues/520) + [#524](https://github.com/kwad77/pincher/issues/524) + [#525](https://github.com/kwad77/pincher/issues/525)) deferred — needs Node toolchain in CI; non-runtime gates (snapshot + 23 contract tests across v0.24-v0.30) cover most of that surface. Final scoreboard: 28 issues closed, 3 deferred with rationale. | ✅ shipped |
| **v0.31.0** | **Autoresearcher haul: dead code + NULL pinchQL + redact tests + audit-shape + HTTP method semantics.** Five issues filed by an autoresearcher dogfood probe of v0.30, fixed in one batch. `pinchQL n.docstring=""` and `n.is_test=false` now match NULL rows — the canonical "find undocumented APIs" demo silently returned 0 because of SQL tri-state ([#606](https://github.com/kwad77/pincher/issues/606)). `guide` routes "find every X without Y" phrasing to `query` instead of `search` — restores the #438 demo across arbitrary nouns ([#608](https://github.com/kwad77/pincher/issues/608)). POST/PUT/DELETE on a known GET-only endpoint now returns 405 with `Allow: GET, HEAD` instead of a misleading "unknown tool" 404; HEAD support added everywhere per RFC 7231 ([#609](https://github.com/kwad77/pincher/issues/609)). `redactSensitiveSlice` recursion path covered — was 0%, now 100% ([#607](https://github.com/kwad77/pincher/issues/607)). `dedupCSymbolsByQN` removed as dead helper ([#605](https://github.com/kwad77/pincher/issues/605)). | ✅ shipped |
| **v0.32.0** | **Loop-2 dogfood haul: 0%-coverage gates + edge-property warning pedagogy.** Three issues filed by autoresearcher round 2 against v0.31. pinchQL edge-property warnings (`r.source` etc.) now list edge properties (`kind`, `confidence`) instead of misleading users with the symbol property list — same pedagogy spirit as #473/#499/#501 ([#612](https://github.com/kwad77/pincher/issues/612)). pinchQL `notExpr.eval` Go-side fallback covered (was 0%) — six tests pin the contract against silent NOT inversion ([#611](https://github.com/kwad77/pincher/issues/611)). `db.CurrentSchemaVersion` covered (was 0%) — pins `len(schemaMigrations)+1` and asserts equality with the freshly-opened DB's `schema_version` row, catching off-by-one and migration-skip regressions ([#613](https://github.com/kwad77/pincher/issues/613)). | ✅ shipped |
| **v0.33.0** | **Loop-3 dogfood haul: guide methodology routing + fetch JS-render warning.** Three usability issues filed by autoresearcher round 3 against v0.32. `guide` methodology questions ("how do I find what calls a private function") stop extracting category nouns as the hint — visibility/category nouns are now stop words ([#615](https://github.com/kwad77/pincher/issues/615)). `guide` "use pinchQL to ..." routes to the `query` tool with a starter template instead of pointing at pinchQL's source code ([#616](https://github.com/kwad77/pincher/issues/616)). `fetch` warns via `_meta.warnings` when the extracted text is suspiciously small relative to raw bytes — JS-rendered SPA shells no longer silently return empty text disguised as a successful fetch ([#617](https://github.com/kwad77/pincher/issues/617)). | ✅ shipped |
| **v0.34.0** | **Measurement honesty: bounded percentages + structured fields + per-tier README claims.** New `tokens_saved_pct` field on every `_meta` envelope alongside the existing `tokens_saved` count — bounded form is easier to reason about per-call than compounding ratios ([#619](https://github.com/kwad77/pincher/issues/619)). `binary_version_warning` now surfaces once per (project, indexed-version) pair per server process rather than once per response — repeated identical warnings trained agents to filter `_meta` entirely ([#620](https://github.com/kwad77/pincher/issues/620)). README savings section repositions around per-tier percentages — symbol retrieval, structural traversal, BM25 search, orientation, persistence — so expected savings can be matched to workflow shape ([#621](https://github.com/kwad77/pincher/issues/621)). Removed: `_meta.savings` prose string (redundant with the structured fields). | ✅ shipped |
| **v0.35.0** | **Envelope discipline + MCP surface split.** Pedagogy `next_steps` no longer ride on every successful response (kept on empty/ambiguous results and same-tool pagination); `verbose=true` opts back into the full envelope on any tool ([#622](https://github.com/kwad77/pincher/issues/622)). New `context lite=true` mode returns source-only minimum-envelope shape — used by the v0.36 PreToolUse hook redirect to land on Read-equivalent bytes with byte-offset precision ([#623](https://github.com/kwad77/pincher/issues/623)). MCP-visible tool surface narrows from 22 to 9 (`search`, `symbol`, `symbols`, `context`, `trace`, `query`, `guide`, `changes`, `fetch`); operator/diagnostic tools (architecture, health, schema, list, index, adr, neighborhood, stats, doctor, rebuild_fts, self_test, dead_code) remain reachable via `POST /v1/<tool>` for monitoring dashboards ([#624](https://github.com/kwad77/pincher/issues/624)). | ✅ shipped |
| **v0.36.0** | **Hook foundation: PreToolUse interception + telemetry.** New `pincher hook-check` subcommand reads Claude Code PreToolUse JSON from stdin, returns hook-spec response on stdout. Read on a large indexed file redirects to `context id=<best> lite=true` ([#625](https://github.com/kwad77/pincher/issues/625)); Grep with single-identifier patterns redirects to `search` ([#630](https://github.com/kwad77/pincher/issues/630)). `pincher init --target=claude` writes the `.claude/settings.json` PreToolUse entry alongside the existing MCP wiring — one install wires both ([#627](https://github.com/kwad77/pincher/issues/627)). Schema v24 `hook_invocations` table logs every decision; post-hoc joiner sets `took_recommendation` once the agent's next 3 tool calls are observable. Conversion rate (taken / redirects) ships as the v0.37 dashboard headline ([#626](https://github.com/kwad77/pincher/issues/626)). | ✅ shipped |
| **v0.37.0** | **Hook conversion-rate dashboard.** New `GET /v1/hook-stats` endpoint returns trailing-7d redirects / taken / conversion_pct from the v0.36 `hook_invocations` table; dashboard renders the headline "Read/Grep → pincher (7d)" panel with onboarding hint when no intercepts exist yet ([#628](https://github.com/kwad77/pincher/issues/628)). Two triangulating cards beneath the headline — override rate isolates "agent saw the hint and rejected" from "no signal yet"; per-tool breakdown reports Read vs Grep separately so an imbalance flags which decision tier needs rebalancing ([#629](https://github.com/kwad77/pincher/issues/629), partial — three remaining panels carved out to [#635](https://github.com/kwad77/pincher/issues/635)). | ✅ shipped |
| **v0.38.0** | **Polyglot install warning.** `pincher init` walks the target directory (via gocodewalker, capped at 5000 files) and prints a per-language extraction-tier profile before printing the next-steps recipe — four tiers (AST / stable-regex / approx-regex / stub) map to the v0.34 README savings vocabulary, headline picks majority-by-file-count and emits a typical-session band ([#631](https://github.com/kwad77/pincher/issues/631)). `--quiet` suppresses for CI/scripted installs while still running the wiring. | ✅ shipped |
| **v0.50.0** | **Maturity consolidation + README differentiator.** Version-number truthing: 18 minor releases shipped in ~24h (v0.21 → v0.38) with sustained 85.2% coverage and a frozen tool surface; v0.50 brings the version number into line with where the codebase actually sits. README "What it does" now leads with the Sourcegraph/OpenGrok/IntelliJ contrast paragraph ([#641](https://github.com/kwad77/pincher/pull/641)). No new functionality past v0.38. | ✅ shipped |
| **v1.0** | Tool schemas frozen, schema attestation, migration guide, public launch. Tracking: [#638](https://github.com/kwad77/pincher/issues/638). | planned |

Live milestone burndown: <https://github.com/kwad77/pincher/milestones>. Full punch lists per release: [#193](https://github.com/kwad77/pincher/issues/193).

---

## Known limitations

- **Sequence-rename ID instability in YAML / JSON arrays.** Inserting an item at index 0 of a YAML sequence renames every downstream symbol's qualified name (`tasks.0` → `tasks.1`). Move detection catches some cases but not deterministically. Decided as won't-fix in v0.7.0 ([#205](https://github.com/kwad77/pincher/issues/205)) — the blast radius is mostly Ansible/k8s manifests which are searched via `corpus=config` BM25 anyway, where qualified-name churn is invisible. For long-lived stored references, prefer searching by name over storing the id. Full rationale in [REFERENCE.md → Known limitations](docs/REFERENCE.md#known-limitations).
- **Single-user SQLite.** Cross-process indexing is safe (filesystem lockfile). Team / enterprise shared indexes need a server mode — explicitly out of v1.0 scope.
- **~7 languages without extractors.** Scala, Lua, Zig, Elixir, Haskell, Dart, R are detected as source but emit zero symbols. Adding any of them = implement one Go interface.
- **In-flight response loss during supervised binary upgrade ([#371](https://github.com/kwad77/pincher/issues/371)).** Affected v0.11.0 specifically — the first non-`health` tool call that fired on the freshly-upgraded binary lost its response; client reported `MCP error -32000`. Fixed in v0.11.1 (server-side defer + supervisor sentinel-id init replay). Upgrade to v0.11.1 or later.
- **`notifications/tools/list_changed` requires client support ([#429](https://github.com/kwad77/pincher/issues/429)).** Supervised mode emits the notification after every respawn — confirmable via `pincher.supervisor.status` (the `tools_list_changed_emitted` counter increments per emit). MCP clients that honour the notification (Cursor, Codex, Zed) re-issue `tools/list` and pick up newly-added tools live. Claude Code (as of this writing) does not honour the notification — after a binary swap that adds tools, a fresh session is still required to surface them in that client. Existing tools remain callable in-session via the auto-restart path; only new-tool *discovery* is affected.
- **Pre-v0.17 polymorphic-method CALLS edges persist after upgrade ([#475](https://github.com/kwad77/pincher/issues/475)).** v0.16.0 stopped new bare-name `String` / `Error` / `Read` method resolution from creating false-positive edges; v0.17.0 added the schema v20 atomic project-wide resolve-pass edge replace so future rule changes converge automatically. Existing DBs indexed under v0.16.0 or earlier still need a one-time `pincher index <path> --force` to re-extract symbols + edges from scratch. New indexes converge thereafter.
- **Field-method calls now resolve via receiver-type tracking ([#423](https://github.com/kwad77/pincher/issues/423), shipped v0.19.0).** Pre-v0.19, `dead_code` false-positived methods called via struct fields (`s.cache.Close()`, `s.idx.Watch()`) because the resolver couldn't tell what type the field was. v0.19 schema v22 adds a `struct_fields` table; the resolver consults it before the polymorphic-method blocklist drops the call. Same-package only — qualified types (`io.Writer`, `*foo.Bar`) still need import-graph awareness, deferred. Recommend `pincher index <path> --force` after upgrading to v0.19 so existing DBs pick up the v22 struct_fields rows.
- **Interface-dispatch dead_code false positives — closed in v0.20.0 ([#493](https://github.com/kwad77/pincher/issues/493)).** Schema v23 `interface_methods` table populates from each Interface symbol's declared method-name set; the dead_code SQL excludes Methods whose name matches any interface method in the same project. Cypher engine's `whereExpr.eval` family (the canonical repro) stops surfacing. Cheap heuristic — over-includes (a Method named `String` gets spared even when no project interface uses it) but the dead_code direction prefers false-negatives over false-positives.
- **Function-value-as-field dead_code false positives — fully closed across v0.21+v0.22 ([#565](https://github.com/kwad77/pincher/issues/565), [#576](https://github.com/kwad77/pincher/issues/576)).** Closes the third leg of the dead_code FP triangle after #423 (receiver-type field-method, v0.19) and #493 (interface dispatch, v0.20). v0.21 covered assignment-statement bindings (`s.handler = fn`) via a new low-confidence (0.4) CALLS edge from the surrounding function. v0.22 (#576) extended the binding pass to file-scope composite-literal bindings (`var X = T{Field: fn}` — the canonical "registry of handlers" pattern) via a new `extractGoFileLevelReads` walker; pincher's own `var CodexTarget = Target{DetectFn: detectCodex, …}` exposed the gap during v0.21 dogfood. Build-tag duplicate-implementation siblings (`web_windows.go` / `web_unix.go` pattern) also fan out — pre-fix only the lex-smallest variant got the inbound edge ([#566](https://github.com/kwad77/pincher/issues/566)). Recommend `pincher index <path> --force` after upgrading to v0.22 so existing DBs pick up the new edges.

Full known-limitations list, with severity and tracking issue: [REFERENCE.md → Known Limitations](docs/REFERENCE.md#known-limitations).

---

## License

MIT

<div align="center">
  <img src="docs/assets/crab.png" width="32" alt="Pinchy"/>
</div>
