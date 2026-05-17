# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Pincher Usage Policy

This project ships pincherMCP — its own product — and dogfoods it. Prefer pincher tools over `Read`/`Grep`/`Glob` for any code-navigation task.

**Workflow:** `architecture` (orient) → `search` (find) → `context` or `symbol` (read) → `trace` (impact) → edit → `changes` (verify before push).

**Fall back to `Read`/`Grep` when:**
- Pincher returns no result (rare for code; common for config/text files).
- You need exact-byte inspection (whitespace audits).
- The file isn't indexable (binaries, large lockfiles).
- You're authoring a new file.
- **The pincher freshness check fires** (see below).

If `mcp__pincher__*` tools aren't in the registry at session start, surface a one-line warning before the first response and fall back to `Read`/`Grep`.

### Pincher freshness check (this repo specifically)

This is pincher's own repo, so the running MCP server is frequently stale relative to master. **Once per session, call `health`. If `running_binary_version` differs from the project's `schema_version_at_index` for `sessionRoot`, treat byte-offset tools (`symbol`, `context`, `neighborhood with include_source=true`) as untrusted — bytes may point at the wrong span. Discovery tools (`search`, `query`, `trace`, `changes`) stay reliable.** Use `Read` for the untrusted reads until the binary is rebuilt and `/mcp` reconnects.

## Release process

- **Minor** (`0.X.0`) — features, schema migrations, new CLI surface.
- **Patch** (`0.X.Y`) — bug fixes only. No features, no schema changes.
- **Major** — reserved for 1.0+.

**Every PR must be assigned to a milestone at PR-create time.** Milestones live at https://github.com/kwad77/pincher/milestones. Default to the next milestone (check the milestones page for the current target); don't leave a PR unassigned. A release ships when its milestone hits 100% closed.

```bash
gh pr create --milestone v0.55.0 ...
gh issue edit <PR#> --milestone v0.55.0  # after the fact
```

### Release-prep checklist (every release, no skipping)

The release-prep PR (the one before tagging) MUST touch all six below. CHANGELOG-only is the historical mistake — the README is what users hit first via the GitHub repo landing page, and stale roadmap claims erode trust faster than missing CHANGELOG entries.

1. **`CHANGELOG.md`** — run `bash scripts/changelog-assemble.sh --apply` to fold per-PR `CHANGELOG.d/<num>.<type>.md` stubs into the `[Unreleased]` section, then convert `[Unreleased]` → versioned heading with the release's theme one-liner. Stub-file convention shipped #694; legacy direct-edit still works for in-flight PRs that predate it.
2. **`README.md` roadmap table** — bump the previous `🚧 in flight` row to `✅ shipped`, add a new row for the version about to ship with its theme one-liner, optionally add the next `🚧 in flight` row.
3. **`README.md` Known limitations** — rewrite any item whose fix lands in this version into past tense; recommend the upgrade.
4. **Version-sensitive claims in README leading paragraph** — tool count, schema version, coverage badge if it moved meaningfully (>1%).
5. **`docs/REFERENCE.md` — leading metadata line** (`**Schema version:** vN · **MCP tools:** N · **Languages detected:** ~N`). Bump every release that moves any of those numbers. Per #688: the leading line is what users see first when they click into the reference doc from README; stale numbers there make every subsequent claim look distrust-by-default. Drift was 12 schema versions before #698 caught it.
6. **`docs/` (GitHub Pages site)** — audit `docs/index.html`, `docs/release-channels.md`, `docs/streamable-http.md`, `docs/troubleshooting.md`, `docs/deployment/*.md`, `docs/tutorials/*.md` for version-sensitive claims. The grep that catches drift: `grep -rnE "v0\.[0-9]+|pincher-v0\.[0-9]+|[0-9]+ MCP tools|schema.{0,15}v[0-9]+" docs/` against the previous release version. Pages renders polished landing copy from `docs/` — install tarball filenames, savings-stat parentheticals, badge value ranges, forward-looking copy about features that did/didn't ship — all higher-visibility than README to search-engine traffic. v0.67 release-prep missed `docs/index.html` "v0.66" parenthetical + `pincher-v0.66.0-linux-amd64.tar.gz` install snippet; caught next morning in a catch-up PR. Don't repeat.

