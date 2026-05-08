<div align="center">
  <img src="assets/banner.png" alt="pincherMCP — pixel-art mascot Pinchy the crab holding a copper penny, wordmark, and tagline" width="900"/>
</div>

<div align="center">

[![CI](https://github.com/kwad77/pincherMCP/actions/workflows/ci.yml/badge.svg)](https://github.com/kwad77/pincherMCP/actions/workflows/ci.yml)
[![Go 1.24](https://img.shields.io/badge/go-1.24-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/license-MIT-22c55e.svg)](LICENSE)
[![Coverage](https://img.shields.io/badge/coverage-84%25-22c55e.svg)](#development)

**Codebase intelligence server for LLM agents.**
Single binary · No cloud dependencies · Any LLM · MCP stdio or HTTP REST

</div>

---

## Table of Contents

- [What it does](#what-it-does)
- [Quick Start](#quick-start)
- [Architectural Diagrams](#architectural-diagrams)
- [15 Tools — Tested Capabilities](#15-tools--tested-capabilities)
- [Cypher Query Reference](#cypher-query-reference)
- [Language Support](#language-support)
- [HTTP REST API](#http-rest-api)
- [Token Savings](#token-savings)
- [Installation](#installation)
- [Performance](#performance)
- [Roadmap](#roadmap)
- [Development](#development)

---

## <img src="docs/assets/crab.png" width="22" alt=""/> What it does

pincherMCP is a single Go binary that indexes a codebase into three co-located layers — byte-offset symbol store, knowledge graph, and FTS5 full-text search — and exposes all three through 15 MCP tools or an HTTP REST API.

Every tool response includes a `_meta` envelope with real BPE token counts (cl100k_base — exact for Claude and OpenAI model families, approximate for Gemini/Llama), latency, and cost avoided:

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

Token savings accumulate across sessions — every reconnect adds to a running all-time total in SQLite.

All three indexes are built in a **single AST parse pass** from one shared `symbols` table. No duplication, no sync overhead.

---

## <img src="docs/assets/crab.png" width="22" alt=""/> Quick Start

```bash
# 1. Build
git clone https://github.com/kwad77/pincherMCP && cd pincherMCP
go build -o pincher ./cmd/pinch/

# 2. Point your MCP client at the binary (examples below for Claude Code,
#    Cursor, and Zed — the stdio command is the same everywhere).

# 3. Index your project
pincher index /path/to/your/project

# 4. Query (via your MCP client, or via HTTP if you ran with --http)
# e.g. the `search` tool with query="processPayment"
#      the `context` tool with id="src/payments/processor.go::payments.processPayment#Function"
```

### Client configuration

Any MCP-compatible client works — pincher speaks the standard JSON-RPC 2.0
over stdio protocol. Three common clients:

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

Continue, Windsurf, and any other MCP client follow the same pattern — run
`pincher` as a stdio subprocess. For editors without MCP support, use the
HTTP REST API (below) instead.

Or run the local HTTP dashboard alongside the MCP process:

```bash
pincher --http :8080 --http-key mysecrettoken
# or let the OS pick a free port:
pincher --http :0
```

For managed installs (Homebrew, systemd, launchd, Windows service),
see [`packaging/README.md`](packaging/README.md).

---

## <img src="docs/assets/crab.png" width="22" alt=""/> Architectural Diagrams

### Two-Process Architecture

```
  Claude Code (IDE)
        │
        │ JSON-RPC 2.0 (stdio)
        ▼
┌───────────────────────┐          ┌───────────────────────────┐
│  pincher (MCP process)│          │  pincher --http :8080     │
│                       │          │  (dashboard / REST)        │
│  • 15 MCP tools       │          │                           │
│  • idx.Watch()        │          │  • POST /v1/{tool}        │
│  • SessionFlusher     │          │  • GET /v1/dashboard      │
│    (flush every 10s)  │          │  • GET /v1/openapi.json   │
│                       │          │  • GET /v1/sessions       │
│                       │          │  • DELETE /v1/projects    │
└──────────┬────────────┘          └───────────┬───────────────┘
           │                                   │
           │     Both share the same SQLite file
           └─────────────┬─────────────────────┘
                         │
                         ▼
             ┌─────────────────────┐
             │  SQLite WAL         │
             │  pincher.db         │
             │                     │
             │  • symbols          │
             │  • edges            │
             │  • symbols_fts      │
             │  • projects         │
             │  • sessions         │
             │  • symbol_moves     │
             │  • adr_entries      │
             │  • schema_version   │
             └─────────────────────┘
```

The HTTP process retries port binding for up to 10 seconds on startup — so reconnecting the MCP process (which briefly holds the port) does not break the dashboard.

---

### Three-Layer Storage

All three layers are populated in **one AST parse pass** from one `symbols` row. No separate sync, no duplication.

```
                         Source File
                              │
                         ast.Extract()
                              │
               ┌──────────────┴──────────────┐
               │         symbols row          │
               │  id · file_path · name       │
               │  start_byte · end_byte       │
               │  kind · language · parent    │
               │  signature · docstring       │
               │  complexity · is_exported    │
               │  extraction_confidence       │
               └──────┬──────────┬────────────┘
                      │          │
          ┌───────────┘          └──────────────┐
          ▼                                     ▼
  ┌───────────────┐    ┌──────────────┐   ┌────────────────┐
  │  Layer 1      │    │  Layer 2     │   │  Layer 3       │
  │  Byte-Offset  │    │  Knowledge   │   │  FTS5 BM25     │
  │  Symbol Store │    │  Graph       │   │  Full-Text     │
  │               │    │              │   │                │
  │  start_byte   │    │  symbols +   │   │  symbols_fts   │
  │  end_byte     │    │  edges table │   │  virtual table │
  │               │    │              │   │  (3 triggers   │
  │  Retrieval:   │    │  Queries:    │   │   auto-sync)   │
  │  1 SQL +      │    │  node scan   │   │                │
  │  1 os.Seek +  │    │  JOIN (1-hop)│   │  BM25 ranking  │
  │  1 os.Read    │    │  BFS (n-hop) │   │  across name + │
  │               │    │  via CTE     │   │  signature +   │
  │  O(1), <1ms   │    │  <2ms        │   │  docstring     │
  └───────────────┘    └──────────────┘   └────────────────┘
```

---

### Cypher Query Routing

The Cypher engine tokenizes and parses each query, then routes to one of three SQL strategies:

```
  MATCH (n) WHERE ...              →  runNodeScan
  (no edge pattern)                   Simple SELECT + WHERE
                                       Sub-ms on indexed columns

  MATCH (a)-[:CALLS]->(b) WHERE   →  runJoinQuery
  (single-hop, fixed edge kind)       Single SQL JOIN
                                       Sub-ms via idx_edge_from/to

  MATCH (a)-[:CALLS*1..3]->(b)    →  runBFS
  (variable-length path)              Go BFS loop over CTE
                                       Bounded by depth + MaxRows
                                       <5ms at depth 3
```

All three paths are project-scoped — cross-project data leakage is structurally impossible.

---

### Data Flow: Index to Query

```
  pincher index path="/my/repo"
        │
        ▼
  index.Index()
   ├── Walk files (gocodewalker, respects .gitignore)
   ├── Hash each file (xxh3, skip if unchanged)
   ├── ast.Extract(source, language, relPath)
   │    ├── Go:    go/ast → exact byte offsets, confidence=1.0
   │    └── Other: regex  → approximate offsets, confidence=0.70–0.85
   ├── Batch upsert symbols (500/batch)
   ├── Batch upsert edges (1000/batch)
   └── FTS5 triggers auto-sync symbols_fts

  idx.Watch() polls every 2s (active) or 30s (idle)
  and re-runs Index() on changed files incrementally.
  No manual re-index required during a session.

  On file move: (qualified_name, kind) match detected →
  symbol_moves redirect recorded → handleSymbol resolves
  stale IDs transparently via store.ResolveStaleID()
```

---

## <img src="docs/assets/crab.png" width="22" alt=""/> 15 Tools — Tested Capabilities

All latencies measured on this codebase (13 files, 618 symbols, 5,785 edges). Token counts use cl100k_base BPE — the same tokenizer family as Claude.

### Indexing & Discovery

| Tool | Capability | Tested latency |
|---|---|---|
| `index` | Index or re-index a repo. One AST pass populates all three layers. xxh3 content-hash skips unchanged files. Concurrent per-file goroutines. | 190ms (3 files changed, 10 skipped) |
| `list` | All indexed projects with file/symbol/edge counts and last-indexed timestamp. | <1ms |
| `changes` | `git diff` → affected symbols → BFS blast radius. Returns changed symbols + impacted callers with CRITICAL/HIGH/MEDIUM/LOW risk labels. Scope: `unstaged` (default), `staged`, or `all`. | ~5ms |

### Symbol Retrieval

| Tool | Capability | Token savings |
|---|---|---|
| `symbol` | Source for one symbol by stable ID. O(1): 1 SQL + 1 `os.Seek` + 1 `os.Read`. No re-parse. Supports `fields` projection to return only selected columns. | File size − symbol size (real BPE) |
| `symbols` | Batch retrieve up to **100** symbols in one call. Hard cap: requests >100 IDs are rejected. Always prefer this over calling `symbol` in a loop. | Same per symbol |
| `context` | Symbol + all direct callees in one call. The preferred tool for understanding a function. | ~90% vs. reading files |

### Search & Graph

| Tool | Capability | Tested latency |
|---|---|---|
| `search` | FTS5 BM25 full-text across names, signatures, and docstrings. Wildcards (`auth*`), phrases (`"process order"`), AND/OR, `kind`/`language` filters. `fields` param projects columns to reduce token usage. `project=*` searches all indexed repos. | 1ms |
| `query` | Cypher-like graph queries. Three SQL paths: node scan, single-hop JOIN, variable-length BFS. `max_rows` param (default 200, max 10000). | 2ms (single-hop) |
| `trace` | BFS call-path trace — who calls this, or what does it call. Grouped by depth. Risk labels: CRITICAL (depth 1) → LOW (depth 4+). | <5ms (depth 3) |

### Architecture & Knowledge

| Tool | Capability | Tested latency |
|---|---|---|
| `architecture` | Language breakdown, entry points, hotspot functions (highest in-degree = highest change risk), graph stats. Start here on any unfamiliar project. | 12ms |
| `schema` | Node kind counts, edge kind counts, totals. Use before `query` to see what's indexed. | 1ms |
| `adr` | Persistent key/value store per project. Survives context resets and binary upgrades. Actions: `get`, `set`, `list`, `delete`. Use to record architectural decisions, known gotchas, or onboarding notes that outlive the conversation. | <1ms |
| `health` | Schema version, index staleness, per-language extraction coverage. Use to detect stale indexes. | 1ms |
| `stats` | Session savings as a formatted CLI summary: without-pincher baseline, with-pincher actual, tokens saved, cost avoided, avg latency. Persists across reconnects. | 8ms |
| `fetch` | Fetch a URL, extract its text, and store it as a searchable `Document` symbol in the project knowledge base. Use for API docs, READMEs, or specs you want to query later. Body cap: 512 KB fetched, 32 KB stored. Retrieve via `search kind:Document` or `symbol`. | ~200ms (network) |

### Stable Symbol IDs

Every symbol gets a human-readable ID that survives re-indexing:

```
"{file_path}::{qualified_name}#{kind}"

e.g.  "internal/db/db.go::db.Open#Function"
      "src/auth/jwt.ts::AuthService.verify#Method"
```

When a file is renamed, pincherMCP records a redirect in `symbol_moves`. The `symbol` tool resolves stale IDs transparently — agents never get "not found" because a file moved.

### Field Projection

The `search` and `symbol` tools accept a `fields` parameter — a comma-separated list of columns to return. Use it to cut token usage when you only need specific attributes:

```
fields="id,name,file_path"           # minimal — just locate the symbol
fields="id,name,signature,start_line" # enough to understand the interface
fields="id,name,source"              # name + full source, skip metadata
```

Available fields: `id`, `name`, `qualified_name`, `kind`, `language`, `file_path`, `start_line`, `end_line`, `signature`, `docstring`, `source`, `is_exported`, `extraction_confidence`

Omitting `fields` returns all columns (default behavior).

### Extraction Confidence

Every symbol carries an `extraction_confidence` score surfaced in search results and graph queries:

| Score | Parser | Languages |
|---|---|---|
| `1.0` | `go/ast` full AST / `gopkg.in/yaml.v3` Node tree | Go, YAML, JSON |
| `0.85` | Stable regex | Python, JavaScript, JSX, TypeScript, TSX, Rust, Java |
| `0.70` | Approximate regex | Ruby, PHP, C, C++, C#, Kotlin, Swift |

---

## <img src="docs/assets/crab.png" width="22" alt=""/> Cypher Query Reference

pincherMCP translates a Cypher subset to SQL at query time. All queries are scoped to one project.

```cypher
-- Node scan: all functions matching a regex
MATCH (f:Function) WHERE f.name =~ '.*Handler.*' RETURN f.name, f.file_path

-- Single-hop JOIN: what does main() call? (sub-ms)
MATCH (f:Function)-[:CALLS]->(g) WHERE f.name = 'main' RETURN g.name, g.file_path LIMIT 20

-- Variable-length BFS: call chains up to 3 hops
MATCH (a)-[:CALLS*1..3]->(b) WHERE a.name = 'ProcessOrder' RETURN b.name

-- Aggregation
MATCH (f:Function) RETURN COUNT(f) AS total

-- Named edge variables (access confidence, kind)
MATCH (a:Function)-[r:CALLS]->(b:Function) WHERE a.name = 'main'
RETURN a.name, r.kind, r.confidence, b.name

-- Ordering
MATCH (f:Function) WHERE f.file_path STARTS WITH 'internal/'
RETURN f.name, f.start_line ORDER BY f.start_line ASC

-- Filter by exported status
MATCH (f:Function) WHERE f.language = 'Go' AND f.is_exported = true
RETURN f.name, f.file_path LIMIT 50
```

**Supported operators:** `=`, `<>`, `>`, `<`, `>=`, `<=`, `=~` (regex), `CONTAINS`, `STARTS WITH`

**Supported clauses:** `WHERE`, `RETURN`, `ORDER BY`, `LIMIT`, `SKIP`, `COUNT()`

**Edge kinds indexed:** `CALLS`, `IMPORTS`. For Go, both edge kinds are resolved **across files** using a deferred project-wide pass — `Bar()` calling `Foo()` from a different file in the same module produces a real `CALLS` edge, not a dropped reference. `IMPORTS` is resolved against `Module` symbols using the `module` line of `go.mod` to rewrite intra-module paths; external imports stay unresolved. For other languages, `CALLS` and `IMPORTS` are scoped to within a single file (the per-file regex-extracted name table can't safely match across files without producing false positives).

**Node kinds indexed:** `Function`, `Method`, `Class` (and subtypes per language: `Interface`, `Struct`, `Trait`, `Type`), `Module` (one per Go file, qualified by within-module import path, e.g. `internal/db`), `Setting` (one per YAML/JSON key, qualified by dotted path, e.g. `services.web.image`), plus `Document` (URLs stored by the `fetch` tool)

---

## <img src="docs/assets/crab.png" width="22" alt=""/> Language Support

| Language | Extraction | Confidence | Symbol kinds extracted |
|---|---|---|---|
| Go | `go/ast` full AST | 1.0 | Functions, Methods, Types, Interfaces, Structs, Constants, Variables |
| YAML / JSON | `gopkg.in/yaml.v3` Node tree | 1.0 | Settings (dotted-path keys, sequence elements, multi-doc-aware) |
| Python | Regex | 0.85 | Functions, Classes, Methods |
| TypeScript / TSX | Regex | 0.85 | Functions, Classes, Interfaces, Methods |
| JavaScript / JSX | Regex | 0.85 | Functions, Classes, Methods |
| Rust | Regex | 0.85 | Functions, Structs, Traits, Impls |
| Java | Regex | 0.85 | Classes, Methods, Interfaces |
| Ruby | Regex | 0.70 | Functions, Classes, Methods |
| PHP | Regex | 0.70 | Functions, Classes, Methods |
| C / C++ | Regex | 0.70 | Functions, Structs, Classes |
| C# | Regex | 0.70 | Classes, Methods, Interfaces |
| Kotlin | Regex | 0.70 | Functions, Classes |
| Swift | Regex | 0.70 | Functions, Classes |

Files in Scala, Lua, Zig, Elixir, Haskell, Dart, Bash, and R are detected as source files but skipped — no extraction yet.

Go and YAML/JSON have full parser-based extraction (confidence 1.0). All other languages use regex patterns. The interface is stable: replace any language's extractor with tree-sitter bindings and confidence jumps to 1.0 with no other changes.

YAML/JSON files emit one `Setting` symbol per key with a dotted-path qualified name (e.g., `services.web.image`, `tasks.0.name`). Multi-document YAML uses a `docN` prefix. Each Setting's byte range covers the key plus its full nested value, so retrieving `services.web` returns the entire `web` block — the same shape as retrieving a function body.

### Skip rules

The indexer refuses to extract from files that are guaranteed to produce noise rather than signal, regardless of extension:

- **Lockfiles** by exact basename: `package-lock.json`, `npm-shrinkwrap.json`, `yarn.lock`, `pnpm-lock.yaml`, `bun.lock(b)`, `Cargo.lock`, `composer.lock`, `Gemfile.lock`, `Pipfile.lock`, `poetry.lock`, `uv.lock`, `pdm.lock`, `mix.lock`, `pubspec.lock`, `Podfile.lock`, `Cartfile.resolved`, `Package.resolved`, `flake.lock`, `go.sum`. Without this rule a 700 KB `package-lock.json` would emit thousands of low-signal `Setting` symbols.
- **Minified bundles** by suffix: `*.min.js`, `*.min.mjs`, `*.min.cjs`, `*.min.jsx`, `*.min.ts`, `*.min.tsx`, `*.min.css`.
- **Source maps** by suffix: `*.map`.

The skip count is reported in the indexer's structured log line as `blocked=N` and on `IndexResult.Blocked` for programmatic callers.

### Refusing obvious bloat traps

`pincher index <path>` refuses to walk paths that are statically known to produce noise rather than signal — exiting non-zero before any database write. This catches the case where a SessionStart hook fires from a parent shell pointed at the wrong directory:

- The user's home directory itself (`$HOME`)
- Common cache locations: `~/Library/Caches/*`, `~/.cache/*`, `%LOCALAPPDATA%\Temp\*`
- Language package roots: `~/go/pkg/*`, `~/.cargo/*`, `~/.npm/*`, `~/.gem/*`, `~/.rustup/*`
- Top-of-volume paths: `/`, `/usr`, `/var`, `/etc`, `/tmp`, `/private/tmp`

The MCP `index` tool goes through the same guard, so the protection applies whether `pincher index` is invoked from the CLI or via Claude Code.

### Cross-process safety

Multiple pincher processes can safely share one data directory. Each `Index()` run acquires a per-project filesystem lockfile (`<dataDir>/locks/<project-id-hash>.lock`) before touching the database; concurrent indexers on the same project block at the file level instead of fighting over the SQLite WAL. Stale lockfiles are reclaimed automatically when (a) the holder PID is no longer alive, (b) the lock is older than 24 hours, or (c) the payload is corrupt. This is what keeps a manual `pincher index` and a Claude Code SessionStart hook from racing each other.

---

## <img src="docs/assets/crab.png" width="22" alt=""/> HTTP REST API

All 15 tools are available via `POST /v1/{tool}` with a JSON body. Run alongside MCP stdio — no either/or.

```bash
# Start with both transports
pincher --http :8080 --http-key mysecrettoken

# Index a repo
curl -s -X POST http://localhost:8080/v1/index \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer mysecrettoken" \
  -d '{"path": "/path/to/your/project"}' | jq .

# Search with field projection (fewer tokens)
curl -s -X POST http://localhost:8080/v1/search \
  -H "Content-Type: application/json" \
  -H "Accept-Encoding: gzip" \
  -d '{"query": "processPayment", "project": "myproject", "fields": "id,name,file_path"}' | jq .

# Cross-repo search
curl -s -X POST http://localhost:8080/v1/search \
  -d '{"query": "auth*", "project": "*"}' | jq .

# Cypher graph query
curl -s -X POST http://localhost:8080/v1/query \
  -H "Content-Type: application/json" \
  -d '{"cypher": "MATCH (f:Function)-[:CALLS]->(g) WHERE f.name = '\''main'\'' RETURN g.name LIMIT 10", "project": "myproject"}' | jq .

# Liveness probe — no auth required
curl http://localhost:8080/v1/health

# OpenAPI spec (Postman / Cursor importable)
curl http://localhost:8080/v1/openapi.json | jq .
```

Responses compress ~65% with `Accept-Encoding: gzip`.

**Tested clients:** curl, Python `requests`, PowerShell `Invoke-WebRequest`

**Rate limiting:** `--http-rate 60` limits to 60 requests/IP/minute (0 = unlimited).

### Additional HTTP endpoints

Beyond `POST /v1/{tool}`, the HTTP server exposes:

| Endpoint | Method | Auth | Description |
|---|---|---|---|
| `/v1/health` | GET | No | Liveness probe — schema version, index staleness. Always returns 200. |
| `/v1/dashboard` | GET | No | Self-contained HTML dashboard — stats, search, project cards, sparkline of last 90 sessions. No external dependencies. |
| `/v1/openapi.json` | GET | No | OpenAPI 3.1 spec covering all 15 tool endpoints. Import into Postman or Cursor. |
| `/v1/stats` | GET | Yes | Current session + all-time savings summary as JSON. |
| `/v1/sessions` | GET | Yes | Per-session history, last 90 sessions, sorted by recency. |
| `/v1/projects` | GET | Yes | All indexed projects with file/symbol/edge counts. |
| `/v1/projects` | DELETE | Yes | Remove a project and all its symbols. Body: `{"id":"<project-id>"}`. |
| `/v1/index-progress` | POST | Yes | Live indexing progress for the given project: `{files_done, files_total, active}`. Useful for progress bars in dashboards. |

**CORS:** All responses include `Access-Control-Allow-Origin: *` — the API is callable directly from browsers and web clients without a proxy.

---

<div align="center">
  <img src="docs/assets/pinchy.png" alt="Pinchy holding a copper penny" width="140"/>
  <p><em>Pinchy's day job.</em></p>
</div>

## <img src="docs/assets/crab.png" width="22" alt=""/> Token Savings

Token counts use the **cl100k_base BPE tokenizer** (same family as Claude) loaded as an embedded Go dependency — no network calls, zero latency after first initialization. Cost is estimated at **$3.00 per 1M tokens** (Claude Sonnet pricing).

The `stats` tool renders a formatted session summary directly in the chat window:

```
┌────────────────────────────────────────────┐
│                  SESSION                   │
│  Tool calls:          5                   │
│  Without pincher:   ~45,200 tokens        │
│  With pincher:        1,200 tokens        │
│  Saved:             ~44,000 tokens  37x   │
│  Cost avoided:        $0.1320             │
│  Avg latency:         2 ms                │
└────────────────────────────────────────────┘
```

**Without pincher** is the estimated baseline — what an agent would spend reading whole files to find the same information. It uses actual `os.Stat` file sizes for retrieval tools (`symbol`, `context`, `search`, `trace`) and a conservative `symbol_count × 20,000 chars / 4` estimate for graph tools (`architecture`, `query`).

**With pincher** is the actual token count of what pincherMCP returned (real BPE, not a heuristic).

**Saved** is the difference, with a `~` to indicate the baseline is estimated. The multiplier (`37x`) is the headline ratio.

Savings persist in SQLite across reconnects, process restarts, and binary upgrades.

**Typical per-call savings:**

| Tool | Baseline | Typical savings |
|---|---|---|
| `symbol` | Whole file read | ~95% |
| `context` | File + all imports | ~90% |
| `search` | Grep-then-read cycle | ~98% |
| `architecture` | Reading every file to orient | ~99.99% |
| `trace` | Manual call-chain traversal | ~99% |

---

## <img src="docs/assets/crab.png" width="22" alt=""/> Installation

### Requirements

- Go 1.24+ (pure Go — no CGO, no C compiler) — only needed if building from source
- Git (for the `changes` blast-radius tool)

### Managed installs

Drop-in service templates and install scripts live under [`packaging/`](packaging/README.md):

- **Homebrew** — tap + formula at `packaging/homebrew/pincher.rb`
- **Linux systemd** — user unit at `packaging/systemd/pincher.service`
- **macOS launchd** — LaunchAgent at `packaging/launchd/com.pinchermcp.pincher.plist`
- **Windows service** — PowerShell installer at `packaging/windows/install-service.ps1`
- **Docker** — `Dockerfile` at repo root; multi-arch image published to `ghcr.io/kwad77/pinchermcp` on every release

### Build from source

```bash
git clone https://github.com/kwad77/pincherMCP
cd pincherMCP
go build -o pincher ./cmd/pinch/         # Linux/macOS
go build -o pincher.exe ./cmd/pinch/     # Windows
```

### Claude Code

Edit `~/.claude/mcp.json`:

```json
{
  "mcpServers": {
    "pincher": {
      "type": "stdio",
      "command": "/path/to/pincher"
    }
  }
}
```

### Claude Desktop

Edit `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "pincher": {
      "type": "stdio",
      "command": "/path/to/pincher"
    }
  }
}
```

### Data directory

| Platform | Default location |
|---|---|
| Windows | `%APPDATA%\pincherMCP\pincher.db` |
| macOS | `~/Library/Application Support/pincherMCP/pincher.db` |
| Linux | `~/.local/share/pincherMCP/pincher.db` |

Override with `--data-dir /custom/path`. Back up with any file copy tool.

### CLI flags

```
pincher --version                    # print version and exit
pincher --data-dir /custom/path      # override database directory
pincher --verbose                    # enable verbose logging to stderr
pincher --http :8080                 # also listen for HTTP REST on :8080
pincher --http :0                    # let the OS pick a free port (logged on startup)
pincher --http-key mysecrettoken     # require bearer token on all HTTP requests
pincher --http-rate 60               # rate limit: 60 requests/IP/minute (0 = unlimited)
```

### Environment variables

Used when the matching flag is empty — convenient for Docker, systemd, and launchd.

| Variable | Equivalent flag | Example |
|---|---|---|
| `PINCHER_HTTP_ADDR` | `--http` | `PINCHER_HTTP_ADDR=:9000 pincher` |
| `PINCHER_HTTP_KEY` | `--http-key` | `PINCHER_HTTP_KEY=secret pincher --http :8080` |

`PINCHER_HTTP_ADDR=:0` picks a free port and the bound address is printed to stderr at startup (`pincherMCP: HTTP listening on http://localhost:59726`). The Docker image sets `PINCHER_HTTP_ADDR=:8080` by default — override with `docker run -e PINCHER_HTTP_ADDR=:9000 -p 9000:9000 ghcr.io/.../pinchermcp`.

### `pincher index` subcommand

`pincher index` runs a one-shot index without starting an MCP server — useful in CI, pre-commit hooks, or as a Claude Code SessionStart hook:

```bash
pincher index                        # index current directory (plain text output)
pincher index /path/to/repo          # index a specific path
pincher index --force                # re-parse all files, ignore content hashes
pincher index --hook                 # emit Claude Code SessionStart JSON envelope
pincher index --data-dir /custom     # override data directory
```

The `--hook` flag outputs a JSON envelope that Claude Code's SessionStart hook system injects as `additionalContext`, telling Claude which project is indexed and whether uncommitted changes exist. Configure it in `.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      { "type": "command", "command": "pincher index --hook" }
    ]
  }
}
```

---

## <img src="docs/assets/crab.png" width="22" alt=""/> Performance

Measured on this codebase (13 files, 618 symbols, 5,785 edges, Windows 11, SQLite WAL):

| Operation | Measured time | Notes |
|---|---|---|
| Cold index (13 files) | ~190ms | Concurrent goroutines, xxh3 hash |
| Incremental re-index (0 changes) | <5ms | All files skipped via hash |
| `architecture` | 12ms server / 69ms HTTP | Was 10s+ before savings-calc fix |
| `search` | 1ms | BM25 via FTS5 |
| `health` | 1ms | |
| `stats` | 8ms | |
| `symbol` (byte-offset seek) | <1ms | 1 SQL + 1 seek + 1 read |
| Single-hop Cypher query | 2ms | SQL JOIN |
| BFS depth 3 | <5ms | Go BFS over CTE |
| Session stats flush | every 10s | Background goroutine |

**SQLite configuration:** WAL mode, `busy_timeout=5000ms`, `SetMaxOpenConns(1)` (serialized single-writer). Readers never block writers in WAL mode.

**WAL bounding:** `journal_size_limit=256 MiB` caps the WAL; `PRAGMA wal_checkpoint(TRUNCATE)` runs at the tail of each `Index()` run to fold the WAL back into the main DB at the natural quiet point. `PRAGMA optimize` runs on the same cadence to refresh query-planner stats. These are the WAL guardrails added after the 70 GB WAL incident produced by an unbounded multi-writer storm — the bound holds even under heavy churn.

**Watch backoff:** the file-change watcher's 5-second tick body short-circuits when any `Index()` is in flight for any project. During large catch-up phases the watcher idles at near-zero CPU instead of bouncing repeatedly off the per-project mutex.

---

## <img src="docs/assets/crab.png" width="22" alt=""/> Roadmap

### v0.2 — Parser accuracy
- **Tree-sitter bindings for Python, TypeScript, Rust, Java** — extraction confidence 1.0 for the four most common non-Go languages; no CGO required via the pure-Go tree-sitter port
- **IMPORTS edge kind** — track `import` / `require` / `use` statements as graph edges, enabling cross-file dependency queries
- **Struct field extraction** — index individual fields/properties as symbols (currently only types/classes are indexed, not their members)

### v0.3 — Multi-project & team use
- **Cross-project `query`** — today `query` is scoped to one project; v0.3 adds an optional `project=*` mode matching `search`
- **Shared index server mode** — one `pincher --http :8080` instance with a `--data-dir` on a network path serves a whole team; MCP clients point at it instead of running a local binary
- **Webhook-triggered re-index** — `POST /v1/reindex` endpoint callable from git post-receive hooks; replaces the 2s polling loop for server deployments

### v0.4 — Enterprise
- **PostgreSQL backend** — drop-in replacement for the SQLite store; enables multi-writer, HA deployments; query API unchanged
- **Role-based access** — per-project API keys with read/write/admin scopes
- **Audit log** — append-only log of every tool call with caller identity, timestamp, and latency

### v1.0 — Stable API
- API stability guarantee: no breaking changes to tool schemas or symbol ID format
- Pre-built binaries for Linux/macOS/Windows (amd64 + arm64) on every release
- Docker image: `ghcr.io/kwad77/pinchermcp:latest` (~12MB scratch image)
- VS Code extension: auto-configures MCP JSON, adds hover-to-inspect command

---

## <img src="docs/assets/crab.png" width="22" alt=""/> Development

### HTTP dashboard

`GET /v1/dashboard` serves a self-contained HTML/CSS/JS page — no CDN, no external requests. Features:

- **Stats tab** — session card (calls, tokens_used, tokens_saved, cost_avoided), all-time totals, sparkline of last 90 sessions
- **Search tab** — live FTS5 search across all indexed projects, results with file path and line numbers
- **Projects tab** — per-project cards (files, symbols, edges, last indexed, stale/invalid detection), delete button, live index-progress bar during re-indexing

Authentication: the dashboard itself requires no bearer token (it's a browser page), but the JS it loads calls authenticated endpoints using the token configured at startup.

### Project layout

```
pincherMCP/
├── cmd/pinch/
│   ├── main.go                  # Sole entry point: MCP server + `pincher index` CLI subcommand
│   └── bloat_trap.go            # IsBloatTrap: refuse to index home dirs, caches, package roots
├── internal/
│   ├── db/db.go                 # SQLite store: schema v6, migrations, all CRUD,
│   │                            # FTS5 ops, graph ops, BPE token counting,
│   │                            # WAL guardrails (Optimize, CheckpointTruncate)
│   ├── ast/
│   │   ├── extractor.go         # Multi-language extraction, byte offsets, confidence
│   │   ├── yaml.go              # YAML/JSON Setting extractor (yaml.v3 Node tree, conf 1.0)
│   │   ├── blocklist.go         # ShouldSkip: lockfiles, minified bundles, source maps
│   │   └── languages.go         # Extension → language detection
│   ├── cypher/engine.go         # Cypher → SQL: tokenizer → parser → 3 query paths
│   ├── index/
│   │   ├── indexer.go           # Index pipeline: walk → hash → extract → resolve → store → watch
│   │   └── lockfile.go          # Cross-process project lockfile with stale-holder reclaim
│   └── server/server.go         # 15 MCP tools, HTTP REST, gzip, OpenAPI 3.1, bearer auth,
│                                # basepath / reverse-proxy support,
│                                # session persistence, token savings accounting
└── go.mod
```

### Schema

Schema is versioned via `schema_version` table. Current version: **v6**. Migrations apply automatically on startup — no data loss, no manual steps. To add a migration: append a SQL string to `schemaMigrations` in `db.go`.

### Key invariants

- `SetMaxOpenConns(1)` — SQLite is single-writer; all writes serialize at the pool
- WAL mode — readers never block writers; 5s busy timeout prevents immediate failure during indexing
- `journal_size_limit=256 MiB` + `wal_checkpoint(TRUNCATE)` at every `Index()` tail — keeps the WAL bounded under heavy churn
- Cross-process project lockfile — multiple pincher binaries on one data directory serialize safely; stale-holder reclaim covers crashed processes
- File re-parse always deletes the file's prior symbols before re-extraction — no stale rows leak; cascades to edges with either endpoint in the file
- FTS5 triggers (`sym_fts_insert`, `sym_fts_delete`, `sym_fts_update`) auto-sync `symbols_fts` — never manually sync it
- The generated `symbol_id` column on `symbols` mirrors `id` so FTS5 content lookups against the FTS column name work; never write to `symbol_id` directly
- `symSelectFrom` and `symRow` (in `cypher/engine.go`) must stay in sync when adding columns
- Batch flush at 500 symbols or 1,000 edges to bound memory on large repos

### Test coverage

```bash
go test ./...                                              # run all tests
go test ./... -coverprofile=cover.out                      # with coverage
go tool cover -func=cover.out | grep "^total"              # total: 84.0%
go test ./internal/db/ -run TestGraphStats_WithData -v     # single test
go test ./internal/server/ -v                              # server package
```

Current coverage by package:

| Package | Coverage |
|---|---|
| `internal/ast` | 98.5% |
| `internal/cypher` | 93.7% |
| `internal/index` | 86.7% |
| `internal/db` | 85.0% |
| `internal/server` | 80.7% |
| **total** | **84.0%** |

The `internal/server` number is dragged down by `ListenAndServeHTTP`, `handleFetch`, `extractTextFromHTML`, and the MCP `onInit`/`onRoots`/`detectRoot` callbacks — network/runtime code that needs integration-style tests. The CI gate is set to 83%.

### Dependencies

| Dependency | Purpose |
|---|---|
| `github.com/modelcontextprotocol/go-sdk v1.4.0` | MCP server (JSON-RPC 2.0 over stdio) |
| `modernc.org/sqlite v1.34.5` | Pure-Go SQLite (no CGO) |
| `github.com/tiktoken-go/tokenizer v0.7.0` | cl100k_base BPE tokenizer — real token counts |
| `github.com/boyter/gocodewalker v1.5.1` | File walker that respects `.gitignore` |
| `github.com/zeebo/xxh3 v1.1.0` | Fast content hashing for incremental indexing |

---

## License

MIT

<div align="center">
  <img src="docs/assets/crab.png" width="32" alt="Pinchy"/>
</div>
