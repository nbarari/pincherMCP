<div align="center">
  <img src="assets/banner.png" alt="pincherMCP — pixel-art mascot Pinchy the crab holding a copper penny, wordmark, and tagline" width="900"/>
</div>

<div align="center">

[![CI](https://github.com/kwad77/pincher/actions/workflows/ci.yml/badge.svg)](https://github.com/kwad77/pincher/actions/workflows/ci.yml)
[![Go 1.25](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/license-MIT-22c55e.svg)](LICENSE)
[![Coverage](https://img.shields.io/badge/coverage-83%25-22c55e.svg)](docs/REFERENCE.md#test-coverage)

**Codebase intelligence server for LLM agents.**
Single binary · No cloud dependencies · Any LLM · MCP stdio or HTTP REST

[What it does](#what-it-does) · [Quick Start](#quick-start) · [Self-healing connections](#self-healing-connections) · [Why it's fast](#why-its-fast) · [Token savings](#token-savings) · [Staying current](#staying-current) · [Roadmap](#roadmap) · [Limitations](#known-limitations)

</div>

---

## What it does

pincherMCP is a single Go binary that indexes a codebase into three co-located layers — byte-offset symbol store, knowledge graph, and FTS5 full-text search — and exposes all three through **20 MCP tools** or an HTTP REST API.

Every tool response includes a `_meta` envelope with real BPE token counts (cl100k_base — exact for Claude and OpenAI families, approximate for Gemini/Llama), latency, and cost avoided:

```json
{
  "name": "processPayment",
  "source": "func processPayment(amount float64) error { ... }",
  "_meta": {
    "tokens_used":  312,
    "tokens_saved": 14500,
    "latency_ms":   2,
    "cost_avoided": "$0.0435"
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
│  Saved:             ~44,000 tokens   37x   │
│  Cost avoided:        $0.1320              │
│  Avg latency:         2 ms                 │
└────────────────────────────────────────────┘
```

**Without pincher** is the estimated baseline (whole file reads). **With pincher** is the actual BPE token count of what was returned. Savings persist in SQLite across reconnects, process restarts, and binary upgrades — the dashboard at `/v1/dashboard` shows the all-time total.

Typical per-call savings: `symbol` ~95%, `context` ~90%, `search` ~98%, `trace` ~99%. (`architecture` returns metadata only — no file-read alternative — so its `tokens_saved` is reported as 0 rather than fabricated, see [#219](https://github.com/kwad77/pincher/issues/219).)

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
| **v0.17.0** | **Receiver-type tracking + extractor precision.** Function-typed field call resolution ([#423](https://github.com/kwad77/pincher/issues/423)) — proper fix for the long-standing "empty callees on obvious Methods" symptom that #410/#465 only partially addressed via stoplists. | 🚧 in flight |
| **v1.0** | Tool schemas frozen, schema attestation, migration guide, public launch. | planned |

Live milestone burndown: <https://github.com/kwad77/pincher/milestones>. Full punch lists per release: [#193](https://github.com/kwad77/pincher/issues/193).

---

## Known limitations

- **Sequence-rename ID instability in YAML / JSON arrays.** Inserting an item at index 0 of a YAML sequence renames every downstream symbol's qualified name (`tasks.0` → `tasks.1`). Move detection catches some cases but not deterministically. Decided as won't-fix in v0.7.0 ([#205](https://github.com/kwad77/pincher/issues/205)) — the blast radius is mostly Ansible/k8s manifests which are searched via `corpus=config` BM25 anyway, where qualified-name churn is invisible. For long-lived stored references, prefer searching by name over storing the id. Full rationale in [REFERENCE.md → Known limitations](docs/REFERENCE.md#known-limitations).
- **Single-user SQLite.** Cross-process indexing is safe (filesystem lockfile). Team / enterprise shared indexes need a server mode — explicitly out of v1.0 scope.
- **~7 languages without extractors.** Scala, Lua, Zig, Elixir, Haskell, Dart, R are detected as source but emit zero symbols. Adding any of them = implement one Go interface.
- **In-flight response loss during supervised binary upgrade ([#371](https://github.com/kwad77/pincher/issues/371)).** Affected v0.11.0 specifically — the first non-`health` tool call that fired on the freshly-upgraded binary lost its response; client reported `MCP error -32000`. Fixed in v0.11.1 (server-side defer + supervisor sentinel-id init replay). Upgrade to v0.11.1 or later.
- **`notifications/tools/list_changed` requires client support ([#429](https://github.com/kwad77/pincher/issues/429)).** Supervised mode emits the notification after every respawn — confirmable via `pincher.supervisor.status` (the `tools_list_changed_emitted` counter increments per emit). MCP clients that honour the notification (Cursor, Codex, Zed) re-issue `tools/list` and pick up newly-added tools live. Claude Code (as of this writing) does not honour the notification — after a binary swap that adds tools, a fresh session is still required to surface them in that client. Existing tools remain callable in-session via the auto-restart path; only new-tool *discovery* is affected.

Full known-limitations list, with severity and tracking issue: [REFERENCE.md → Known Limitations](docs/REFERENCE.md#known-limitations).

---

## License

MIT

<div align="center">
  <img src="docs/assets/crab.png" width="32" alt="Pinchy"/>
</div>
