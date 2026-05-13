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

Underneath, it is a single Go binary that indexes the codebase into three co-located layers — byte-offset symbol store, knowledge graph, and FTS5 full-text search — and exposes all three through **22 agent-callable MCP tools**, every one also reachable via the HTTP REST API at `/v1/<tool>`.

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

## Indexing & staleness

Four mechanisms keep the index current. Each handles a different staleness source — knowing which is which makes it obvious when (and whether) to call `index` manually.

| Mechanism | Triggered by | Cadence | What it does |
|---|---|---|---|
| **Initial index** | `pincher index` (CLI), `index` MCP tool, or first-time `pincher init` follow-up | Once per project | Walks every file, parses, populates symbols + edges + FTS in one AST pass |
| **Watcher** | `pincher` server starts a per-project watcher loop | Polls 2s active / 30s idle | xxh3-hashes every file; re-extracts on hash change. Per-file goroutine; tail-pass GC removes orphans |
| **Cross-file resolution** | After per-file edits, project-wide resolve passes run | On every batch flush | Resolves Go IMPORTS / CALLS / READS edges that span files; uses the v0.16 `pending_edges` table so transitive edges aren't lost |
| **SessionStart hook** | Claude Code SessionStart → `pincher index --hook` | Per session start | Catches anything the watcher missed since last shutdown |

### When to call `index` manually

The watcher covers steady-state usage. Three cases need the explicit lever:

1. **Fresh repo / first-time setup.** Nothing has indexed this project yet. The agent calls `index` (or you run `pincher index` from the CLI) before any other tool returns useful results.
2. **Binary-version drift.** When `pincher` upgrades, the new binary's resolver rules may differ from whatever indexed the project — `_meta.binary_version_warning` and the dashboard surface this. The remedy is `index force=true`, which re-runs every file under the new rules.
3. **In-session race the watcher hasn't ticked yet.** You edit a file, immediately want to query it, and the 2s watcher tick hasn't fired. `index` (incremental — only the changed file re-parses) closes the gap immediately.

The MCP `index` tool was operator-only in v0.35 and restored to the agent-facing surface in v0.51 (after a real user hit "unknown tool 'index'" trying to do exactly case 1 above). Both the MCP tool and the CLI subcommand call the same code path.

### Indicators that the index is stale

- `_meta.binary_version_warning` on any tool response → upgrade-driven drift (case 2)
- `_meta.warnings` containing `index_stale` or similar → watcher missed something
- `health` output `index_drift: true` + `index_drift_message` → upgrade-driven drift again
- Search results that "feel wrong" relative to a recent edit → in-session race (case 3)

