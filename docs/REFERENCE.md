# pincherMCP Reference

The long-form reference. The [README](../README.md) is the pitch + quickstart; this file is the manual. For 10-minute end-to-end walkthroughs, see [`tutorials/`](tutorials/) — [Claude Code](tutorials/claude-code.md), [Cursor](tutorials/cursor.md), [HTTP dashboard](tutorials/http-dashboard.md).

**Schema version:** v25 · **MCP tools:** 22 · **Languages detected:** ~25 (10 AST/parser-tier, 15 regex-tier, plus 7 stub-tier with no extractor — see [Language support](#language-support))

## Contents

- [Architecture](#architecture)
  - [Two-process architecture](#two-process-architecture)
  - [Three-layer storage](#three-layer-storage)
  - [pinchQL query routing](#pinchql-query-routing)
  - [Data flow: index to query](#data-flow-index-to-query)
- [The 22 MCP tools](#the-22-mcp-tools)
  - [Stable symbol IDs](#stable-symbol-ids)
  - [Field projection](#field-projection)
  - [Extraction confidence](#extraction-confidence)
- [pinchQL query reference](#pinchql-query-reference)
- [Language support](#language-support)
  - [Skip rules](#skip-rules)
  - [Refusing obvious bloat traps](#refusing-obvious-bloat-traps)
  - [Cross-process safety](#cross-process-safety)
- [HTTP REST API](#http-rest-api)
- [CLI subcommands](#cli-subcommands)
  - [`pincher index`](#pincher-index)
  - [`pincher doctor`](#pincher-doctor)
  - [`pincher self-test`](#pincher-self-test)
  - [`pincher rebuild-fts`](#pincher-rebuild-fts)
  - [`pincher update`](#pincher-update)
  - [`pincher web`](#pincher-web)
  - [`pincher init`](#pincher-init)
  - [`pincher project`](#pincher-project)
- [CLI flags](#cli-flags)
- [Environment variables](#environment-variables)
- [Data directory](#data-directory)
- [Performance](#performance)
- [Schema](#schema)
- [Key invariants](#key-invariants)
- [Project layout](#project-layout)
- [Test coverage](#test-coverage)
- [Dependencies](#dependencies)
- [Roadmap](#roadmap)
- [Known limitations](#known-limitations)

---

## Architecture

### Two-process architecture

```
  Claude Code (IDE)
        │
        │ JSON-RPC 2.0 (stdio)
        ▼
┌───────────────────────┐          ┌───────────────────────────┐
│  pincher (MCP process)│          │  pincher --http :8080     │
│                       │          │  (dashboard / REST)       │
│  • 22 MCP tools       │          │                           │
│  • idx.Watch()        │          │  • POST /v1/{tool}        │
│  • SessionFlusher     │          │  • GET /v1/dashboard      │
│    (flush every 10 s) │          │  • GET /v1/openapi.json   │
│                       │          │  • GET /v1/sessions       │
│                       │          │  • DELETE /v1/projects    │
└──────────┬────────────┘          └───────────┬───────────────┘
           │                                   │
           │     Both share the same SQLite file
           └─────────────┬─────────────────────┘
                         ▼
             ┌─────────────────────┐
             │  SQLite WAL         │
             │  pincher.db         │
             │                     │
             │  • symbols          │
             │  • edges            │
             │  • symbols_fts +    │
             │    per-corpus FTS5  │
             │  • projects         │
             │  • sessions         │
             │  • symbol_moves     │
             │  • adr_entries      │
             │  • schema_version   │
             └─────────────────────┘
```

The HTTP process retries port binding for up to 10 seconds on startup — reconnecting the MCP process (which briefly holds the port) doesn't break the dashboard. `pincher web` discovers the bound URL via the `sessions.http_url` column added in schema v11; PID liveness check covers stale rows.

### Three-layer storage

All three layers populate in **one AST parse pass** from one `symbols` row.

```
                         Source File
                              │
                         ast.Extract()
                              │
               ┌──────────────┴──────────────┐
               │         symbols row         │
               │  id · file_path · name      │
               │  start_byte · end_byte      │
               │  kind · language · parent   │
               │  signature · docstring      │
               │  complexity · is_exported   │
               │  extraction_confidence      │
               └──────┬──────────┬───────────┘
                      │          │
          ┌───────────┘          └──────────────┐
          ▼                                     ▼
  ┌───────────────┐    ┌──────────────┐   ┌────────────────────┐
  │  Layer 1      │    │  Layer 2     │   │  Layer 3 — FTS5    │
  │  Byte-Offset  │    │  Knowledge   │   │  BM25 full-text    │
  │  Symbol Store │    │  Graph       │   │                    │
  │               │    │              │   │  symbols_fts       │
  │  start_byte   │    │  symbols +   │   │   (legacy/all)     │
  │  end_byte     │    │  edges table │   │  symbols_code_fts  │
  │               │    │              │   │  symbols_config_fts│
  │  Retrieval:   │    │  Queries:    │   │  symbols_docs_fts  │
  │  1 SQL +      │    │  node scan   │   │                    │
  │  1 os.Seek +  │    │  JOIN (1-hop)│   │  BM25 across name +│
  │  1 os.Read    │    │  BFS (n-hop) │   │  signature +       │
  │               │    │  via CTE     │   │  docstring; corpus=│
  │  O(1), <1ms   │    │  <2ms        │   │  routes per index  │
  └───────────────┘    └──────────────┘   └────────────────────┘
```

**Per-corpus FTS5** (#32 ✅): one symbol → one corpus. Routing rules: `language IN ('YAML','JSON','HCL','TOML')` → config; `Markdown` or `kind=Document` → docs; everything else → code. The `search` tool's `corpus` parameter routes to the right index. **Default is `code`** — the most common search is for an identifier. Pass `corpus=config` for YAML/JSON/HCL/TOML settings, `corpus=docs` for Markdown / fetched Documents, or `corpus=all` to hit the legacy mixed index (deprecated, slated for removal).

**Per-symbol confidence** (#34 ✅): `extraction_confidence` is composed from BaseExtractor + KindBaseline + PathPenalty + IdentBonus + GeneratedPen, clamped to `[0, 1]`. Lockfile keys score ~0.4–0.6, vendored Go ~0.7, real config ~0.95–1.0. `search` accepts `min_confidence` and **defaults to 0.7**. Every search response carries `_meta.confidence_distribution` (4-bucket histogram).

### pinchQL query routing

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

Project-scoped paths — `search`, `symbol`/`symbols` when `project=` is passed, `query`, `trace`, `changes` — apply a `project_id` filter at lookup and BFS traversal time, so cross-project data is structurally inaccessible from those paths.

### Data flow: index to query

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
   └── FTS5 triggers auto-sync symbols_fts + per-corpus

  idx.Watch() polls every 2 s (active) or 30 s (idle)
  and re-runs Index() on changed files incrementally.
  No manual re-index required during a session.

  On file move: (qualified_name, kind) match detected →
  symbol_moves redirect recorded → handleSymbol resolves
  stale IDs transparently via store.ResolveStaleID()
```

---

## The 22 MCP tools

All latencies measured on this codebase. Token counts use cl100k_base BPE — the same tokenizer family as Claude.

### Starter

| Tool | Capability | Tested latency |
|---|---|---|
| `guide` | Free-form task description (`"fix login retry bug"`, `"refactor auth middleware"`) returns 2–3 recommended pincher tool calls with reasoning. Removes decision friction at session start. Keyword classifier; no model. | <1 ms |

### Indexing & discovery

| Tool | Capability | Tested latency |
|---|---|---|
| `index` | Index or re-index a repo. One AST pass populates all three layers. xxh3 content-hash skips unchanged files. Concurrent per-file goroutines. | 190 ms (3 changed, 10 skipped) |
| `list` | All indexed projects with file/symbol/edge counts and last-indexed timestamp. | <1 ms |
| `changes` | `git diff` → affected symbols → BFS blast radius. Returns changed symbols + impacted callers with CRITICAL/HIGH/MEDIUM/LOW risk labels. Scope: `unstaged` (default), `staged`, `all`. | ~5 ms |

### Symbol retrieval

| Tool | Capability | Token savings |
|---|---|---|
| `symbol` | Source for one symbol by stable ID. O(1): 1 SQL + 1 `os.Seek` + 1 `os.Read`. No re-parse. Supports `fields` projection. | File size − symbol size (real BPE) |
| `symbols` | Batch retrieve up to **100** symbols in one call. Hard cap: requests >100 IDs are rejected. Always prefer this over calling `symbol` in a loop. | Same per symbol |
| `context` | Symbol + all direct callees in one call. The preferred tool for understanding a function. | ~90% vs reading files |

### Search & graph

| Tool | Capability | Tested latency |
|---|---|---|
| `search` | FTS5 BM25 across names, signatures, docstrings. Wildcards (`auth*`), phrases (`"process order"`), AND/OR. `kind`/`language`/`corpus` filters. `corpus` defaults to `code`; pass `config` for YAML/JSON/HCL settings, `docs` for Markdown / Documents, `all` for the legacy index. `fields` projects columns. `project=*` searches all repos. | 1 ms |
| `query` | pinchQL graph queries — Cypher-shaped subset. Three SQL paths: node scan, single-hop JOIN, variable-length BFS. `max_rows` (default 200, max 10000). Parameter: `pinchql` (legacy alias `cypher` accepted for one release). | 2 ms (single-hop) |
| `trace` | BFS call-path trace — who calls this, or what does it call. Grouped by depth. Risk labels: CRITICAL (depth 1) → LOW (depth 4+). | <5 ms (depth 3) |

### Architecture & knowledge

| Tool | Capability | Tested latency |
|---|---|---|
| `architecture` | Language breakdown, entry points, hotspot functions, graph stats. Start here on any unfamiliar project. | 12 ms |
| `schema` | Node kind counts, edge kind counts, totals. Use before `query` to see what's indexed. | 1 ms |
| `adr` | Persistent key/value store per project. Survives context resets and binary upgrades. Actions: `get`, `set`, `list`, `delete`. | <1 ms |
| `health` | Schema version, index staleness, per-language extraction coverage. Detects stale indexes. | 1 ms |
| `stats` | Session savings as a formatted CLI summary. Persists across reconnects. | 8 ms |
| `fetch` | Fetch a URL, extract its text, store as a searchable `Document` symbol in the project knowledge base. Body cap: 512 KB fetched, 32 KB stored. Retrieve via `search kind:Document` or `symbol`. | ~200 ms (network) |

### Stable symbol IDs

```
"{file_path}::{qualified_name}#{kind}"

e.g.  "internal/db/db.go::db.Open#Function"
      "src/auth/jwt.ts::AuthService.verify#Method"
```

When a file is renamed, pincher records a redirect in `symbol_moves`. `symbol` resolves stale IDs transparently via `store.ResolveStaleID()` — agents never get "not found" because a file moved.

### Field projection

The `search` and `symbol` tools accept a `fields` parameter — a comma-separated list of columns to return. Use it to cut token usage when you only need specific attributes.

```
fields="id,name,file_path"            # minimal — just locate the symbol
fields="id,name,signature,start_line" # enough to understand the interface
fields="id,name,source"               # name + full source, skip metadata
```

Available fields: `id`, `name`, `qualified_name`, `kind`, `language`, `file_path`, `start_line`, `end_line`, `signature`, `docstring`, `source`, `is_exported`, `extraction_confidence`. Omitting `fields` returns all columns.

### Extraction confidence

Every symbol carries an `extraction_confidence` score surfaced in search results and graph queries.

| Score | Parser | Languages |
|---|---|---|
| `1.0` | `go/ast` / `yaml.v3` / `mvdan.cc/sh/v3` / `hashicorp/hcl/v2/hclsyntax` / `BurntSushi/toml` / `yuin/goldmark` / `nikolalohinski/gonja` | Go, YAML, JSON, Bash, HCL/Terraform, TOML, Markdown, Jinja2 |
| `0.85` | Stable regex | Python, JavaScript, JSX, TypeScript, TSX, Rust, Java, Makefile, SQL |
| `0.70` | Approximate regex | Ruby, PHP, C, C++, C#, Kotlin, Swift |

---

## pinchQL query reference

pincher's graph-query language is **pinchQL** — a Cypher-shaped pragmatic subset that translates to SQL at query time. The grammar below is the contract; anything outside it is unsupported. All queries are scoped to one project.

> **Why "pinchQL" and not "Cypher"?** Real Cypher (the Neo4j query language) is a moving target with hundreds of features pincher doesn't implement and won't. Calling our subset "Cypher-like" set a maintenance backlog of forever-pending features. pinchQL is honest about scope: what's documented below is what works, full stop. The MCP `query` tool's `pinchql` parameter is the new canonical name; the `cypher` parameter name is still accepted as a soft alias for one release to ease transition. Decided in #206.

```pinchql
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

**Edge kinds indexed:** `CALLS`, `IMPORTS`, `REFERENCES` (for HCL `var.NAME` references). For Go, `CALLS` and `IMPORTS` are resolved **across files** via a deferred project-wide pass — `Bar()` calling `Foo()` from a different file in the same module produces a real `CALLS` edge. `IMPORTS` is resolved against `Module` symbols using `go.mod` to rewrite intra-module paths. For other languages, `CALLS`/`IMPORTS` are scoped to a single file (the per-file regex name table can't safely match across files without false positives).

**Node kinds indexed:** `Function`, `Method`, `Class` (and per-language subtypes: `Interface`, `Struct`, `Trait`, `Type`), `Module` (one per Go file or Terraform `module` block), `Variable` (also covers Terraform `variable` blocks as `var.NAME`), `Setting` (one per YAML/JSON/HCL `.tfvars`/TOML key, dotted-path qualified), Terraform-specific `Resource` / `DataSource` / `Output` / `Local` / `Provider`, `Block` (nested HCL blocks of any depth), `Section` (Markdown headings), `Document` (URLs stored by the `fetch` tool).

---

## Language support

| Language | Extraction | Confidence | Symbol kinds extracted |
|---|---|---|---|
| Go | `go/ast` full AST | 1.0 | Functions, Methods, Types, Interfaces, Structs, Constants, Variables |
| YAML / JSON | `gopkg.in/yaml.v3` Node tree | 1.0 | Settings (dotted-path keys, sequence elements, multi-doc-aware). Ansible-aware `RENDERS` edges for `template: src:`. |
| Bash | `mvdan.cc/sh/v3/syntax` (the `shfmt` parser) | 1.0 | Functions (POSIX `name() { … }` and reserved-word `function name { … }`; `.sh`, `.bash`) |
| HCL / Terraform | `github.com/hashicorp/hcl/v2/hclsyntax` | 1.0 | Resources, DataSources, Modules, Variables, Outputs, Locals, Providers, plus `Block` for nested `lifecycle` / `provisioner` / `connection` / `dynamic` / `backend` / `required_providers`. `.tfvars` assignments emit `Setting`. `var.NAME` references emit `REFERENCES` edges. Covers `.tf`, `.tfvars`. |
| TOML | `github.com/BurntSushi/toml` parseability gate + structural source-walk | 1.0 | One `Setting` per section header and per key assignment with dotted qualified names. Array-of-tables indexed as `name.0`, `name.1`. Multi-line strings/arrays span their full body. Covers `.toml`. |
| Markdown | `github.com/yuin/goldmark` CommonMark | 1.0 | One `Section` symbol per heading. Hierarchical dotted-path qualified name (`intro.getting_started.installation`). Each Section's byte range covers its full body. Covers `.md`, `.markdown`, `.mdx`, `.mdc`. |
| Jinja2 | `github.com/nikolalohinski/gonja` parser | 1.0 | `{% macro %}` → Function, `{% block %}` → Block, `{% set %}` → Setting, `{% extends/include/import/from %}` → IMPORTS edges. 2-second per-file parse timeout protects against gonja lexer hangs on truncated input. Covers `.j2`, `.jinja`, `.jinja2`. |
| Python | Regex | 0.85 | Functions, Classes, Methods |
| TypeScript / TSX | Regex | 0.85 | Functions, Classes, Interfaces, Methods |
| JavaScript / JSX | Regex | 0.85 | Functions, Classes, Methods |
| Rust | Regex | 0.85 | Functions, Structs, Traits, Impls |
| Java | Regex | 0.85 | Classes, Methods, Interfaces |
| Makefile | Regex | 0.85 | Rule targets → Function (`.PHONY` → `IsExported=true`), variable assignments → Setting. Detected by basename (`Makefile`, `GNUmakefile`, lowercase `makefile`) + extension (`.mk`, `.mak`). |
| SQL | Regex | 0.85 | `CREATE TABLE`/`VIEW` → Class; `CREATE FUNCTION`/`PROCEDURE`/`TRIGGER` → Function (handles `IF NOT EXISTS`). Schema prefix split into `qualified_name` (`auth.users`) with bare `name` (`users`). Dialect-aware quoting (backticks/quotes/brackets). Comment-aware. Covers `.sql`, `.ddl`. |
| Ruby | Regex | 0.70 | Functions, Classes, Methods |
| PHP | Regex | 0.70 | Functions, Classes, Methods |
| C / C++ | Regex | 0.70 | Functions, Structs, Classes |
| C# | Regex | 0.70 | Classes, Methods, Interfaces |
| Kotlin | Regex | 0.70 | Functions, Classes |
| Swift | Regex | 0.70 | Functions, Classes |

Files in **Scala, Lua, Zig, Elixir, Haskell, Dart, R** are detected as source files but skipped — no extraction yet. The `Extractor` interface is stable: adding any of them is one file.

YAML/JSON files emit one `Setting` symbol per key with a dotted-path qualified name (e.g. `services.web.image`, `tasks.0.name`). Multi-document YAML uses a `docN` prefix. Each Setting's byte range covers the key plus its full nested value, so retrieving `services.web` returns the entire `web` block.

### Skip rules

The indexer refuses to extract from files that are guaranteed to produce noise rather than signal, regardless of extension:

- **Lockfiles** by exact basename: `package-lock.json`, `npm-shrinkwrap.json`, `yarn.lock`, `pnpm-lock.yaml`, `bun.lock(b)`, `Cargo.lock`, `composer.lock`, `Gemfile.lock`, `Pipfile.lock`, `poetry.lock`, `uv.lock`, `pdm.lock`, `mix.lock`, `pubspec.lock`, `Podfile.lock`, `Cartfile.resolved`, `Package.resolved`, `flake.lock`, `go.sum`. Without this rule a 700 KB `package-lock.json` would emit thousands of low-signal `Setting` symbols.
- **Minified bundles** by suffix: `*.min.js`, `*.min.mjs`, `*.min.cjs`, `*.min.jsx`, `*.min.ts`, `*.min.tsx`, `*.min.css`.
- **Source maps** by suffix: `*.map`.

Per-symbol confidence (#34) carries the gradient for everything else (vendor/, README, generated markers); the static blocklist is preserved as a hard pre-filter only for files where extraction would be wasted work.

The skip count is reported in the indexer's structured log line as `blocked=N` and on `IndexResult.Blocked` for programmatic callers.

### Refusing obvious bloat traps

`pincher index <path>` refuses two catastrophic targets in any mode — the filesystem root (`/` on Linux/macOS, `C:\` on Windows, detected as any path that is its own parent) and the user's home directory (`$HOME` / `%USERPROFILE%`, with symlinks resolved). Either mistake walks tens of GB of cache and package data and was the cause of the 70 GB WAL incident this guard addresses.

In **hook mode** (`pincher index --hook`), the guard tightens further: the target directory must contain at least one project marker (`.git`, `.hg`, `.svn`, `go.mod`, `package.json`, `pyproject.toml`, `Cargo.toml`, `Gemfile`, `pom.xml`, `build.gradle`, `build.gradle.kts`, `Makefile`, `CMakeLists.txt`). Manual `pincher index <path>` skips the marker check — explicit user action is treated as authoritative for any non-catastrophic path. The MCP `index` tool path goes through the same guard.

### Cross-process safety

Multiple pincher processes can safely share one data directory. Each `Index()` run acquires a per-project filesystem lockfile (`<dataDir>/locks/<project-id-hash>.lock`) before touching the database; concurrent indexers on the same project block at the file level instead of fighting over the SQLite WAL. Stale lockfiles are reclaimed automatically when (a) the holder PID is no longer alive, (b) the lock is older than 24 hours, or (c) the payload is corrupt. This is what keeps a manual `pincher index` and a Claude Code SessionStart hook from racing each other.

---

## HTTP REST API

All 22 tools are available via `POST /v1/{tool}` with a JSON body. Run alongside MCP stdio — no either/or.

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

# pinchQL graph query (legacy `cypher` parameter still accepted for one release)
curl -s -X POST http://localhost:8080/v1/query \
  -d '{"pinchql": "MATCH (f:Function)-[:CALLS]->(g) WHERE f.name = '\''main'\'' RETURN g.name LIMIT 10", "project": "myproject"}' | jq .

# Liveness probe — no auth required
curl http://localhost:8080/v1/health

# OpenAPI spec (Postman / Cursor importable)
curl http://localhost:8080/v1/openapi.json | jq .
```

Responses compress ~65% with `Accept-Encoding: gzip`. Tested clients: curl, Python `requests`, PowerShell `Invoke-WebRequest`. Rate limiting: `--http-rate 60` limits to 60 requests/IP/minute (0 = unlimited).

### Additional HTTP endpoints

| Endpoint | Method | Auth | Description |
|---|---|---|---|
| `/v1/health` | GET | No | Liveness probe — schema version, index staleness. Always 200. |
| `/v1/dashboard` | GET | No | Self-contained HTML dashboard (stats, search, project cards, sparkline). No external deps. |
| `/v1/dashboard.css` | GET | No | Dashboard stylesheet. Served separately so CSP can drop `'unsafe-inline'`. |
| `/v1/dashboard.js` | GET | No | Dashboard JavaScript. Same CSP rationale. |
| `/v1/openapi.json` | GET | No | OpenAPI 3.1 spec covering all 16 tool endpoints. Import into Postman or Cursor. |
| `/v1/stats` | GET | Yes | Current session + all-time savings as JSON. |
| `/v1/sessions` | GET | Yes | Per-session history, last 90 sessions, sorted by recency. |
| `/v1/projects` | GET | Yes | All indexed projects with file/symbol/edge counts. |
| `/v1/projects` | DELETE | Yes | Remove a project and all its symbols. Body: `{"id":"<project-id>"}`. |
| `/v1/index-progress` | POST | Yes | Live indexing progress: `{files_done, files_total, active}`. |

CORS: all responses include `Access-Control-Allow-Origin: *` so browsers can call directly without a proxy.

---

## CLI subcommands

`pincher <subcommand> --help` prints per-subcommand flag detail.

### `pincher index`

One-shot index without starting an MCP server — useful in CI, pre-commit hooks, or as a Claude Code SessionStart hook.

```bash
pincher index                        # index current directory
pincher index /path/to/repo          # index a specific path
pincher index --force                # re-parse all files, ignore content hashes
pincher index --hook                 # emit Claude Code SessionStart JSON envelope
pincher index --json-summary         # machine-readable JSON output
pincher index --data-dir /custom     # override data directory
pincher index --max-file-size-mb 32  # per-file size cap (override default)
```

`--hook` outputs the JSON envelope Claude Code's SessionStart hook injects as `additionalContext`. Configure in `.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      { "type": "command", "command": "pincher index --hook" }
    ]
  }
}
```

### `pincher doctor`

Diagnostic report — schema version, index staleness, extraction-failure counts by reason, slow-query log. Both human-readable and JSON output:

```bash
pincher doctor                       # Markdown report
pincher doctor --json                # structured output for CI
pincher doctor --lookback 24h        # filter slow queries / failures by age
```

### `pincher self-test`

End-to-end smoke check against a temporary data directory: open the database, create a synthetic project, index a sample file, search for a known symbol, retrieve it via byte-offset. Exits non-zero on any failure.

```bash
pincher self-test                    # 5-step smoke test
pincher self-test --verbose          # also prints per-step timings
```

### `pincher rebuild-fts`

Escape hatch for FTS5 corruption. Drops every FTS5 virtual table (legacy `symbols_fts` + the per-corpus `symbols_{code,config,docs}_fts`) and their sync triggers, then bulk-loads them back from the canonical `symbols` table.

```bash
pincher rebuild-fts                  # rebuild and print row count
pincher rebuild-fts --quiet          # row count only — pipe-friendly
pincher rebuild-fts --data-dir /x    # override data directory
```

Use this if `pincher search` returns results inconsistent with `pincher query` against the same project. Cost is proportional to symbol count (seconds-to-minutes on large repos). Source files are not re-walked.

### `pincher update`

Auto-detects whether the binary is running from a clone of pincherMCP (walks ancestors looking for a `go.mod` matching this module). In-repo: `git fetch` + `git pull --ff-only` + `go build`. Otherwise: queries the GitHub releases API, picks an asset matching `GOOS`/`GOARCH`, atomically swaps the running binary aside on Windows before installing the replacement.

```bash
pincher update                       # apply update if behind
pincher update --check               # report status only
pincher update --source DIR          # override auto-detected checkout
pincher update --dry-run             # print what would run
pincher update --yes                 # skip confirmation
```

**Caveat:** release artifacts (windows/linux/darwin binaries on each tag) aren't published yet. The asset-matching code is ready for them; the workflow change to upload artifacts is a separate task. Until then, in-repo mode is the supported path.

### `pincher web`

Resolve the dashboard URL of the running pincher HTTP server. If a live server is found via the sessions table (PID liveness + `/v1/health` probe), prints the URL. Otherwise auto-spawns `pincher --http 127.0.0.1:N` detached on a free port (scans upward from 7777, 16-port window), polls `/v1/health` until ready, prints the new server's URL.

```bash
pincher web                          # print dashboard URL; auto-start if none
pincher web --no-start               # exit non-zero if none running
pincher web --port 8080              # scan from 8080 instead of 7777
pincher web --json                   # {url, base, pid, started_by}
pincher web --timeout 8              # auto-start readiness wait (seconds)
```

The dashboard URL is `<base>/v1/dashboard` (honors `--basepath` reverse-proxy prefix).

### `pincher init`

Inject the pincher usage policy block into an editor or agent's rules file. Wraps the policy in `<!-- pincher:start --> ... <!-- pincher:end -->` markers (or `// pincher:start` line markers for JSON-based targets like Continue) so re-running replaces the block in place — idempotent, no duplicates.

```bash
pincher init                              # default: ./CLAUDE.md
pincher init --global                     # claude global: ~/.claude/CLAUDE.md
pincher init --target=cursor              # ./.cursor/rules/pincher.mdc (with frontmatter)
pincher init --target=cursor-legacy       # ./.cursorrules
pincher init --target=windsurf            # ./.windsurfrules
pincher init --target=aider               # ./CONVENTIONS.md
pincher init --target=continue            # ~/.continue/config.json (merges into systemMessage)
pincher init --target=detect              # write only to editors whose marker file exists under cwd
pincher init --target=all                 # write every project-scoped target
pincher init --dry-run                    # print what would be written; do not modify
```

The cursor target preserves any user-edited YAML frontmatter (`description`, `globs`, `alwaysApply`) on re-runs — only the marker block in the body is replaced. The continue target preserves all unknown JSON keys; only the `systemMessage` field is touched.

After writing, prints a short next-steps recipe + the URL of any running pincher HTTP dashboard discovered via the v11 sessions table.

### `pincher project`

Surface the HTTP `DELETE /v1/projects` and the `list` MCP tool as CLI verbs so users on the stdio binary don't need a SQL or curl one-liner.

```bash
pincher project list                      # human-readable table (alias: ls)
pincher project list --json               # machine-readable JSON
pincher project rm <name>                 # interactive Y/n confirmation (alias: remove, delete)
pincher project rm <name> --force         # skip confirmation
pincher project rm <name> --json --force  # JSON receipt; --force required in JSON mode
```

`<name>` resolves in this order: full project id → exact name (case-insensitive) → substring on name or path. A substring that matches multiple projects errors with a disambiguation list rather than picking one. JSON mode requires `--force` (no interactive prompt is possible in a scripted workflow).

---

## CLI flags

Apply to the no-subcommand form (running as MCP server).

| Flag | Default | Env fallback | Purpose |
|---|---|---|---|
| `--version` | false | — | Print version and exit. |
| `--data-dir` | platform default | — | Override database directory. |
| `--verbose` | false | — | Verbose logging to stderr. |
| `--http` | "" | `PINCHER_HTTP_ADDR` | Listen for HTTP REST on this address (`:8080`, `:0` for OS-picked). |
| `--http-key` | "" | `PINCHER_HTTP_KEY` | Bearer token for HTTP requests. Recommended for non-localhost. |
| `--http-rate` | 0 | — | Max HTTP requests per IP per minute. 0 = unlimited. |
| `--basepath` | "" | `PINCHER_BASEPATH` | URL prefix behind a reverse proxy (e.g. `/pincher`). |
| `--trust-proxy` | false | `PINCHER_TRUST_PROXY=1` | Honor X-Forwarded-* headers. Only enable behind a trusted proxy. |
| `--slow-query-ms` | 0 | — | Persist tool calls slower than N ms to `slow_queries`. 0 = disabled (zero overhead). |
| `--db-readers` | 4 | `PINCHER_DB_READERS` | Max concurrent SQLite read connections (1–32). Higher = more parallel tool calls under load. |
| `--max-file-size-mb` | 512 | `PINCHER_MAX_FILE_SIZE_MB` | Per-file size cap during indexing. Larger files recorded as `file_too_large`, skipped. 0 disables cap. |

---

## Environment variables

Used when the matching flag is empty — convenient for Docker, systemd, launchd.

| Variable | Equivalent flag |
|---|---|
| `PINCHER_HTTP_ADDR` | `--http` |
| `PINCHER_HTTP_KEY` | `--http-key` |
| `PINCHER_BASEPATH` | `--basepath` |
| `PINCHER_TRUST_PROXY` | `--trust-proxy` (set to `1` to enable) |
| `PINCHER_DB_READERS` | `--db-readers` |
| `PINCHER_MAX_FILE_SIZE_MB` | `--max-file-size-mb` |

`PINCHER_HTTP_ADDR=:0` picks a free port and the bound address is printed to stderr at startup. The Docker image sets `PINCHER_HTTP_ADDR=:8080` by default.

---

## Data directory

| Platform | Default location |
|---|---|
| Windows | `%APPDATA%\pincherMCP\pincher.db` |
| macOS | `~/Library/Application Support/kwad77/pincher.db` |
| Linux | `~/.local/share/kwad77/pincher.db` |

Override with `--data-dir /custom/path`. Back up with any file copy tool.

---

## Performance

Measured on this codebase (13 files, 618 symbols, 5,785 edges, Windows 11, SQLite WAL):

| Operation | Measured time | Notes |
|---|---|---|
| Cold index (13 files) | ~190 ms | Concurrent goroutines, xxh3 hash |
| Incremental re-index (0 changes) | <5 ms | All files skipped via hash |
| `architecture` | 12 ms server / 69 ms HTTP | Was 10 s+ before savings-calc fix |
| `search` | 1 ms | BM25 via FTS5 |
| `health` | 1 ms | |
| `stats` | 8 ms | |
| `symbol` (byte-offset seek) | <1 ms | 1 SQL + 1 seek + 1 read |
| Single-hop pinchQL query | 2 ms | SQL JOIN |
| BFS depth 3 | <5 ms | Go BFS over CTE |
| Session stats flush | every 10 s | Background goroutine |

**SQLite configuration:** WAL mode, `busy_timeout=5000ms`, `SetMaxOpenConns(1)` (serialized single-writer). Readers never block writers in WAL mode. Reader pool (`--db-readers`, default 4, capped at 32) fans concurrent reads across `mode=ro` connections.

**WAL bounding:** `journal_size_limit=256 MiB` caps the WAL; `PRAGMA wal_checkpoint(TRUNCATE)` runs at the tail of each `Index()` run to fold the WAL back into the main DB at the natural quiet point. `PRAGMA optimize` runs on the same cadence. These are the WAL guardrails added after the 70 GB WAL incident produced by an unbounded multi-writer storm — the bound holds even under heavy churn.

**Watch backoff:** the file-change watcher's 5-second tick body short-circuits when any `Index()` is in flight for any project. During large catch-up phases the watcher idles at near-zero CPU instead of bouncing repeatedly off the per-project mutex.

**Pinned-corpus benchmarks:** `make bench` runs per-corpus benchmarks at `-benchtime=2s -benchmem` against `testdata/corpus/{go-project,k8s-ops,node-monorepo,docs-site}`. CI gate compares against committed baselines and fails on `ns/op +20%` or `allocs/op +30%` regressions.

---

## Schema

Schema is versioned via the `schema_version` table. Current version: **v11**. Migrations apply automatically on startup — no data loss, no manual steps. To add a migration: append a SQL string to `schemaMigrations` in `db.go`; the version number is auto-derived from the slice length.

Migration history:

| Version | Summary |
|---|---|
| v1 | Baseline: projects, symbols, edges, symbols_fts |
| v1→v2 | `extraction_confidence` column on symbols |
| v2→v3 | `symbol_moves` + `idx_sym_qnkind` (file rename detection) |
| v3→v4 | `sessions` table for ROI tracking |
| v4→v5 | (slot reserved during refactor; no DDL) |
| v5→v6 | Generated `symbol_id` column for FTS5 external-content lookups |
| v6→v7 | `extraction_failures` table for `pincher doctor` |
| v7→v8 | `slow_queries` table (--slow-query-ms capture) |
| v8→v9 | Per-corpus FTS5 split — `symbols_{code,config,docs}_fts` + routing triggers |
| v9→v10 | TOML routed to the config corpus (DROP/CREATE triggers) |
| v10→v11 | `http_url` + `http_pid` columns on sessions for `pincher web` discovery |

---

## Key invariants

- `SetMaxOpenConns(1)` — SQLite is single-writer; all writes serialize at the pool.
- WAL mode — readers never block writers; 5 s busy timeout prevents immediate failure during indexing.
- `journal_size_limit=256 MiB` + `wal_checkpoint(TRUNCATE)` at every `Index()` tail — keeps the WAL bounded under heavy churn.
- Cross-process project lockfile — multiple pincher binaries on one data directory serialize safely; stale-holder reclaim covers crashed processes.
- File re-parse always deletes the file's prior symbols before re-extraction — no stale rows leak; cascades to edges with either endpoint in the file.
- FTS5 triggers (`sym_fts_insert`, `sym_fts_delete`, `sym_fts_update`, plus the v9 corpus-routed variants) auto-sync — never manually sync.
- Generated `symbol_id` column on `symbols` mirrors `id` so FTS5 content lookups against the FTS column name work; never write to `symbol_id` directly.
- `symSelectFrom` and `symRow` (in `cypher/engine.go`) must stay in sync when adding columns.
- Batch flush at 500 symbols or 1,000 edges to bound memory on large repos.
- ClassifyCorpus parity gate — the Go classifier and the v9 SQL trigger WHERE clauses encode the same rules; `TestClassifyCorpus_MatchesSQLTriggerRouting` is the regression test that catches drift.

---

## Project layout

```
pincherMCP/
├── cmd/pinch/
│   ├── main.go                   # Sole entry point: MCP server + subcommand dispatch
│   ├── bloat_trap.go             # isBloatTrap: refuse filesystem root + $HOME;
│   │                             # hook mode also requires a project marker
│   ├── doctor.go                 # `pincher doctor` subcommand
│   ├── rebuild_fts.go            # `pincher rebuild-fts` subcommand
│   ├── selftest.go               # `pincher self-test` subcommand
│   ├── update.go                 # `pincher update` subcommand
│   ├── web.go                    # `pincher web` subcommand
│   ├── web_unix.go / web_windows.go  # detached-spawn helpers per OS
│   ├── init.go                   # `pincher init` subcommand
│   └── policy.md                 # Embedded policy block written by `pincher init`
├── internal/
│   ├── db/db.go                  # SQLite store: schema, migrations, all CRUD,
│   │                             # FTS5 ops (legacy + per-corpus), graph ops,
│   │                             # BPE token counting, WAL guardrails
│   ├── db/corpus.go              # ClassifyCorpus(language, kind) → code/config/docs
│   ├── ast/                      # Multi-language extraction
│   │   ├── extractor.go          # Per-language registry, byte offsets, confidence
│   │   ├── yaml.go / hcl.go / bash.go / markdown.go / toml.go / jinja2.go / sql.go / makefile.go
│   │   ├── blocklist.go          # Lockfile / minified / source-map filter
│   │   └── confidence.go         # Per-symbol confidence composition
│   ├── cypher/engine.go          # Cypher → SQL: tokenizer → parser → 3 query paths
│   ├── index/
│   │   ├── indexer.go            # Walk → hash → extract → resolve → store → watch
│   │   └── lockfile.go           # Cross-process project lockfile w/ stale reclaim
│   └── server/server.go          # 22 MCP tools, HTTP REST, gzip, OpenAPI 3.1, bearer auth,
│                                 # basepath / reverse-proxy support, sessions persistence
└── go.mod
```

---

## Test coverage

```bash
go test ./...                                              # run all tests
go test ./... -coverprofile=cover.out                      # with coverage
go tool cover -func=cover.out | grep "^total"              # total: 84.3%
go test ./internal/db/ -run TestGraphStats_WithData -v     # single test
go test ./internal/server/ -v                              # server package
```

Current coverage by package:

| Package | Coverage |
|---|---|
| `internal/cypher` | 94.2% |
| `internal/ast` | 89.9% |
| `internal/server` | 89.1% |
| `internal/index` | 84.1% |
| `internal/db` | 84.1% |
| **total** | **84.3%** |

`internal/db` and `internal/index` set the floor — both have OS / SQLite / network code that resists pure unit testing (`ListenAndServeHTTP`, `handleFetch`, `extractTextFromHTML`, MCP `onInit`/`onRoots`/`detectRoot` callbacks, file-system race paths in the watcher). The CI gate is set to **84%**.

---

## Dependencies

| Dependency | Purpose |
|---|---|
| `github.com/modelcontextprotocol/go-sdk v1.4.0` | MCP server (JSON-RPC 2.0 over stdio) |
| `modernc.org/sqlite` | Pure-Go SQLite (no CGO) |
| `github.com/tiktoken-go/tokenizer` | cl100k_base BPE tokenizer — real token counts |
| `github.com/boyter/gocodewalker` | File walker that respects `.gitignore` |
| `github.com/zeebo/xxh3` | Fast content hashing for incremental indexing |
| `gopkg.in/yaml.v3` | YAML/JSON Node tree parsing |
| `github.com/BurntSushi/toml` | TOML parseability gate |
| `github.com/hashicorp/hcl/v2` | HCL/Terraform parser |
| `mvdan.cc/sh/v3` | Bash parser (the `shfmt` parser) |
| `github.com/yuin/goldmark` | Markdown CommonMark parser |
| `github.com/nikolalohinski/gonja` | Jinja2 parser |

---

## Roadmap

Each release tier names a theme and the issues that close it. Issue numbers link the roadmap to actionable work — track progress at <https://github.com/kwad77/pincher/issues>.

### v0.2 — Index quality at scale ✅

- Pinned-corpus snapshot tests (`testdata/corpus/{go-project,k8s-ops,node-monorepo,docs-site}`); CI gate catches extraction drift on every PR. ([#33](https://github.com/kwad77/pincher/issues/33))
- Bash extractor — `mvdan.cc/sh/v3/syntax` at confidence 1.0. ([#38](https://github.com/kwad77/pincher/pull/38))
- HCL/Terraform extractor — `hashicorp/hcl/v2/hclsyntax` at confidence 1.0. ([#67](https://github.com/kwad77/pincher/pull/67))
- Per-corpus FTS5 split — three new vtabs route queries per language/kind. ([#32](https://github.com/kwad77/pincher/issues/32))
- Markdown extractor — `yuin/goldmark`, one Section per heading.
- Jinja2 extractor — `nikolalohinski/gonja`, macro/block/set + IMPORTS edges. ([#70](https://github.com/kwad77/pincher/issues/70))

### v0.3 — Trust + observability ✅

- Security audit — every documented security claim has a regression test. ([#41](https://github.com/kwad77/pincher/issues/41))
- `pincher doctor` subcommand + `extraction_failures` table + slow-query log. ([#42](https://github.com/kwad77/pincher/issues/42))
- Dashboard CSP tightening — externalized inline JS/CSS. ([#65](https://github.com/kwad77/pincher/pull/65))
- `pincher rebuild-fts` escape hatch. ([#72](https://github.com/kwad77/pincher/pull/72))
- Per-symbol confidence scoring — replaces per-language constant with composable signals. ([#34](https://github.com/kwad77/pincher/issues/34))

### v0.4 — Performance under load 🚧

- Pinned-corpus benchmarks — `make bench` per-corpus; CI smoke-job gates against accidental order-of-magnitude regressions. ([#50](https://github.com/kwad77/pincher/issues/50)) ✅
- Reader pool — split read connections from the single-writer using SQLite WAL's concurrent-read capability. ([#51](https://github.com/kwad77/pincher/issues/51)) ✅
- HTTP discovery via sessions table — `pincher web` resolves the dashboard URL without scanning ports.
- Self-update — `pincher update` for in-repo and (planned) release-asset paths.
- `pincher init` for one-step CLAUDE.md policy injection.
- Incremental edge resolution — `resolveCalls` / `resolveImports` only re-process files touched in the current `Index()` run. Filed when bench data justifies it.

### v0.5 — Polish + extension surface

- Struct field extraction — index fields/properties as symbols.
- Cross-project `query` — explicit opt-in via `cross_project=true`.
- Webhook-triggered re-index — `POST /v1/reindex` for git post-receive hooks.
- VS Code extension — auto-configures MCP, hover-to-inspect command.
- `.pincher.yml` per-project config — blocklist additions, confidence threshold defaults, primary-language hint.

### v1.0 — Stable API

- Tool schemas frozen — no breaking changes to the 16 tool I/O shapes after this.
- Symbol-ID format frozen — `{file_path}::{qualified_name}#{kind}` is the contract.
- HTTP REST surface frozen — `POST /v1/{tool}`, basepath/trust-proxy/rate-limit/SSRF behaviours all locked.
- `SECURITY.md` — documented threat model.
- Pre-built binaries on every release tag (Linux/macOS/Windows × amd64/arm64).
- Docker image — `ghcr.io/kwad77/pincher:latest`.

### Out-of-scope until real demand

- PostgreSQL backend — meaningful scope; SQLite + cross-process lockfile + WAL covers the documented single-team-per-machine case.
- Role-based access beyond auth + SSRF — multi-tenant ACL is a different product.
- Shared multi-user server mode — needs real deployment validation.

---

## Known limitations

- **Sequence-rename ID instability in YAML / JSON arrays** (#205, decided as won't-fix for v0.7.0). Inserting an item at index 0 of a YAML sequence (or JSON array) renames every downstream symbol's qualified name: `tasks.0.name` becomes `tasks.1.name`, the old ID disappears, a new ID appears. Move detection via `(qualified_name, kind)` matching catches *some* of this but not deterministically — the qualified names changed, so the move-detection key doesn't match.

  **Practical impact**: in `pincher changes` output, a sequence reorder shows up as N deletes + N adds rather than a single move. In long-lived stored ID references (e.g. an ADR pinning a specific symbol id), inserting a new item at the top of a sequence breaks the reference.

  **Why not fix it now**: a content-hash ID scheme (deterministic across reorders) is real engineering work — symbol-ID format change, migration path, full re-index of every existing DB. The blast radius is mostly Ansible / k8s manifests, and those are typically searched via `corpus=config` BM25 anyway, where the qualified-name churn is invisible to FTS5. We'd be paying a structural cost for a problem that real users mostly don't hit through the queries pincher is good at.

  **Workarounds**: use named-list syntax where the YAML schema allows it (`tasks: [{name: deploy, ...}]` reads `tasks.0.name = "deploy"` regardless of position once the parser sees `name:` as the canonical key — a future enhancement). For ADRs and long-lived references, prefer searching by symbol *name* (`pincher search`) over storing the symbol id.

  **Revisit trigger**: real complaints with reproducible churn. v0.8 / v1.1 territory. Tracked at #205.
- **Single-user SQLite.** Concurrent processes are safely serialized via `internal/index/lockfile.go`, but the `sessions` table and symbol store are local-only. Team/enterprise shared indexes need a server mode that's not built yet.
- **Regex gap.** ~7 non-Go languages still use regex extraction (~70–85% accuracy). `extraction_confidence` surfaces this to callers. Full fix = per-language pure-Go AST libraries (no tree-sitter / no CGO), tracked in the extractor refactor plan.
- **HTTP auth.** The `--http` REST API is open by default; bearer-token auth is opt-in via `--http-key` (or `PINCHER_HTTP_KEY`). For non-localhost deployments, set `--http-key` or front pincher with a reverse proxy.
- **Two-process stats lag.** The MCP stdio process and the HTTP dashboard process can be separate (e.g. `pincher web` auto-spawns its own). Stats are shared via the `sessions` SQLite table; the flusher cadence adapts (#204): 10 s steady-state when running solo, 1 s when an HTTP dashboard peer process is detected. The dashboard shows all-time totals from DB when it has no live MCP session.
- **`symbols` batch cap.** `maxBatchSymbols = 100` — requests with more than 100 IDs are rejected. Larger batches: split client-side.
