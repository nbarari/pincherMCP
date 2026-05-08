# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
# Build the binary
go build -o pincher.exe ./cmd/pinch/     # Windows
go build -o pincher ./cmd/pinch/         # Linux/macOS

# Run all tests
go test ./...

# Run tests with coverage
go test ./... -coverprofile=cover.out
go tool cover -func=cover.out | grep "^total"

# Run a single test
go test ./internal/db/ -run TestGraphStats_WithData -v

# Run tests for one package
go test ./internal/server/ -v

# View per-function coverage gaps
go tool cover -func=cover.out | grep -v "100.0%" | sort -t'%' -k1 -n
```

**After any schema change** (adding a column to `db.go`), rebuild `pincher.exe` and reconnect via `/mcp` in Claude Code so the binary serving MCP requests picks up the new schema.

## Architecture

### Data flow

```
cmd/pinch/main.go          ← sole entry point (MCP server + optional HTTP + `pincher index` CLI)
  → db.Open()              open/migrate SQLite (schema v6)
  → index.New()            create indexer (holds *db.Store)
  → server.New()           create MCP server (holds *db.Store + *index.Indexer)
  → srv.StartSessionFlusher() background goroutine: flushes session stats to DB every 10s
  → idx.Watch()            background goroutine: polls projects for file changes
  → [--http :PORT]         optional HTTP server for platform-agnostic REST access
  → mcp.StdioTransport     JSON-RPC 2.0 over stdin/stdout (Claude Code)