In all four cases the recovery is one tool call: `index force=true` for upgrade-driven drift, plain `index` for the rest.

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
| **v0.11 → v0.15** | **Self-healing MCP + JS AST + autoresearcher loop enablers.** `pincher supervised` auto-respawn + initialize-replay (#371 patch in v0.11.1). Pure-Go JS AST behind opt-in flag (#266). Two new MCP tools — `changes scope=base:<branch>` (#394) and `dead_code` (#396). Cross-repo pinchQL via `query project=*` (#395). Adaptive trace depth (#402). Field projection across every read tool (#400). Six dogfood-driven patches in the v0.15.x line — pinchQL pushdown completeness (#412/#430/#434), FTS5 sanitizer (#424), language-scoped READS/WRITES name lookups eliminating ~8% cross-language false-positive edges (#436, re-index recommended). Tool count: 18 → 20. | ✅ shipped |
| **v0.16 → v0.19** | **Structural perf + failure-as-pedagogy + receiver-type precision.** Schema v19 `pending_edges` closes the transitive watcher edge-loss (#427/#457). Schema v20–v22 — atomic resolve-pass edge replace (#475), `celebrations` (#494), Go receiver-type tracking via `struct_fields` table (#423 four-piece stack). Honest savings: real file-path baseline (#476/#478/#479), `_meta.baseline_method` on every response (#477), `cost_avoided` $-figures dropped. pinchQL pedagogy across the whole tool surface — unknown property warnings (#473), unknown args in `_meta.warnings` (#499), enum-shaped value typos (#501). `changes` intersects diff hunks with symbol line ranges (3-function PR no longer balloons to 240 changed_symbols, #502). | ✅ shipped |
| **v0.20 → v0.23** | **Dead_code FP triangle close + parity finish-line + HTTP/pinchQL hardening.** Three legs of the dead_code false-positive triangle closed across four releases — receiver-type field-method calls (#423/v0.19), interface dispatch (#493/v0.20), function-value-as-field (#565/#576/v0.21+v0.22). JS AST default-on (#266). CLI↔MCP parity gate (#558). HTTP gateway + pinchQL deep-query precision fixes (v0.22 / v0.23). Tool count: 21 → 24. | ✅ shipped |
| **v0.24 → v0.30** | **Dashboard hardening — umbrella [#519](https://github.com/kwad77/pincher/issues/519).** Seven-release march (one continuous session) that took the dashboard from "untested HTML emit" to API-paginated, theme-toggleable, keyboard-shortcut-driven, with 23 contract tests + per-endpoint payload guards. Pagination + standardized `{error}` envelope (BREAKING in v0.25, #537). Per-tab error state, AbortController XHR cancellation, search-as-you-type, sparkline tooltips, three-state theme, empty-state CTAs, custom confirm dialogs, CSV/JSON export, deep links, ETag/304. Final scoreboard: 28 issues closed, 3 deferred (E2E harness needs Node toolchain). | ✅ shipped |
| **v0.31 → v0.33** | **Autoresearcher dogfood loops 1–3.** Three sequential probe → file → fix → ship rounds drove 11 issues to closure in one session. Highlights: pinchQL NULL match for `n.docstring=""` (#606), `guide` "find every X without Y" routing (#608), HTTP 405 + RFC 7231 HEAD (#609), pinchQL edge-property warning pedagogy (#612), `fetch` JS-render warning when extracted text is suspiciously small (#617). | ✅ shipped |
| **v0.34 → v0.38** | **Measurement honesty + envelope discipline + hook foundation.** Bounded `tokens_saved_pct` (#619) + per-tier README savings vocabulary (#621). Pedagogy `next_steps` off by default; `verbose=true` opts back in (#622). PreToolUse hook foundation — `pincher hook-check` redirects Read on large indexed files to `context lite=true` and Grep on identifier-shaped patterns to `search` (#625, #630). Conversion-rate dashboard (#628). Polyglot install warning at `pincher init` time (#631). Schema v24 `hook_invocations` table. | ✅ shipped |
| **v0.50.0** | **Maturity consolidation + README differentiator.** Version-number truthing: 18 minor releases in ~24h (v0.21 → v0.38) at sustained 85.2% coverage and a frozen tool surface — v0.50 brings the version number into line with where the codebase sits. README leads with the Sourcegraph/OpenGrok/IntelliJ contrast paragraph ([#641](https://github.com/kwad77/pincher/pull/641)). | ✅ shipped |
| **v0.51.0** | **Restore `index` + `adr` to MCP; explain indexing in README.** Real-user feedback through zelos surfaced `unknown tool "index"` over MCP — the v0.35 surface narrowing had bucketed `index` operator-only on the theory it was diagnostic. It isn't. v0.51 restores it (and `adr`, same shape — institutional memory the agent reads + writes mid-session) to MCP-visible ([#645](https://github.com/kwad77/pincher/issues/645)). README adds an "Indexing & staleness" section. | ✅ shipped |
| **v0.51.1** | Patch — structured `operator_tool_not_on_mcp` redirect for the remaining 11 operator tools, replacing the SDK's bare `unknown tool "X"` ([#644](https://github.com/kwad77/pincher/issues/644)). Stub mechanism deleted in v0.52. | ✅ shipped |
| **v0.52.0** | **Full MCP restoration; bedrock-layer surface contract.** Reversal of v0.35 #624. All 22 tools agent-callable via MCP with typed schemas; the v0.51.1 stub mechanism deleted entirely. Aggregator deployment (zelos / bifrost / detour-shape) makes the original "fewer tools = less decision tax" argument null — the agent's working set is `N backends × M tools each`, pincher's 22 vs 11 is invisible noise. Real cost was bare "unknown tool" errors biting users. ([#645](https://github.com/kwad77/pincher/issues/645) follow-on). | ✅ shipped |
| **v0.53.0** | **Router-integration contract: capabilities + complexity tiers + release channels.** Phase 1 release 2 of 9. `_meta.capabilities` advertises runtime-detected feature support so routers don't scrape version strings ([#649](https://github.com/kwad77/pincher/issues/649)). Per-tool `complexity_tier` (lite/standard/heavy) on every response + `x-pincher-tier` in OpenAPI lets routers pick the right model for the agent step that consumes the response ([#650](https://github.com/kwad77/pincher/issues/650)). Release-channel infrastructure: stable channel = minor%10==0; everything else dev; `-beta`/`-alpha`/`-rc` suffixes route to their channels ([#642](https://github.com/kwad77/pincher/issues/642)). Schema unchanged at v24. | ✅ shipped |
| **v0.54.0** | **Closure tables + streamable-HTTP MCP transport.** Phase 1 release 3 of 9. First beta-tag release (`v0.54.0-beta.1`) exercising v0.53's release-channel infrastructure end-to-end. Schema **v25**: closure-tables phase 1 ([#652](https://github.com/kwad77/pincher/issues/652)) materializes the depth-3 transitive closure at index time so `trace` becomes a single indexed SELECT (~1ms) vs recursive CTE (5–50ms) when `PINCHER_CLOSURE_TABLES=1`. Streamable-HTTP MCP transport ([#651](https://github.com/kwad77/pincher/issues/651)) mounts on the existing HTTP server at a configurable path — routers (zelos, bifrost) deployed in k8s skip per-backend stdio sub-process spawning. `cmd/closurebench/` measurement tool ([#639](https://github.com/kwad77/pincher/issues/639)) validated default depth=3 at ~325 MB worst-case on 10k files. Capabilities `closure_tables` + `streamable_http` advertised conditionally. | ✅ shipped |
| **v0.55.0** | **Dogfood iteration: CI cycle-time + signal hardening.** Umbrella [#681](https://github.com/kwad77/pincher/issues/681). Five buckets: kill Windows stdio-timing flakes; resolve bench-regression noise (promote-or-remove); eliminate CHANGELOG merge conflicts via per-issue stub files; profile + speed Windows test job; workflow ergonomics (stacked-PR base handling, rerun queue). Plus closure-table at-scale measurement ([#686](https://github.com/kwad77/pincher/issues/686)) and REFERENCE.md staleness audit ([#688](https://github.com/kwad77/pincher/issues/688)). | 🚧 in flight |
| **v1.0** | Tool schemas frozen, schema attestation, migration guide, public launch. Tracking: [#638](https://github.com/kwad77/pincher/issues/638). | planned |

Live milestone burndown: <https://github.com/kwad77/pincher/milestones>. Full punch lists per release: [#193](https://github.com/kwad77/pincher/issues/193).

---

## Known limitations

- **Sequence-rename ID instability in YAML / JSON arrays.** Inserting an item at index 0 of a YAML sequence renames every downstream symbol's qualified name (`tasks.0` → `tasks.1`). Move detection catches some cases but not deterministically. Decided as won't-fix in v0.7.0 ([#205](https://github.com/kwad77/pincher/issues/205)) — the blast radius is mostly Ansible/k8s manifests which are searched via `corpus=config` BM25 anyway, where qualified-name churn is invisible. For long-lived stored references, prefer searching by name over storing the id.
- **Single-user SQLite.** Cross-process indexing is safe (filesystem lockfile). Team / enterprise shared indexes need a server mode — explicitly out of v1.0 scope.
- **~7 languages without extractors.** Scala, Lua, Zig, Elixir, Haskell, Dart, R are detected as source but emit zero symbols. Adding any of them = implement one Go interface.
- **`notifications/tools/list_changed` requires client support ([#429](https://github.com/kwad77/pincher/issues/429)).** Supervised mode emits the notification after every respawn — confirmable via `pincher.supervisor.status` (`tools_list_changed_emitted` counter). Cursor, Codex, and Zed re-issue `tools/list` and pick up newly-added tools live. Claude Code (as of this writing) does not honour the notification — after a binary swap that adds tools, a fresh session is required to surface them in that client. Existing tools remain callable in-session via the auto-restart path; only new-tool *discovery* is affected.

> **Recently resolved (re-index recommended after upgrade):** the dead_code false-positive triangle (#423 receiver-type field-method calls, v0.19; #493 interface dispatch, v0.20; #565/#576 function-value-as-field, v0.21+v0.22) and the polymorphic-method CALLS edge persistence from pre-v0.17 (#475). Run `pincher index <path> --force` once after upgrading to pick up the new edges. Full release-by-release history in [`CHANGELOG.md`](CHANGELOG.md); long-form rationale per item in [REFERENCE.md → Known limitations](docs/REFERENCE.md#known-limitations).

---

## License

MIT

<div align="center">
  <img src="docs/assets/crab.png" width="32" alt="Pinchy"/>
</div>
