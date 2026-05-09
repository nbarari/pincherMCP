<div align="center">
  <img src="assets/banner.png" alt="pincherMCP — pixel-art mascot Pinchy the crab holding a copper penny, wordmark, and tagline" width="900"/>
</div>

<div align="center">

[![CI](https://github.com/kwad77/pincherMCP/actions/workflows/ci.yml/badge.svg)](https://github.com/kwad77/pincherMCP/actions/workflows/ci.yml)
[![Go 1.25](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/license-MIT-22c55e.svg)](LICENSE)
[![Coverage](https://img.shields.io/badge/coverage-84.3%25-22c55e.svg)](docs/REFERENCE.md#test-coverage)

**Codebase intelligence server for LLM agents.**
Single binary · No cloud dependencies · Any LLM · MCP stdio or HTTP REST

</div>

---

## What it does

pincherMCP is a single Go binary that indexes a codebase into three co-located layers — byte-offset symbol store, knowledge graph, and FTS5 full-text search — and exposes all three through **16 MCP tools** or an HTTP REST API.

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
# 1. Build (Go 1.25+, pure Go — no CGO, no C compiler)
git clone https://github.com/kwad77/pincherMCP && cd pincherMCP
go build -o pincher ./cmd/pinch/         # or pincher.exe on Windows

# 2. Drop the policy block into your project's CLAUDE.md (one-time)
./pincher init                           # writes ./CLAUDE.md
./pincher init --global                  # writes ~/.claude/CLAUDE.md

# 3. Index your project
./pincher index /path/to/your/project

# 4. Point your MCP client at the binary (Claude Code / Cursor / Zed examples below)
#    Or open the dashboard: ./pincher web
```

### Client configuration

pincher speaks the standard JSON-RPC 2.0 MCP protocol over stdio. The `command` field is the same everywhere — only the file location and key name change.

<details>
<summary><b>Claude Code</b> — <code>~/.claude/mcp.json</code></summary>

```json
{
  "mcpServers": {
    "pincher": { "type": "stdio", "command": "/path/to/pincher" }
  }
}
```
</details>

<details>
<summary><b>Cursor</b> — <code>~/.cursor/mcp.json</code></summary>

```json
{
  "mcpServers": {
    "pincher": { "command": "/path/to/pincher" }
  }
}
```
</details>

<details>
<summary><b>Zed</b> — <code>settings.json</code> under <code>context_servers</code></summary>

```json
{
  "context_servers": {
    "pincher": {
      "command": { "path": "/path/to/pincher", "args": [] }
    }
  }
}
```
</details>

Continue, Windsurf, and any MCP-compatible client follow the same pattern. For editors without MCP, use the [HTTP REST API](docs/REFERENCE.md#http-rest-api).

For managed installs (Homebrew, systemd, launchd, Windows service, Docker), see [`packaging/README.md`](packaging/README.md).

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

Measured on this codebase (13 files, 618 symbols, 5,785 edges): cold index 190 ms, single-hop Cypher 2 ms, BFS depth 3 <5 ms, FTS5 search 1 ms. Full benchmark + methodology in [REFERENCE.md → Performance](docs/REFERENCE.md#performance).

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

Typical per-call savings: `symbol` ~95%, `context` ~90%, `search` ~98%, `architecture` ~99.99%, `trace` ~99%.

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

| Tier | Theme | Status |
|---|---|---|
| **v0.2** | Index quality at scale (Bash, HCL, Markdown, Jinja2 extractors; per-corpus FTS5 split; pinned-corpus snapshot tests) | ✅ shipped |
| **v0.3** | Trust + observability (security audit, `pincher doctor`, dashboard CSP tightening, FTS5 escape hatch, per-symbol confidence) | ✅ shipped |
| **v0.4** | Performance under load (pinned-corpus benchmarks, reader pool, sessions schema for HTTP discovery, `pincher web` / `init` / `update`) | 🚧 in flight |
| **v0.5** | Polish + extension surface (struct field extraction, cross-project query, webhook re-index, VS Code extension, `.pincher.yml` config) | planned |
| **v1.0** | Stable API (tool schemas frozen, symbol-ID format frozen, HTTP REST surface frozen, SECURITY.md) | planned |

Issues + PR tracker: <https://github.com/kwad77/pincherMCP/issues>. Per-tier detail in [REFERENCE.md → Roadmap](docs/REFERENCE.md#roadmap).

---

## Known limitations

- **`go install` is not yet supported.** The `go.mod` module path (`github.com/pincherMCP/pincher`) doesn't match the GitHub URL (`kwad77/pincherMCP`); `go install github.com/...@latest` fails until that's reconciled. Clone + `go build` works today; `pincher update` from a checkout pulls + rebuilds in place.
- **Pre-built release binaries** are not yet attached to GitHub release tags. The asset-fetching path in `pincher update` is ready for them and will activate once the release workflow uploads artifacts.
- **Sequence-rename ID instability in YAML.** Inserting an item at index 0 of a YAML sequence renames every downstream symbol's qualified name (`tasks.0` → `tasks.1`). Move detection catches some cases but not deterministically.
- **Single-user SQLite.** Cross-process indexing is safe (filesystem lockfile). Team/enterprise shared indexes need a server mode that's not built yet.
- **~7 languages without extractors.** Scala, Lua, Zig, Elixir, Haskell, Dart, R are detected as source but emit zero symbols. Adding any of them = implement one Go interface.

Full known-limitations list, with severity and tracking issue: [REFERENCE.md → Known Limitations](docs/REFERENCE.md#known-limitations).

---

## License

MIT

<div align="center">
  <img src="docs/assets/crab.png" width="32" alt="Pinchy"/>
</div>