```

### Three-layer storage (single `symbols` table serves all three)

| Layer | Mechanism | Query path |
|---|---|---|
| 1 — Byte-offset retrieval | `start_byte` / `end_byte` on every symbol | `GetSymbol` → `ReadSymbolSource` = 1 SQL + 1 `os.File.Seek` + 1 `Read` |
| 2 — Knowledge graph | `symbols` rows + `edges` table | Cypher → SQL via `cypher/engine.go` |
| 3 — FTS5 full-text search | `symbols_fts` virtual table + 3 triggers | `SearchSymbols` via BM25 |

All three indexes are populated in a single `ast.Extract()` call per file during indexing.

### Package responsibilities

- **`internal/db/db.go`** — SQLite store. Schema lives here as a `schema` const. Schema migrations live in `schemaMigrations` (a `[]string` slice — append to add a migration; version is auto-derived from slice length). Current schema: **v6** (added generated `symbol_id` column on `symbols` to match the FTS5 vtab's first column name, fixing latent content-lookup errors — issue #19). `symSelectFrom` is the canonical SELECT column list used by all symbol queries; update it and all scan functions together when adding columns.

- **`internal/ast/extractor.go`** — Multi-language symbol extraction. `Extract(source, language, relPath)` dispatches to per-language extractors and sets `ExtractionConfidence` on each symbol (1.0 for Go/AST and YAML/JSON via yaml.v3, 0.85 for stable regex languages, 0.70 for approximate ones). Go uses `go/ast`; YAML/JSON uses `gopkg.in/yaml.v3` Node tree to emit one `Setting` symbol per key with a dotted-path qualified name (`services.web.image`); all other languages use regex. `extractionConfidence` map controls per-language scores.

- **`internal/ast/blocklist.go`** — `ShouldSkip(path) (bool, reason)` filters lockfiles (`package-lock.json`, `yarn.lock`, etc.), minified bundles (`*.min.js`), and source maps (`*.map`) before extraction. Belt-and-suspenders relative to gocodewalker's `.gitignore` respect — committed lockfiles still get filtered. `IndexResult.Blocked` reports the count.

- **`internal/ast/languages.go`** — File extension → language detection and `IsSourceFile` filter. YAML and JSON are registered here.

- **`internal/cypher/engine.go`** — Cypher-to-SQL translation. Pipeline: `tokenize` → `parseQuery` → `run`. Three query paths: `runNodeScan` (no edge), `runJoinQuery` (single-hop, SQL JOIN), `runBFS` (variable-length, Go BFS loop). `symRow` struct and all SELECT queries must stay in sync with `db.go`'s `Symbol` fields — both have `extraction_confidence`.

- **`internal/index/indexer.go`** — Indexing pipeline. `Index()` walks files concurrently (goroutine per file, `sync.WaitGroup`), hashes with xxh3, skips unchanged files, calls `ast.Extract`, converts to `db.Symbol`/`db.Edge`, flushes in batches. Per-file goroutine calls `DeleteSymbolsForFile` before re-extraction so removed symbols don't leak (cascades to edges). Per-project mutex (`idx.active` map + `idx.mu`) prevents concurrent index of the same project within one process; `acquireProjectLock` (a filesystem lockfile under `<dataDir>/locks/`) prevents concurrent index across processes, with stale-holder reclaim. After all per-file goroutines finish, `resolveImports` and `resolveCalls` run a deferred project-wide pass against the now-complete symbol table — this is what makes Go cross-file `CALLS` and `IMPORTS` edges resolve. `Watch()` polls all projects every 2s (active) or 30s (idle), and short-circuits via `anyActive()` when any `Index()` is in flight (no busy CPU during catch-up phases). On re-index, detects file moves by matching `(qualified_name, kind)` across projects and records redirects in `symbol_moves`. Tail of `Index()` invokes `store.Optimize()` and `store.CheckpointTruncate()` to keep WAL bounded.

- **`internal/index/lockfile.go`** — Cross-process project lockfile. `acquireProjectLock(dataDir, projectID)` writes `<dataDir>/locks/<hash>.lock` with `O_EXCL`; payload carries holder PID and start time. Reclaim path covers stale (>24h), dead-PID, and corrupt-payload cases.

- **`cmd/pinch/bloat_trap.go`** — `IsBloatTrap(absPath) (bool, reason)` refuses to index home directories, common cache locations, and language package roots. Both the `pincher index` CLI subcommand and the MCP `index` tool path go through this guard before walking.

- **`internal/server/server.go`** — MCP server + HTTP REST gateway. All 14 tools registered in `registerTools()`. Every handler calls `jsonResultWithMeta()` which wraps the result in a `_meta` envelope and atomically increments session stats. `StartSessionFlusher()` flushes those stats to the `sessions` table every 10s. Token savings are estimated via `savedVsFileSizes()` (uses real `os.Stat` file sizes for search/trace) and `savedVsFullRead()` (uses `avgFileSize=20000` for other tools) — baseline is "agent reads whole file", not just the symbol. The HTTP stats endpoint falls back to DB totals when no live MCP session exists (e.g. HTTP-only dashboard process). `ServeHTTP` / `ListenAndServeHTTP` expose all tools as `POST /v1/{tool}` plus `DELETE /v1/projects` for project removal. `sessionID`/`sessionRoot` are set once via `sessionOnce` from the MCP roots list. The `cypher.Executor` is initialised with `ProjectID` so all three query paths (node scan, JOIN, BFS) are scoped to the resolved project.

### The 14 MCP tools

| # | Tool | Purpose |
|---|---|---|
| 1 | `index` | Index a repo (incremental by default; `force=true` to re-parse all) |
| 2 | `symbol` | Retrieve source by stable ID via O(1) byte-offset seek |
| 3 | `symbols` | Batch retrieve multiple symbols in one call |
| 4 | `context` | Symbol + its direct imports as a minimal-token bundle |
| 5 | `search` | FTS5 BM25 full-text search (wildcards, phrases, kind/language/fields filters) |
| 6 | `query` | Cypher-like graph queries (node scan, single-hop JOIN, BFS) |
| 7 | `trace` | BFS call-path trace with CRITICAL/HIGH/MEDIUM/LOW risk labels |
| 8 | `changes` | Git diff → affected symbols → blast radius BFS |
| 9 | `architecture` | High-level orientation: languages, entry points, hotspot functions |
| 10 | `schema` | Graph schema: node/edge kind counts |
| 11 | `list` | All indexed projects with stats |
| 12 | `adr` | Architecture Decision Records: persistent key/value project knowledge |
| 13 | `health` | Schema version, index staleness, per-language extraction coverage |
| 14 | `stats` | Session savings summary: tokens used/saved, cost avoided, call count |
| 15 | `fetch` | Fetch a URL, extract text, store as a searchable `Document` symbol in the knowledge base (512 KB body cap, 32 KB stored). Retrieve later via `search kind:Document` or `symbol`. |

### Symbol ID format

```
"{file_path}::{qualified_name}#{kind}"
e.g. "internal/db/db.go::db.Open#Function"
```

IDs are stable across re-indexing as long as file path and qualified name don't change. Built by `db.MakeSymbolID()`. If a file moves, `handleSymbol` automatically resolves stale IDs via `store.ResolveStaleID()` → `symbol_moves` table.

### Schema migration pattern

To add a schema change:
1. Append a SQL string to `schemaMigrations` in `db.go`
2. Update the `Symbol` struct field, `symSelectFrom` const, and all scan functions (`scanOneSymbol`, `scanSymbolRowsRow`, `scanSymbolRow`) together
3. Update `cypher/engine.go`'s `symRow` struct and all SELECT queries there too
4. Update `ast/extractor.go`'s `ExtractedSymbol` struct and `indexer.go`'s symbol construction if the field originates in extraction

### Key invariants

- `db.SetMaxOpenConns(1)` — SQLite is single-writer; all writes are serialized at the connection pool level
- WAL mode + `_busy_timeout=5000` — readers never block writers; a 5s retry window prevents immediate failures during index
- WAL bounding: `journal_size_limit=256 MiB` (set in `Open()`) plus `store.CheckpointTruncate()` at every `Index()` tail. An earlier branch tried `wal_autocheckpoint=100` for the same purpose and measured 14.5× slowdown on heavy single-writer indexing — reverted to the SQLite default of 1000 pages; the cap-plus-tail-truncate path is what holds.
- Cross-process project lock: `internal/index/lockfile.go` serializes concurrent indexers across binaries on the same data directory. Stale lockfiles are reclaimed via PID liveness check + 24h timeout + corrupt-payload guard.
- Stale-symbol cleanup: every per-file goroutine calls `DeleteSymbolsForFile(projectID, relPath)` before re-extraction. The DELETE cascades to edges with either endpoint in the file, so removing a symbol also clears inbound CALLS edges that resolved to it via `resolveCalls`. Hash-skip short-circuits unchanged files at the parent level so the DELETE only fires for actually-changed content.
- Go cross-file resolution: `resolveImports` and `resolveCalls` run a project-wide pass after all per-file goroutines complete, producing real cross-file CALLS/IMPORTS edges. Scoped to Go (extractor at confidence 1.0); regex-extracted languages keep per-file resolution to avoid false positives.
- FTS5 triggers (`sym_fts_insert`, `sym_fts_delete`, `sym_fts_update`) auto-sync the `symbols_fts` virtual table; never manually sync it.
- The generated `symbol_id` column on `symbols` (v6 migration) mirrors `id` to satisfy FTS5's content-lookup column-name parity. Read-only by definition (`GENERATED ALWAYS … VIRTUAL`); never written to directly.
- `flushBuffers` is called when the in-memory batch reaches 500 symbols or 1000 edges to bound memory during large index runs.
- Bloat-trap and blocklist are belt-and-suspenders: gocodewalker's `.gitignore` respect is the first line of defense; `IsBloatTrap` refuses obvious dead-end paths before walking; `ShouldSkip` filters individual files (lockfiles, minified, source maps) that ARE committed.

## Dependencies

- `github.com/modelcontextprotocol/go-sdk v1.4.0` — MCP server framework (JSON-RPC 2.0 over stdio)
- `modernc.org/sqlite` — Pure-Go SQLite (no CGO required)
- `github.com/boyter/gocodewalker` — File walker that respects `.gitignore`
- `github.com/zeebo/xxh3` — Fast content hashing for incremental indexing
- `gopkg.in/yaml.v3` — YAML/JSON Node tree parsing for the `Setting` extractor (JSON is parsed via the same library since JSON is a YAML 1.2 subset)
- `github.com/tiktoken-go/tokenizer` — cl100k_base BPE tokenizer for real (not approximate) token-savings accounting

## Known Architectural Limitations (tracked, not yet fixed)

- **Regex gap**: 17 non-Go non-YAML languages use regex extraction (~80% accuracy). `extraction_confidence` field surfaces this to callers. YAML/JSON are now at confidence 1.0 via yaml.v3. Full fix for the rest = tree-sitter bindings (no CGO path; planned next sprint).
- **YAML sequence-rename ID instability**: Inserting an item at index 0 of a YAML sequence renames every downstream symbol's qualified name (`tasks.0.name` → `tasks.1.name`). Move detection via `(qualified_name, kind)` matching catches some of this but not deterministically. The CLAUDE.md-documented "IDs stable across re-indexing" invariant has caveats for sequence-heavy YAML.
- **Single-user SQLite**: Concurrent processes are now safely serialized via `internal/index/lockfile.go`, but the `sessions` table and symbol store are still local-only. For team/enterprise shared indexes, a server mode with a shared DB path or a PostgreSQL backend is needed.
- **Single shared FTS5 corpus**: All symbol kinds (Function, Method, Setting, etc.) share `symbols_fts`, so adding YAML Settings or future markdown headings can dilute BM25 scores for code identifiers. Tracked at #32 (per-corpus FTS5 split) — splits into `symbols_fts_code` / `symbols_fts_config` / `symbols_fts_docs`.
- **Per-language extraction_confidence is a constant, not a score**: A `package-lock.json` Setting and a Helm `services.web.image` Setting both score 1.0 today, even though one is generated noise and the other is real config. The static blocklist in `internal/ast/blocklist.go` patches this at the file level. Tracked at #34 (per-symbol confidence with composable signals: file-level penalties, content-shape penalties, identifier-quality bonuses).
- **No corpus-level regression test**: Each PR's manual smoke tests aren't committed, so the index-quality gates we'd want (lockfile Settings == 0, cross-file CALLS edge count, BM25 score for code identifiers) aren't enforced in CI. Tracked at #33 (pinned-corpus snapshot tests with positive AND negative assertions; `make corpus-snapshot-update` regenerates positive snapshots but `negative_assertions` require explicit code review).
- **HTTP auth**: The `--http` REST API supports optional bearer token auth via `--http-key <token>`. Without it the API is open; for production deployment put it behind a reverse proxy or always set `--http-key`. With `--basepath` and `--trust-proxy`, pincher fronts cleanly behind nginx-style reverse proxies.
- **Two-process stats gap**: The MCP stdio process and the HTTP dashboard process are separate. Stats are shared via the `sessions` SQLite table (flushed every 10s). The dashboard shows all-time totals from DB when it has no live MCP session.
- **`symbols` batch cap**: `maxBatchSymbols = 100` — the batch symbols tool rejects requests with more than 100 IDs.
