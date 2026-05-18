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

[What it does](#what-it-does) · [Where pincher fits](#where-pincher-fits) · [Quick Start](#quick-start) · [Token savings](#token-savings) · [Roadmap](#roadmap) · [Limitations](#known-limitations) · 📖 **[Reference manual](docs/REFERENCE.md)**

</div>

---

## What it does

**The grab is token savings.** An agent asks "what does `processPayment` do?" Pincher returns the symbol's source plus its direct callers and imports in ~300 tokens. The same answer from `Read` over the containing file is ~12 KB. Multiply by every navigation step in a long session and the cost collapses an order of magnitude — measured savings ratios on real codebases routinely sit at 80×+ vs the agent's default read-then-grep loop. Every response carries a `_meta` envelope with real BPE token counts (`cl100k_base` — exact for Claude / OpenAI, approximate for Gemini / Llama) and the savings number, totaled across sessions in SQLite so the running balance is always visible.

**The retention is routing.** Pincher is a shared code-intelligence backend that talks both **MCP stdio** (so Claude Code, Codex, Cursor, and any other MCP client get the same answers) and an **HTTP REST gateway** at `/v1/<tool>` with full OpenAPI 3.1.0 contracts (so a router, a webhook, or a curl one-liner reaches the same surface). Every response also advertises `_meta.capabilities`, `_meta.complexity_tier` (`lite`/`standard`/`heavy`), and stable `X-Request-ID` headers — the hooks a multi-agent router needs to assign the right model to the right step. One backend, one index, one running savings total, every agent in your stack.

Sourcegraph, OpenGrok, and IntelliJ index a codebase for humans browsing it; pincherMCP indexes the same codebase for an LLM agent calling tools. The agent-shaped surface is the whole point — responses sized for a context window rather than a UI pane, runtime interception of Read and Grep calls before the agent opens the file, and a local-only binary so neither the index nor the code leaves the machine.

Under the hood: a single Go binary that indexes the codebase into three co-located layers — byte-offset symbol store, knowledge graph, and FTS5 full-text search — populated in a single AST parse pass from one shared `symbols` table; no duplication, no sync overhead. All three are exposed through **23 agent-callable MCP tools**, every one also reachable via the HTTP REST API at `/v1/<tool>`.

Concrete shape of a single response — every tool returns the same envelope:

```json
{
  "name": "processPayment",
  "source": "func processPayment(amount float64) error { ... }",
  "_meta": {
    "tokens_used":       312,
    "tokens_saved":      14500,
    "tokens_saved_pct":  97.9,
    "complexity_tier":   "lite",
    "latency_ms":        2
  }
}
```

Token savings accumulate in SQLite across sessions — every reconnect adds to the running all-time total surfaced on the dashboard.

> 📖 **Full manual:** [`docs/REFERENCE.md`](docs/REFERENCE.md) — every tool, every flag, every endpoint, schema history, performance numbers, project layout. This README is pitch + quickstart.

---

## Where pincher fits

```
                   ┌─────────────────────────────────────────┐
                   │  Agent runtime / MCP router / aggregator│
                   │  (multi-backend coordination layer)     │
                   └─────────────────┬───────────────────────┘
                                     │ MCP stdio · streamable-HTTP · REST
                                     │ reads _meta.capabilities, _meta.complexity_tier,
                                     │ X-Request-ID, OpenAPI schemas
                       ┌─────────────┼─────────────┐
                       ▼             ▼             ▼
                  ┌─────────┐  ┌──────────┐  ┌──────────┐
                  │ pincher │  │  other   │  │  other   │
                  │ (this)  │  │ backend  │  │ backend  │
                  │ bedrock │  │   ...    │  │   ...    │
                  └─────────┘  └──────────┘  └──────────┘
```

pincher is a **bedrock backend** — a reliable, local-first source of code-intelligence facts that other layers compose on top of. The integration contract is metadata exposure, not opinions about how to use the data:

- **`_meta.capabilities`** advertises what this binary can actually do (e.g. `closure_tables`, `streamable_http`, `hook_check`) so a router doesn't have to parse version strings or guess from feature flags.
- **`_meta.complexity_tier`** (`lite` / `standard` / `heavy`) per response lets a router pick the right model for the agent step that *consumes* the response — a 200-token `symbol` lookup is a different model assignment than a 50k-token `architecture` survey.
- **`X-Request-ID`** correlation is threaded across HTTP / streamable-HTTP / stdio `_meta` / logs so distributed traces walk through pincher end-to-end.
- **OpenAPI 3.1 at `/v1/openapi.json`** with per-tool `x-pincher-tier` typings; any MCP- or REST-shaped consumer can codegen against it. Codegen config + a `scripts/generate-sdks.sh` wrapper for TypeScript / Python / Go SDKs lives under [`sdks/`](sdks/) ([#1262](https://github.com/kwad77/pincher/issues/1262) — local-generation today, registry publishing pending the credentials wire-up).

The bet is that *correctness* is the bedrock virtue. If pincher returns silently-wrong results — empty graphs that look populated, sorted rows that aren't, audit queries that don't address what was asked — every layer above has to wrap pincher in fallback paths or escape hatches, and the integration contract erodes. The roadmap below is mostly about closing those gaps so a router can hand work to pincher and trust the response shape.

**Deployment shapes:** single-binary local dev (stdio), long-lived process behind a reverse proxy (REST + streamable-HTTP), or co-deployed alongside a router in a multi-backend cluster. The same binary, the same indexed data, the same surface.

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
pincher init --target=vscode             # .github/copilot-instructions.md (Copilot rules)
pincher init --target=vscode-mcp         # .vscode/mcp.json (Copilot Chat MCP server)
pincher init --target=jetbrains          # .idea/.junie/guidelines.md (IntelliJ IDEA, PyCharm, GoLand, WebStorm)
pincher init --target=detect             # auto-detect from marker files in cwd

# 3. Index your project
pincher index /path/to/your/project

# 4. Point your MCP client at the binary (Claude Code / Cursor / Codex / VS Code / Zed below)
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
<summary><b>VS Code Copilot Chat</b> — <code>./.vscode/mcp.json</code> (run <code>pincher init --target=vscode-mcp</code>)</summary>

```json
{
  "servers": {
    "pincher": {
      "type": "stdio",
      "command": "/path/to/pincher",
      "args": ["supervised"],
      "env": { "PINCHER_DATA_DIR": "/vscode-isolated/data/dir" }
    }
  }
}
```

`pincher init --target=vscode-mcp` writes this file (with a VS Code-isolated `PINCHER_DATA_DIR`) and preserves any other MCP servers you've added. Pair with `pincher init --target=vscode` to drop the Copilot instructions file at `.github/copilot-instructions.md` so Copilot Chat knows when to call pincher.
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

Continue, Windsurf, Aider, Warp, Gemini CLI, and any MCP-compatible client follow the same pattern. For editors without MCP, use the [HTTP REST API](docs/REFERENCE.md#http-rest-api).

For managed installs (Homebrew, systemd, launchd, Windows service, Docker), see [`packaging/README.md`](packaging/README.md).

### Tutorials

End-to-end walkthroughs (~10 min each):

- **[Claude Code](docs/tutorials/claude-code.md)** — install → index → `pincher init` → wire MCP → first query.
- **[Cursor](docs/tutorials/cursor.md)** — same flow with `pincher init --target=cursor` and Cursor's `.mdc` rules format.
- **[VS Code Copilot Chat](docs/tutorials/vscode-copilot.md)** — `pincher init --target=vscode` (rules) + `--target=vscode-mcp` (MCP), verify Copilot picks up pincher tools.
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

The roadmap focuses on **direction**, not history. Per-release notes for everything below live in [`CHANGELOG.md`](CHANGELOG.md); aggregate milestone burndown at <https://github.com/kwad77/pincher/milestones>.

| Release | Theme | Status |
|---|---|---|
| **v0.2 → v0.55** | Foundational work — index quality at scale, the three-layer storage shape, self-healing MCP (supervised mode + auto-restart-on-drift), the dead_code FP triangle close across receiver-type / interface-dispatch / function-value-as-field, JS AST default-on, HTTP REST gateway with paginated dashboard, pinchQL pedagogy across every silent-zero failure path, hook foundation, closure tables + streamable-HTTP transport, capabilities + complexity-tier + X-Request-ID metadata exposure, CI hardening. ~600 issues closed over the line. | ✅ shipped |
| **v0.56** | Bedrock-layer observability — SSE `/v1/events`, X-Request-ID correlation across stdio/HTTP/streamable-HTTP, prune-stale + vacuum, diff-encoded `context`, auto-reindex on binary-drift respawn. Plus a deep dogfood haul. | ✅ shipped |
| **v0.57** | **Python AST + C CALLS + type-info resolver + silent-confidently-wrong sweep.** Schema **v26**. Three structural extractor wins — full Python AST with IMPORTS+CALLS (the `rich` codebase went 0 → 3,973 resolved edges), per-file CALLS for C, type-info-gated resolver closing the dead_code FP triangle's last leg. A 30-issue sweep through the silent-confidently-wrong family — queries returning plausible-but-wrong results with no signal. Roadmap-row examples: guide audit-template routing + scope ([#921](https://github.com/kwad77/pincher/issues/921)/[#923](https://github.com/kwad77/pincher/issues/923)/[#924](https://github.com/kwad77/pincher/issues/924)), FTS5 CamelCase OR ([#919](https://github.com/kwad77/pincher/issues/919)), pinchQL DISTINCT runs on the full match set ([#929](https://github.com/kwad77/pincher/issues/929)), CONTAINS escape semantics ([#885](https://github.com/kwad77/pincher/issues/885)), max_rows as a hard cap ([#900](https://github.com/kwad77/pincher/issues/900)). | ✅ shipped |
| **v0.58** | **Failure-as-pedagogy at the project boundary.** Every retrieval-shape tool (`symbol`/`symbols`/`context`/`neighborhood`/`trace`/`search`) now surfaces when an unscoped lookup crosses into a different project than the session — closing the silent-cross-project-leak class on mirror projects ([#1049](https://github.com/kwad77/pincher/issues/1049)–[#1052](https://github.com/kwad77/pincher/issues/1052)). `project="*"` handled consistently across all 13 project-arg tools ([#1048](https://github.com/kwad77/pincher/issues/1048)/[#1055](https://github.com/kwad77/pincher/issues/1055)/[#1056](https://github.com/kwad77/pincher/issues/1056)). `doctor` ghost-project advisory + `top` ceiling lowered 500→50 so the response actually fits the MCP token cap ([#1009](https://github.com/kwad77/pincher/issues/1009)/[#1054](https://github.com/kwad77/pincher/issues/1054)). Ghost-extraction diagnosis family closed across `dead_code`/`architecture`/`schema`/`query` ([#1040](https://github.com/kwad77/pincher/issues/1040)–[#1044](https://github.com/kwad77/pincher/issues/1044)). `changes` empty-state diagnosis ([#1053](https://github.com/kwad77/pincher/issues/1053)). Pinned Python corpus + YAML/Markdown edge-case tests ([#1057](https://github.com/kwad77/pincher/issues/1057)). | ✅ shipped |
| **v0.59 + v0.60** | **Phase 1 stable promotion — silent-confidently-wrong family drained, drift correctness, encoder consistency.** Bundles the v0.59 hardening drain (17 PRs landing pinchQL warning emitters incl. aliased-aggregate-mixed-with-bare ([#1155](https://github.com/kwad77/pincher/issues/1155)), edge-graph-empty surfacing across `trace`/`dead_code`/`neighborhood`/`context` ([#858](https://github.com/kwad77/pincher/issues/858)/[#1145](https://github.com/kwad77/pincher/issues/1145)/[#1146](https://github.com/kwad77/pincher/issues/1146)), CI gates for PR title↔body close-ref consistency ([#1103](https://github.com/kwad77/pincher/issues/1103))) with the v0.60 correctness blockers — atomic drift-reindex binary-version stamping ([#986](https://github.com/kwad77/pincher/issues/986)), binary-version downgrade race in `UpsertProjectMeta` ([#1154](https://github.com/kwad77/pincher/issues/1154)), C reserved-keyword extractor false positives ([#1148](https://github.com/kwad77/pincher/issues/1148)), vacuum WAL-reader advisory ([#1149](https://github.com/kwad77/pincher/issues/1149)), JSON encoder consistency across all `_meta`-bearing surfaces ([#1152](https://github.com/kwad77/pincher/issues/1152)). 41-shape known-good pinchQL safety net guards future warning emitters. Capability advertisement runtime-probed in CI. Tagged as v0.60.0 — v0.59 work absorbed since the hardening + stable-promotion sequence shipped without an intermediate tag. Umbrella: [#665](https://github.com/kwad77/pincher/issues/665). | ✅ shipped |
| **v0.61** | **Phase 2 entry — TypeScript foundation.** TS class methods now extract as Method symbols with Parent=class; per-file CALLS pass enabled so TS edge graphs are no longer empty; polymorphic-method-name blocklist generalized into a per-language map with TS + Python entries pre-populated for the v0.62+ resolvers. Receiver-type resolver itself rolls to v0.62 ([#1177](https://github.com/kwad77/pincher/issues/1177)) where the TS AST extractor work lands. | ✅ shipped |
| **v0.62** | **Regex-tier CALLS sweep — every regex-tier language emits CALLS edges.** Closes the #858 edge-graph-empty warning surface for the regex-tier set: Rust, Java, PHP, C#, Kotlin, Swift, Ruby all opt into the shared `regexCallScan` path. `regexCallScan` generalized from `{`-only signature-skip to `{`-or-first-newline so Ruby's end-keyword bodies work. Same-file calls resolve immediately; cross-file resolution rolls forward to [#1177](https://github.com/kwad77/pincher/issues/1177)/[#1182](https://github.com/kwad77/pincher/issues/1182)/[#1183](https://github.com/kwad77/pincher/issues/1183)/[#1184](https://github.com/kwad77/pincher/issues/1184) in v0.63. | ✅ shipped |
| **v0.63** | **Stub-tier language audit — 6 of 7 stubs promoted to regex-tier.** Lua / Elixir / Zig (round 1) and Scala / Dart / R (round 2) all promoted; Haskell deferred (indentation-sensitive layout requires harder regex representation). Every detected language now emits symbols + same-file CALLS edges except Haskell. Rust/Java AST extractors + Python real-corpus validation + TS receiver-type resolver carried forward to v0.64+ ([#1177](https://github.com/kwad77/pincher/issues/1177) / [#1182](https://github.com/kwad77/pincher/issues/1182) / [#1183](https://github.com/kwad77/pincher/issues/1183) / [#1184](https://github.com/kwad77/pincher/issues/1184)). | ✅ shipped |
| **v0.64** | **Dashboard data plumbing + description-honesty audit.** Schema **v27** lands `session_tool_calls` — per-call event log persisted on every response, substrate for the dashboard triangulating panels (tool-call entropy, payload distribution, per-tier saved-percentage medians). Description audit drains stale claims accumulated across v0.57→v0.63: `dead_code` no longer claims `language=Go` is a default (Python AST shipped in v0.57); `health` distinguishes three parser tiers AST/Regex/Stub (post-v0.63 the bucketing collapsed real-regex with empty-stub coverage). python-web pinned corpus extends the AST gate with decorators + inheritance + async. v0.63 stub-promotion CI fallout (`node-monorepo` snapshot, `profile_test`) bundled. Panel rendering rolls to v0.65. | ✅ shipped |
| **v0.65** | **Description-honesty audit sweep.** Seven tool descriptions drained of stale/incomplete claims — `search` min_confidence corpus='docs' branch, `architecture` every-response-field naming, `query` cypher-alias v1.0 removal anchor ([#638](https://github.com/kwad77/pincher/issues/638)), `fetch` correct `kind="Document"` arg syntax (was recommending FTS5 operator syntax — silently wrong), `stats` three-section box + ALL-TIME omission, `symbol` rename-resilience via symbol_moves, `doctor` advisories array + top=50 ceiling. Each PR ships contract tests pinning description-vs-runtime parity. DOGFOOD-2 outcome: description audit was the highest-yield surface for finding real agent-misleading bugs across the v0.6x cycle. | ✅ shipped |
| **v0.66** | **Thin-client envelope + silent-cross-project guard + DOGFOOD haul.** Thin-client payload umbrella ([#1224](https://github.com/kwad77/pincher/issues/1224)): `trace`/`search` `compact=true`, `_meta=lite` env + per-call arg, `trace max_hops` cap, `neighborhood include_fixtures`, `trace include_fixtures` split from include_tests. ~30-50% per-call reduction on trace/search compact + ~150-200 tokens off every response envelope in lite mode. Silent-cross-project guard ([#1232](https://github.com/kwad77/pincher/issues/1232)): `symbol`/`context`/`neighborhood` now error on omitted-project requests whose ID lives only off-session (`cross_project=true` opt-in preserves legacy). Plus a 16-fix DOGFOOD drain: doctor latency 60s→<2s on multi-project DBs ([#1205](https://github.com/kwad77/pincher/issues/1205)), per-project byte estimate ([#1220](https://github.com/kwad77/pincher/issues/1220)), Markdown preamble extraction ([#1097](https://github.com/kwad77/pincher/issues/1097)), pincher vacuum 4-step flow ([#1219](https://github.com/kwad77/pincher/issues/1219)), parity-check guard for #1231 ([#1233](https://github.com/kwad77/pincher/issues/1233)/[#1234](https://github.com/kwad77/pincher/issues/1234)), and more. | ✅ shipped |
| **v0.67** | **Dashboard panel triad + OTLP observability + #1134 resolver fix.** Dashboard substrate becomes user-facing — per-tool call breakdown, per-tier complexity, payload-size distribution, Tool-Mix Health entropy panel ([#635](https://github.com/kwad77/pincher/issues/635) closed). OTLP traces complete the observability story — per-tool-call spans + per-index-pass spans + Error status on `res.IsError` + graceful shutdown, plus a dashboard Backend Status strip + `health.observability` field surfacing metrics/SSE/OTLP state ([#1163](https://github.com/kwad77/pincher/issues/1163)). Multi-language extractor scoping — Rust `impl` blocks and Swift `extension` blocks bind methods to receiver type via shared `scopeRE` mechanism; Kotlin extension functions now capture real method name; #1134 `range over receiver.Field` infers element type ([#1183](https://github.com/kwad77/pincher/issues/1183) partial). Plus search `snippet_lines` knob with query-aware default ([#1091](https://github.com/kwad77/pincher/issues/1091)), CONTRIBUTING + troubleshooting docs ([#1264](https://github.com/kwad77/pincher/issues/1264)), and `cmd/tracelatencybench` closure-vs-CTE measurement utility ([#1162](https://github.com/kwad77/pincher/issues/1162)). | ✅ shipped |
| **v0.68** | **Falsifiable savings + closure correctness + branch-switch reindex.** Phase 2 testing-depth release. `pincher bench` ships as the runs-on-your-own-project savings measurement (subcommand + `--persist` + `/v1/bench-results` HTTP + dashboard Bench History panel — schema v29 `bench_runs`/`bench_results` tables). Closure phase 2 ([#685](https://github.com/kwad77/pincher/issues/685)) — schema v30 records `via_kind` so trace fast-path populates `Via` identically to CTE; `BuildClosure` filters to default trace kinds making closure semantically equivalent to CTE. Closure-tables-default-on decision shipped: real-world mean speedup 2.3-5.6× across four 10k-40k file corpora, 10× p50 bar not met; stays opt-in via `PINCHER_CLOSURE_TABLES=1` ([#1162](https://github.com/kwad77/pincher/issues/1162) closed). `pincher init --git-hooks` installs post-checkout/post-merge/post-rewrite hooks for eager reindex on branch switches ([#1261](https://github.com/kwad77/pincher/issues/1261) §1). Plus `traces_otlp` runtime capability probe + OTLP tracer coverage push 12.5% → 87.5%. Heavy AST work (Java/Rust/TS receiver-type) deferred to v0.71+ — wrong shape for the testing release. | ✅ shipped |
| **v0.69** | **Hardening: watcher hot-path -85% allocs + every server handler below v0.60 baseline + Windows install path + dogfood-found HTML correctness.** Perf regression validation against v0.60 ([#670](https://github.com/kwad77/pincher/issues/670) §2): watcher no-change tick gated on `force \|\| totalFiles > 0`, dropped 4806 → 716 allocs/op (-85%, 5.5× faster) — pincher's 2s watcher poll is now cheap. `_meta.tokens_used` switched from cl100k_base BPE to a char/4 heuristic by default (opt back in via `PINCHER_TOKEN_ACCOUNTING=exact`), cutting 60% of allocs on every authenticated handler call — every Symbol/Search/Query/Architecture bench now below v0.60 baseline. Distribution polish ([#1260](https://github.com/kwad77/pincher/issues/1260)): Scoop manifest for Windows raw-URL install, `pincher update` Homebrew dispatch, ADR-0001 deferring code-signing to v1.0+. `pincher doctor --fix` shipped earlier in the cycle. internal/index coverage 84.2 → 86.1% (#1164 target met). HTML extractor `byte_range_negative` fix found by dogfooding `pincher doctor` against pincher's own `docs/index.html`. v0.70 stable-promotion sign-off pending §3 cross-platform smoke. | ✅ shipped |
| **v0.70** | **Stable promotion** — bug-fix triage + perf regression bar + cross-platform smoke + capability audit all green. Tagged 2026-05-17. Tracks [#670](https://github.com/kwad77/pincher/issues/670). | ✅ shipped |
| **v0.71** | **Ansible graph completion + multi-branch foundation + structured _meta diagnostics + typed-SDK scaffolding.** Schema **v31 → v32**: `branch` column on symbols/edges/files + `projects.current_branch`. The indexer detects the git branch via `git rev-parse --abbrev-ref HEAD` (2s timeout, short-SHA fallback on detached HEAD, 30s per-project cache) and stamps it on every Symbol/Edge it writes; `doctor` (CLI + MCP) fires a branch-drift advisory when the on-disk branch has diverged from the last-indexed branch — closing the silent "checkout without re-index → wrong byte-offsets" footgun ([#1303](https://github.com/kwad77/pincher/issues/1303) Phase 1 + Phase 2a; Phase 2b PK widening for true multi-branch coexistence rolls to v0.72 as [#1371](https://github.com/kwad77/pincher/issues/1371)). Ansible YAML extractor closes BOTH legs of [#71](https://github.com/kwad77/pincher/issues/71) in one cycle — Phase 1 `INCLUDES` playbook→role + `LOADS` host→`host_vars` ([#1160](https://github.com/kwad77/pincher/issues/1160)) AND Phase 2 `USES_VAR` dataflow edges from Jinja `{{ var_name }}` substitutions to canonical `Setting` declarations in `group_vars/` / `host_vars/` / `vars/` / `roles/*/defaults/main.yml` / `roles/*/vars/main.yml` ([#1165](https://github.com/kwad77/pincher/issues/1165)). Structural edges cascade for Bash ([#1341](https://github.com/kwad77/pincher/issues/1341)) / HCL Terraform modules ([#1342](https://github.com/kwad77/pincher/issues/1342)) / Markdown inter-doc references ([#1343](https://github.com/kwad77/pincher/issues/1343)) / Makefile rule-to-rule dependencies ([#1344](https://github.com/kwad77/pincher/issues/1344)). Synthetic external Module symbols at sentinel `@external/<qn>` ensure unresolved IMPORTS edges persist instead of silently dropping at resolve time ([#1340](https://github.com/kwad77/pincher/issues/1340) option a). Structured `_meta.warnings_v2` + `_meta.diagnosis_v2` ship side-by-side with the legacy string forms so MCP hosts can consume typed envelopes ([#1098](https://github.com/kwad77/pincher/issues/1098)). Typed-SDK codegen scaffolding under `sdks/` with openapi-generator configs for TS/Python/Go ([#1262](https://github.com/kwad77/pincher/issues/1262) first slice — registry publishing deferred). MCP `StartSchemaDriftWatcher` exits the process on DB-vs-binary schema-version skew so supervised mode respawns onto a fresh instance ([#1374](https://github.com/kwad77/pincher/issues/1374)). `pincher init --target=jetbrains` writes `.idea/.junie/guidelines.md` for JetBrains AI Assistant ([#1335](https://github.com/kwad77/pincher/issues/1335)). New `pincher hook-stats --export-7d` CLI emits a shareable JSON snapshot of trailing 7-day hook conversion-rate metrics — anonymized by default ([#640](https://github.com/kwad77/pincher/issues/640) field-data thread; closes [#662](https://github.com/kwad77/pincher/issues/662)). Migration guide v0.4 → v1.0 first draft at `docs/migration/v0.4-to-v1.0.md` shipped early ([#1332](https://github.com/kwad77/pincher/issues/1332), v0.73 deliverable). | ✅ shipped |
| **v0.72** | **Scope-to-session sweep finishes + pinchQL IN / COUNT(DISTINCT) land + 30 dogfood-driven fixes.** Completes the strict-cross-project guard ([#1232](https://github.com/kwad77/pincher/issues/1232), v0.66) follow-through across every retrieval-shape tool — `symbol`/`context` ([#1408](https://github.com/kwad77/pincher/issues/1408)), `neighborhood` ([#1425](https://github.com/kwad77/pincher/issues/1425)), `trace` ([#1431](https://github.com/kwad77/pincher/issues/1431)) — so the canonical `search → symbol` workflow stays sane on hosts that have indexed forks of the same source tree. pinchQL grows two long-standing surface gaps: `WHERE col IN [a, b, c]` ([#1439](https://github.com/kwad77/pincher/issues/1439)) and `COUNT(DISTINCT n.prop)` ([#1437](https://github.com/kwad77/pincher/issues/1437)) become first-class. Two Go-extractor shadow fixes drain the same family ([#1423](https://github.com/kwad77/pincher/issues/1423) READS, [#1429](https://github.com/kwad77/pincher/issues/1429) CALLS) — parameter / receiver / body-local shadows no longer emit phantom edges to unrelated project Functions. TS nested-function var-scope replaces #1375's single-slot tracker with a stack ([#1422](https://github.com/kwad77/pincher/issues/1422)) — Next.js App Router page.tsx siblings no longer collide. doctor's extraction_failures rows now carry `binary_version_at_failure` (schema **v33**, [#1421](https://github.com/kwad77/pincher/issues/1421)) plus project-arg filter ([#1401](https://github.com/kwad77/pincher/issues/1401)) and tiered-match ([#1404](https://github.com/kwad77/pincher/issues/1404)). New `pincher verify` integrity-check ([#1399](https://github.com/kwad77/pincher/issues/1399)); `pincher init --target=antigravity` ([#1368](https://github.com/kwad77/pincher/issues/1368)). TypeScript receiver-type resolver ([#1177](https://github.com/kwad77/pincher/issues/1177)) closes the v0.61 TS receiver-type stack. One **BREAKING (pre-1.0):** `projects.current_branch` JSON tag renamed to `last_indexed_branch` ([#1388](https://github.com/kwad77/pincher/issues/1388)). | ✅ shipped |
| **v0.73** | **Migration guide + integrator-facing _meta envelope contract + three-layer agent-leverage story.** Phase 3 docs slice. Migration guide v0.4 → v1.0 reaches review-ready first draft ([#1332](https://github.com/kwad77/pincher/issues/1332)). `docs/integrations/loop-leverage-layers.md` names the three-layer agent-leverage model — hook + _meta + composites ([#1392](https://github.com/kwad77/pincher/issues/1392)). Integrator-facing _meta envelope contract page names the planning-loop input surface ([#1394](https://github.com/kwad77/pincher/issues/1394)). Three docs items written as one coherent body so the conceptual frame stays consistent. | 🚧 in flight |
| **v1.0** | Tool schemas frozen, schema attestation, public launch. First-draft migration guide at [`docs/migration/v0.4-to-v1.0.md`](docs/migration/v0.4-to-v1.0.md). Tracking: [#638](https://github.com/kwad77/pincher/issues/638). | planned |

---

## Known limitations

- **Sequence-rename ID instability in YAML / JSON arrays.** Inserting an item at index 0 of a YAML sequence renames every downstream symbol's qualified name (`tasks.0` → `tasks.1`). Move detection catches some cases but not deterministically. Decided as won't-fix in v0.7.0 ([#205](https://github.com/kwad77/pincher/issues/205)) — the blast radius is mostly Ansible/k8s manifests which are searched via `corpus=config` BM25 anyway, where qualified-name churn is invisible. For long-lived stored references, prefer searching by name over storing the id.
- **Single-user SQLite.** Cross-process indexing is safe (filesystem lockfile). Team / enterprise shared indexes need a server mode — explicitly out of v1.0 scope.
- **Haskell is the only language without an extractor.** v0.63 ([#1186](https://github.com/kwad77/pincher/pull/1186) / [#1187](https://github.com/kwad77/pincher/pull/1187)) promoted Scala / Lua / Zig / Elixir / Dart / R from stub-tier to regex-tier (confidence 0.70). Haskell still emits zero symbols — indentation-sensitive layout with no `{` / `def` / `function` anchor makes regex-tier representation significantly harder; proper extractor tracked under [#1161](https://github.com/kwad77/pincher/issues/1161).
- **Edge graph: Go + Python with full IMPORTS/CALLS resolution; every other extracted language with same-file CALLS only.** As of v0.62 ([#1159](https://github.com/kwad77/pincher/pull/1159)) every regex-tier language emits same-file CALLS edges via the shared `regexCallScan` path. v0.57 ([#856](https://github.com/kwad77/pincher/issues/856)) put Python on the cross-file resolver alongside Go. **Cross-file resolution for the long tail** (TypeScript receiver-type binding, Rust/Java AST, etc.) is tracked under [#1177](https://github.com/kwad77/pincher/issues/1177) / [#1182](https://github.com/kwad77/pincher/issues/1182) / [#1183](https://github.com/kwad77/pincher/issues/1183) — multi-day work rolling forward through v0.65+. The graph-shaped tools (`trace`/`dead_code`/`neighborhood`/`context`) emit a per-language honesty warning when the dominant language lacks cross-file resolution ([#1145](https://github.com/kwad77/pincher/issues/1145) / [#1146](https://github.com/kwad77/pincher/issues/1146)).
- **`notifications/tools/list_changed` requires client support ([#429](https://github.com/kwad77/pincher/issues/429)).** Supervised mode emits the notification after every respawn — confirmable via `pincher.supervisor.status` (`tools_list_changed_emitted` counter). Cursor, Codex, and Zed re-issue `tools/list` and pick up newly-added tools live. Claude Code (as of this writing) does not honour the notification — after a binary swap that adds tools, a fresh session is required to surface them in that client. Existing tools remain callable in-session via the auto-restart path; only new-tool *discovery* is affected.

> **Recently resolved:** v0.72 (shipped 2026-05-17) completes the strict-cross-project guard ([#1232](https://github.com/kwad77/pincher/issues/1232)) follow-through across every retrieval-shape tool — `symbol`/`context` ([#1408](https://github.com/kwad77/pincher/issues/1408)), `neighborhood` ([#1425](https://github.com/kwad77/pincher/issues/1425)), `trace` ([#1431](https://github.com/kwad77/pincher/issues/1431)) — so the canonical `search → symbol` workflow no longer breaks on hosts that have indexed forks of the same source tree. pinchQL grows two long-standing surface gaps: `WHERE col IN [a, b, c]` ([#1439](https://github.com/kwad77/pincher/issues/1439)) and `COUNT(DISTINCT n.prop)` ([#1437](https://github.com/kwad77/pincher/issues/1437)). Two Go-extractor shadow fixes ([#1423](https://github.com/kwad77/pincher/issues/1423) READS, [#1429](https://github.com/kwad77/pincher/issues/1429) CALLS) drain phantom-edge false positives when a local name matches a project Function. Schema **v33** adds `extraction_failures.binary_version_at_failure` so doctor consumers can tell fixed-since-this-binary rows from still-recurring rows without cross-referencing CHANGELOG ([#1421](https://github.com/kwad77/pincher/issues/1421)). One BREAKING (pre-1.0): `projects.current_branch` JSON tag renamed to `last_indexed_branch` ([#1388](https://github.com/kwad77/pincher/issues/1388)) across every response carrying a project record. v0.71 (shipped 2026-05-17) closed the silent "checkout without re-index → wrong byte-offsets" footgun — schema v32 adds `projects.current_branch` plus a `branch` column on `symbols`/`edges`/`files` (v31); the indexer stamps git branch on every Symbol/Edge it writes, and `doctor` surfaces a branch-drift advisory when on-disk branch differs from last-indexed ([#1303](https://github.com/kwad77/pincher/issues/1303) Phase 1 + 2a; Phase 2b PK widening rolls to v0.72 as [#1371](https://github.com/kwad77/pincher/issues/1371)). Ansible YAML closes BOTH legs of [#71](https://github.com/kwad77/pincher/issues/71) in one cycle — Phase 1 `INCLUDES` playbook→role + `LOADS` host→`host_vars` ([#1160](https://github.com/kwad77/pincher/issues/1160)) AND Phase 2 `USES_VAR` Jinja-substitution dataflow edges to canonical `Setting` declarations in `group_vars/` / `host_vars/` / `vars/` / role defaults ([#1165](https://github.com/kwad77/pincher/issues/1165)) — alongside Bash/HCL/Markdown/Makefile structural edges ([#1341](https://github.com/kwad77/pincher/issues/1341)–[#1344](https://github.com/kwad77/pincher/issues/1344)). Synthetic external Module symbols at sentinel `@external/<qn>` keep unresolved IMPORTS edges from silently dropping at resolve time ([#1340](https://github.com/kwad77/pincher/issues/1340)). Structured `_meta.warnings_v2` + `_meta.diagnosis_v2` ship side-by-side with legacy string forms for typed-envelope MCP hosts ([#1098](https://github.com/kwad77/pincher/issues/1098)). Typed-SDK codegen scaffolding under `sdks/` with openapi-generator configs for TS/Python/Go ([#1262](https://github.com/kwad77/pincher/issues/1262)). MCP `StartSchemaDriftWatcher` exits on DB-vs-binary schema-version skew so supervised mode respawns ([#1374](https://github.com/kwad77/pincher/issues/1374)). `pincher init --target=jetbrains` lands JetBrains AI Assistant rules at `.idea/.junie/guidelines.md` ([#1335](https://github.com/kwad77/pincher/issues/1335)). New `pincher hook-stats --export-7d` CLI emits a shareable JSON snapshot of trailing 7-day hook conversion metrics for the [#640](https://github.com/kwad77/pincher/issues/640) field-data thread (closes [#662](https://github.com/kwad77/pincher/issues/662)). v0.70 stable promotion tagged 2026-05-17 with full bug-fix triage + perf regression bar + cross-platform smoke + capability audit green. v0.69 collapsed the v0.60 perf-regression surface that had stacked across Phase 2 feature releases — watcher no-change tick 4806 → 716 allocs/op (-85%, 5.5× faster); `_meta.tokens_used` cl100k BPE → char/4 heuristic by default (opt back in via `PINCHER_TOKEN_ACCOUNTING=exact`) closing every handler bench below the v0.60 baseline. Full release-by-release history in [`CHANGELOG.md`](CHANGELOG.md); long-form rationale per item in [REFERENCE.md → Known limitations](docs/REFERENCE.md#known-limitations).

---

## License

MIT

<div align="center">
  <img src="docs/assets/crab.png" width="32" alt="Pinchy"/>
</div>