If a release ships without README touched, the user's first reaction is "the README didn't say anything about it" and follow-up cleanup PRs read as forgetting, not catching up. Do it inline.

After tag pushes, the auto-bump workflow handles the Homebrew formula and Docker image — those don't go in the release-prep PR itself.

## CI conventions

- **Required gates:** `Test (mac/ubuntu/windows)`, `Coverage`, `Corpus snapshot`, `Benchmark smoke`, `Release channel rule`, `Workflow isolation lint`, `CHANGELOG stub check`. Merge requires all green (the stub check is skipped on doc-only PRs).
- **Removed in v0.55:** `Benchmark regression (advisory)` (#692) — failed on most PRs from runner variance and signal-to-noise hit zero. `make corpus-bench` survives for local perf validation.
- **Wakeup timing:** Windows test queues 4–7 min behind ubuntu/mac. When polling CI, schedule a 270s wakeup (not 60s) — fits inside the 5-min cache TTL twice.
- **Stub-file convention for CHANGELOG (#694):** instead of editing `CHANGELOG.md` `[Unreleased]` directly, drop a `CHANGELOG.d/<num>.<type>.md` file with one bullet (no leading dash; assembler adds it). `<type>` ∈ {added, changed, fixed, removed}. Eliminates the rebase-conflict source that bit every concurrent-PR pair pre-v0.55.

## Repo-specific test gates

These fail when changes elsewhere don't update them in lockstep:

- **New exported `*Store` method (`db.go`):** classify in `readerRoutedStoreMethods` or `writerRoutedStoreMethods` (`db_test.go`), or `TestStore_AllExportedMethodsClassified` fails.
- **Schema migration changes:** bump `schema_version` in 5 corpus snapshot files. `make corpus-snapshot-update` regenerates them; on Windows where `make` may be unavailable, `sed -i 's/"schema_version": N/"schema_version": N+1/' testdata/corpus/*.snapshot.json`.
- **Tool-contract changes (descriptions, InputSchema):** regenerate via `go test ./internal/server -run TestToolContract -update-tool-contract`.
- **Dashboard HTML/CSS changes (`dashboard.go`):** regenerate via `go test ./internal/server -run 'TestDashboardHTMLSnapshot|TestDashboardCSS' -count=1 -update-dashboard-snapshot -update-dashboard-css-snapshot`.
- **New language extractor:** update `ast/registry.go` self-registration AND `db/corpus.go` `ClassifyCorpus` AND the v9 SQL trigger WHERE clauses. `TestClassifyCorpus_MatchesSQLTriggerRouting` is the gate.
- **Bounded-duplication advisories (CLI ↔ MCP doctor):** when adding a doctor advisory, ship the helper in BOTH `internal/server/admin.go` and `cmd/pinch/doctor.go` with a "mirrors X / must stay identical" comment. The CLI lives in package main and can't import the server package.

## JSON response invariants

- **All slice fields in tool responses must be allocated as `[]T{}`, never `var x []T`.** A nil slice marshals to `null`; consumers iterating without a null-check break. Six bugs of this class fixed in v0.9.0 (#328/#330/#332/#334/#338/#330). The pattern keeps recurring.
- Map fields are fine — `make(map[K]V)` is non-nil.
- Informal lint: `grep -n "var \w\+ \[\]map\[string\]" internal/server/` should return nothing once a handler is response-stable.

## Idioms

- **Logging:** `slog` everywhere. `log.Printf` will silence under bench `TestMain` and corrupt baselines.
- **Reader pool:** pure SELECT methods use `s.ro.Query`/`s.ro.QueryContext`; writes use `s.db.Exec`. Routing is enforced by the classification gate.
- **Symbol IDs:** always build via `db.MakeSymbolID(file, qn, kind)`. Never string-concat.
- **`InputSchema: json.RawMessage(\`...\`)` raw-string gotcha:** backticks inside the description terminate the string. Use plain text or rewrite without — bit #293 and #302.

## Build & Test

```bash
# Build (recommended — stamps version from `git describe`)
make build PINCHER_BIN=./pincher.exe     # Windows
make build                               # Linux/macOS

# Build + swap the on-PATH binary (autonomous dogfood path).
# Uses scripts/swap-active-binary.sh's rename-out trick on Windows where
# `cp` over a running .exe fails with "Device or resource busy" (#705).
#
# PREREQUISITES (per #1151 — fresh clones miss these silently):
#   1. An on-PATH `pincher` (Linux/macOS) or `pincher.exe` (Windows) must
#      exist. The script swaps the binary at that path; no on-PATH binary
#      → script exits 1. One-time bootstrap:
#        cp ./pincher $HOME/.local/bin/pincher   # Linux/macOS
#        copy pincher.exe %USERPROFILE%\bin\pincher.exe   # Windows
#   2. For "no manual /mcp" auto-pickup, EITHER (a) the running MCP
#      child must have been launched as `pincher supervised`, OR
#      (b) PINCHER_AUTO_RESTART_ON_DRIFT=1 must be set in the MCP
#      child's env. Without one of these, the swap lands on disk but
#      the running process keeps serving the old binary until manual
#      /mcp reconnect. `mcp__pincher__health` reports
#      `binary_stale: true` when the swap landed but the running
#      process didn't pick it up.
make install PINCHER_BIN=./pincher.exe   # Windows
make install                             # Linux/macOS

# Bare go build (skips version stamping — `pincher --version` reports "dev")
go build -o pincher.exe ./cmd/pinch/     # Windows, dev-stamped
go build -o pincher ./cmd/pinch/         # Linux/macOS, dev-stamped

# Manual stamp without make:
go build -ldflags="-s -w -X main.version=$(git describe --tags --dirty --always | sed 's/^v//')" -o pincher.exe ./cmd/pinch/

# Test
go test ./...
go test ./... -coverprofile=cover.out && go tool cover -func=cover.out | grep "^total"
go test ./internal/db/ -run TestGraphStats_WithData -v   # single test
go tool cover -func=cover.out | grep -v "100.0%" | sort -t'%' -k1 -n   # coverage gaps

# Pinned-corpus snapshots (#33)
make corpus-test                  # verify; runs in CI as Corpus snapshot
make corpus-snapshot-update       # regenerate after intentional changes

# Performance benchmarks (#50)
make bench                        # local feedback
make bench-index | make bench-server   # narrow scope
make corpus-bench                 # gate vs committed baseline (local-only since #692; not gated in CI)
make corpus-bench-update          # regen baselines (intentional perf changes only)

# Diagnostics & admin
pincher doctor [--json]
pincher rebuild-fts [--quiet]
pincher stats [--json] [--reset]
```

**After any schema change** rebuild `pincher.exe` and reconnect via `/mcp` so the running MCP picks up the new schema.

### Pinned-corpus snapshot policy (#33)

`testdata/corpus/<name>/` holds small hand-crafted corpora. `<name>.snapshot.json` is the committed expected output of `pincher index --json-summary`. Counts (symbols, edges, files, kinds, average confidence) are exact-match. Noisy fields (`db_size_kb`, `duration_ms`) are stripped.

Two redundant gates: `make corpus-test` (jq) and `TestCorpusSnapshot_*` (pure Go). The JSON diff IS the rationale; review it in PRs.

**`extraction_failures_by_reason` cross-cutting gate:** every snapshot pins a per-corpus map of failure reasons → counts. Healthy corpora show `{}`. A PR that bumps any count from 0 to N is a regression by default — fix the bug, don't update the baseline. Caught #69, #74, #79, #80 before they reached real corpora.

### Bench gating (#50)

`testdata/bench/<package>.bench.txt` holds committed `go test -bench` output captured at `-benchtime=2s -benchmem`. Comparator (`cmd/benchcmp/`) gates on `ns/op +20%` and `allocs/op +30%`. Phase 1: `continue-on-error: true` — see CI conventions above.

## Architecture

### Data flow

```
cmd/pinch/main.go          ← sole entry point (MCP server + optional HTTP + `pincher index` CLI)
  → db.Open()              open/migrate SQLite
  → index.New()            create indexer (holds *db.Store)
  → server.New()           create MCP server (holds *db.Store + *index.Indexer)
  → srv.StartSessionFlusher()  flush session stats to DB every 10s
  → idx.Watch()            poll projects for file changes
  → [--http :PORT]         optional REST gateway
  → mcp.StdioTransport     JSON-RPC 2.0 over stdin/stdout
```

### Three-layer storage (single `symbols` table serves all three)

| Layer | Mechanism | Query path |
|---|---|---|
| 1 — Byte-offset retrieval | `start_byte` / `end_byte` per symbol | `GetSymbol` → `ReadSymbolSource` = 1 SQL + 1 `os.File.Seek` + 1 `Read` |
| 2 — Knowledge graph | `symbols` rows + `edges` table | pinchQL → SQL via `cypher/engine.go` |
| 3 — FTS5 full-text search | `symbols_fts` virtual table + 3 triggers | `SearchSymbols` via BM25 |

All three populated in a single `ast.Extract()` call per file during indexing.

### Package responsibilities

- **`internal/db/db.go`** — SQLite store. Schema lives here as a `schema` const. Migrations in `schemaMigrations` slice — append to add. Current schema: **v29**. `symSelectFrom` is the canonical SELECT column list — update it and all scan functions together when adding columns. See `docs/REFERENCE.md` for the per-version migration history; this line was 7 versions stale before #998/#999 caught it.

- **`internal/db/corpus.go`** — `ClassifyCorpus(language, kind)` returns `code` / `config` / `docs`. **PARITY INVARIANT:** Go function and v9 SQL trigger WHERE clauses encode the same routing. `TestClassifyCorpus_MatchesSQLTriggerRouting` is the gate.

- **`internal/ast/extractor.go`** — Multi-language symbol extraction. Parser-backed AST (1.0): Go, YAML/JSON, HCL/Terraform, TOML, Bash, Markdown, Jinja2, Python (dispatcher upgrades from 0.85 → 1.0 when `python` is on PATH). Stable regex (0.85): JS/TS/JSX/TSX, Rust, Java. Approximate regex (0.70): Ruby, PHP, C/C++, C#, Kotlin, Swift, Scala, Lua, Zig, Elixir, Dart, R (the last six promoted from stub in v0.63 per #1161/#1186/#1187). Stub (0.0): Haskell (indentation-sensitive layout requires harder regex representation; tracked separately). The shared `regexExtractor` framework supports `scopeRE` — a syntactic-grouping container that scopes inner symbols' Parent without emitting its own Class symbol (Rust `impl` / Swift `extension`, v0.67 #1183 partial).

- **`internal/ast/registry.go`** — `Extractor` interface + per-language registry. Each extractor self-registers in `init()`.

- **`internal/ast/blocklist.go`** — `ShouldSkip(path)` filters lockfiles, minified bundles, source maps before extraction. Belt-and-suspenders relative to `gocodewalker`'s `.gitignore` respect.

- **`internal/cypher/engine.go`** — pinchQL-to-SQL translation. `tokenize` → `parseQuery` → `run`. Three paths: `runNodeScan` (no edge), `runJoinQuery` (single-hop SQL JOIN), `runBFS` (variable-length Go BFS). `symRow` and SELECT queries must stay in sync with `db.go`'s `Symbol`.

- **`internal/index/indexer.go`** — Indexing pipeline. Concurrent per-file goroutines, xxh3 hash skip, batch flush. Per-file `DeleteSymbolsForFile` before re-extraction. Per-project mutex + cross-process `acquireProjectLock`. Tail GC pass (#326): files no longer on disk get their symbols + file_hash row pruned. After `wg.Wait`, `resolveImports` / `resolveCalls` / `resolveReads` run project-wide for cross-file Go edges. `Watch()` polls 2s active / 30s idle.

- **`internal/index/lockfile.go`** — Cross-process project lockfile with PID liveness + 24h stale reclaim.

- **`internal/index/bloat_trap.go`** — `IsBloatTrap(absPath, hookMode)` refuses fs root and `$HOME`; in hook mode also requires a project marker (`.git`, `go.mod`, `package.json`, etc.). Lives in `internal/index` (moved from `cmd/pinch` in #790) so both the CLI entry point AND the MCP `index` tool handler share the guard.

- **`internal/server/server.go`** — MCP server + HTTP REST gateway. All tools registered in `registerTools()`. Every handler calls `jsonResultWithMeta()` which wraps result in `_meta` and atomically increments session stats. `StartSessionFlusher` flushes every 10s. `cypher.Executor` is initialised with `ProjectID` so all query paths are scoped.

### Symbol ID format

```
"{file_path}::{qualified_name}#{kind}"
e.g. "internal/db/db.go::db.Open#Function"
```

IDs are stable across re-indexing as long as file path and qualified name don't change. Built by `db.MakeSymbolID()`. File moves resolve via `symbol_moves` table.

### Schema migration pattern

1. Append a SQL string to `schemaMigrations` in `db.go`.
2. Update the `Symbol` struct field, `symSelectFrom` const, and all scan functions (`scanOneSymbol`, `scanSymbolRowsRow`, `scanSymbolRow`) together.
3. Update `cypher/engine.go`'s `symRow` struct and SELECT queries.
4. Update `ast/extractor.go`'s `ExtractedSymbol` and `indexer.go`'s symbol construction if the field originates in extraction.
5. Bump `schema_version` in 5 corpus snapshot files.

### Key invariants

- `db.SetMaxOpenConns(1)` — SQLite is single-writer; writes serialize at the connection pool level.
- WAL mode + `_busy_timeout=5000` — readers don't block writers.
- WAL bounding: `journal_size_limit=256 MiB` plus `CheckpointTruncate()` at every `Index()` tail. (`wal_autocheckpoint=100` was tried and reverted — 14.5× slowdown on heavy single-writer indexing.)
- Cross-process project lock serializes concurrent indexers.
- Stale-symbol cleanup on every per-file goroutine; tail-pass GC for files removed from disk (#326).
- Go cross-file resolution scoped to confidence-1.0 extractors; regex-extracted languages keep per-file resolution.
- FTS5 triggers auto-sync the virtual tables; never sync manually.
- `flushBuffers` fires at 500 symbols or 1000 edges to bound memory.
- Symlink safety relies on `gocodewalker`'s default (v1.5.1, audited #41 item 3): symlinks are reported as paths, NOT recursed. Pinned by `internal/index/symlink_safety_test.go`.

## Dependencies

- `github.com/modelcontextprotocol/go-sdk v1.4.0` — MCP framework
- `modernc.org/sqlite` — pure-Go SQLite (no CGO)
- `github.com/boyter/gocodewalker` — `.gitignore`-respecting walker
- `github.com/zeebo/xxh3` — fast content hashing
- `gopkg.in/yaml.v3`, `github.com/BurntSushi/toml`, `github.com/hashicorp/hcl/v2`, `mvdan.cc/sh/v3`, `github.com/yuin/goldmark`, `github.com/nikolalohinski/gonja` — language parsers
- `github.com/tiktoken-go/tokenizer` — cl100k_base BPE for token-savings accounting

## Known Architectural Limitations

- **Regex gap:** ~13 non-Go languages still regex-extract (~80% accuracy). Tracked in #266 (JS AST), #268 (multi-language AST roadmap).
- **YAML/JSON sequence-rename ID instability** (#205, won't-fix for v0.7.0): inserting at index 0 renames every downstream symbol's QN. Workaround: search by name rather than id. Full content-hash-ID fix is v0.8/v1.1+ territory.
- **Single-user SQLite:** symbols + sessions are local-only. Team mode would need a server with shared DB or PostgreSQL backend.
- **HTTP auth:** `--http` supports optional `--http-key <token>` bearer auth. Without it, the API is open — front behind a reverse proxy or set the key for production.
- **`symbols` batch cap:** `maxBatchSymbols = 100`.
