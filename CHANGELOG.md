# Changelog

All notable changes to pincherMCP. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/) — once 1.0 ships, schema
breaking changes will be major bumps and tool-contract additions will be
minors.

## [Unreleased]

### Fixed
- **All tools: unknown args surface in `_meta.warnings` instead of
  silent ignore ([#499](https://github.com/kwad77/pincher/issues/499)).**
  Pre-fix, calling `neighborhood id=... depth=1` silently dropped the
  `depth` arg (it isn't in `neighborhood`'s schema) and returned a
  same-file paginated result the agent didn't expect. Same failure
  family as #473 (typo'd pinchQL properties): silent ignore is the
  bug; surfacing the typo + listing accepted args is the fix. Per-tool
  arg allow-lists are computed once from the registered InputSchema
  on first call. Adds zero overhead when args are valid (pure map
  lookups).
- **changes: blast radius now intersects diff hunks with symbol line
  ranges ([#502](https://github.com/kwad77/pincher/issues/502)).**
  Pre-fix, every symbol in any changed file was treated as "changed"
  — adding 3 functions to a 6000-line file inflated `changed_symbols`
  to 240 and the BFS to 472 critical-impacted symbols, producing a
  345 KB response that didn't fit in agent context. Now `changes`
  fetches `git diff --unified=0` alongside `--name-only` and parses
  hunk headers (`@@ -old +new @@`); symbols whose `[StartLine,
  EndLine]` doesn't overlap any hunk are dropped. On the same 3-PR
  workload: `changed_symbols` drops from 240 → 3, payload from
  345 KB → ~3 KB. Fallback preserved: if `git diff --unified=0`
  fails (e.g. permissions), the per-file all-symbols behaviour
  kicks back in so the tool stays usable.
- **dead_code precision: Go init / TestMain / main filtered
  ([#492](https://github.com/kwad77/pincher/issues/492)).** The static
  CALLS graph cannot see runtime-invoked callers (Go init() called at
  package load, TestMain called by `go test` discovery, main called
  by the runtime). These symbols are necessarily false positives in
  dead_code — they have no inbound edges by definition, yet are
  always reachable. Filter is language-gated (Go only) and name-list
  bounded to avoid hiding legitimately-dead symbols in other
  languages. Interface-dispatch false positives ([#493]) need a
  separate satisfaction-analysis pass; not addressed here.

## [v0.17.0] — 2026-05-11 — honest savings + failure-as-pedagogy

Minor — the v0.17 theme is "honest savings + failure-as-pedagogy."
Pincher's pitch is the cost story; this release makes the displayed
`tokens_saved` defensible by removing the heuristic-fabricated baseline
and the misleading `cost_avoided` $-figure (we don't know the user's
model or pricing). Plus two failure-surface fixes: the pinchQL engine
now warns on typo'd property names instead of silently returning 0
rows, and `trace` accepts an exact-symbol `id` arg as the
disambiguation escape hatch promised by the ambiguous-match hint.

### Changed
- **Honest `tokens_saved` counter
  ([#476](https://github.com/kwad77/pincher/issues/476),
  [#478](https://github.com/kwad77/pincher/issues/478),
  [#479](https://github.com/kwad77/pincher/issues/479)).** Two
  inflation sources removed:
  - Per-session `accessed_files` dedup: the second `context`/`symbol`
    call against the same file in a session claims zero baseline
    (file is already in the agent's context window), not a fresh
    full-file save.
  - Fabricated `savedVsFullRead(count × avgFileSize)` baseline
    eliminated. `handleQuery`, `handleDeadCode`, `handleChanges` now
    harvest real file paths from result rows and pass them through
    the honest `savedVsFileSizesSession` path. When no `file_path`
    column was projected (`handleQuery` on arbitrary
    `RETURN n.name` shapes), `tokens_saved` is 0 — honest "I can't
    tell what files you'd have read" beats a guess.
  Net effect: cumulative `tokens_saved` on a typical session is
  30-50% lower than v0.16.0 reported. The displayed number now
  reflects file-bytes-the-agent-would-have-read, not heuristics.
- **No more `$-cost figures
  ([#476](https://github.com/kwad77/pincher/issues/476)).**
  `cost_avoided` removed from every response envelope, `stats`
  output, dashboard, README, and tutorials. We don't know the
  user's model or pricing — a hardcoded `baseCostPer1M = 3.0`
  assumed Sonnet, but users on Opus / Haiku / GPT / open-source
  models all saw guesses. Tokens are concrete; dollars were not.
  DB `cost_avoided` column kept (always 0 going forward) to avoid a
  schema bump; readers no longer surface it.

### Fixed
- **`trace` ambiguous-match hint references a real escape hatch
  ([#474](https://github.com/kwad77/pincher/issues/474)).** Old
  hint promised "Pass an exact ID via TraceByID." `TraceByID` was
  an internal Go method, not an MCP tool — an agent that took the
  hint at face value failed. Two changes: `trace` now accepts an
  `id` argument (exact-symbol seed; bypasses name resolution and
  the ambiguous_match meta), and the hint text now references that
  parameter with a concrete next-call example.
- **pinchQL surfaces unknown-property warnings instead of silently
  returning 0 rows
  ([#473](https://github.com/kwad77/pincher/issues/473)).** A
  WHERE referencing a typo'd property (`n.typo_name = "x"`) used to
  evaluate to undefined → falsy → 0 rows, no diagnostic. The engine
  now walks the parsed query, collects every property name not in
  the `cypherPropToCol` allowlist, and surfaces them via
  `Result.Warnings` (and `_meta.warnings` at the MCP boundary).
  Walker covers WHERE conditions (flat AND-chain + recursive
  tree), inline match braces (`MATCH (n:Function {foo:"x"})`),
  and RETURN projections. Non-breaking — query still runs; this
  is a non-fatal advisory, not an error.

### Known limitations
- **Pre-#465 polymorphic-method CALLS edges persist until
  `pincher index <path> --force`
  ([#475](https://github.com/kwad77/pincher/issues/475)).** v0.16.0
  added `isPolymorphicInterfaceMethodName` to stop bare-name CALLS
  resolution for `String` / `Error` / `Read` / etc. New edges
  follow the new rules, but existing false-positive edges in older
  DBs don't auto-clean. Recommended migration after upgrading to
  v0.17.0: run `pincher index <path> --force` once per project to
  re-extract symbols + edges from scratch. Atomic project-wide
  edge replace (Option B from #475) deferred to v0.18.0 — needs
  proper separation of resolve-pass vs per-file edges first.

## [v0.16.0] — 2026-05-11 — structural perf + dogfood haul

Minor — schema v19, watcher correctness, pinchQL property surface, BFS
planner, supervised-respawn observability, and seven dogfood-driven
precision fixes. Eight new MCP-tool features unlocked by the property
surface (canonical "find undocumented exported APIs" query) and BFS
planner inversion. The release closes 20 issues from the v0.15.x
backlog; #423 (function-typed field call resolution) bumped to v0.17.0
since it requires receiver-type tracking — a substantial extractor
change to be tackled in its own dogfood loop.

### Fixed
- **Watcher incremental re-index drops cross-file Go CALLS edges
  whenever the caller's file is hash-skipped — full fix
  ([#427](https://github.com/kwad77/pincher/issues/427),
  [#457](https://github.com/kwad77/pincher/issues/457)).** The v0.15.6
  one-hop referencer invalidation (#456) only caught direct callers;
  transitive ripples still dropped edges from files that were
  hash-skipped because their content didn't change. Schema v19 adds
  a `pending_edges` table that persists each file's deferred edge
  candidates (CALLS / IMPORTS / READS / WRITES). The per-file
  extraction goroutine `DELETE`s + bulk-`INSERT`s its candidates;
  re-resolution at the end of each `Index()` call sources the FULL
  set from the table (via `LoadPendingEdges`), so candidates from
  hash-skipped files survive across runs. The tail-pass GC deletes
  rows for files removed from disk so stale candidates don't leak.
  Resolves the transitive edge-loss the v0.15.6 partial fix couldn't
  reach; the watcher no longer needs `force=true` to stay correct.

### Added
- **Session counters survive supervised respawn
  ([#420](https://github.com/kwad77/pincher/issues/420)).** `pincher
  supervised` now stamps a stable `PINCHER_SESSION_ID` once per
  supervisor lifetime and propagates it to every inner spawn. The
  server reads the env var on startup and, if a `sessions` table row
  already exists for that ID, seeds the in-memory counters
  (`calls`/`tokens_used`/`tokens_saved`/`queries_*`/per-language map)
  from the prior flush. Flushes use `INSERT OR REPLACE` on the same
  key so no double-counting. Counters that previously reset to zero
  on every binary swap now continue across respawn, surfacing the
  cumulative value an agent expects across a single MCP session.
  `sessionStartedAt` is also restored so uptime reflects supervisor
  lifetime, not inner lifetime. Pairs with the v0.16.0 `Process up:`
  line in stats (#420 partial fix, already merged).

- **`pincher.supervisor.status` surfaces `tools/list_changed` delivery
  counters ([#429](https://github.com/kwad77/pincher/issues/429)).**
  Three new fields: `tools_list_changed_emitted`,
  `tools_list_changed_emit_failed`, `last_tools_list_changed_emit_at`.
  Lets an agent confirm the supervisor IS doing its part (notification
  pushed) even when the client doesn't honour the notification. The
  README's *Known limitations* section now documents the client matrix:
  Cursor / Codex / Zed honour the notification and re-list tools live;
  Claude Code (as of this writing) does not, so binary swaps that add
  tools still require a fresh session in that client.

- **`guide` recognises structural-audit tasks and routes them to
  pinchQL `query` instead of BM25 search
  ([#467](https://github.com/kwad77/pincher/issues/467)).** Tasks like
  "find an undocumented exported function" used to receive a generic
  `search query="undocumented exported"` recommendation — which
  matches nothing useful in BM25 because the user is asking about the
  *absence* of a docstring, not the literal phrase. A new `shapeAudit`
  intent catches "undocumented", "no docstring", "missing comment",
  etc., and recommends the canonical query: `MATCH (n:Function) WHERE
  n.docstring IS NULL AND n.is_exported=true RETURN ...`. Builds on
  #438 (which exposed the docstring/is_exported properties to
  pinchQL).

### Fixed
- **`trace` and `architecture` attributed every `.String()` (and other
  polymorphic-interface) call in the project to the single local
  Method with that name
  ([#465](https://github.com/kwad77/pincher/issues/465)).** On
  pincher-repo, `trace name="String" inbound` returned 30 spurious
  "callers" — `formatStats`, `runUpdateCLI`, `markdownSlug`, etc. —
  none of which reach the lone `*bytesCollector.String` Method.
  They're calling `time.Time.String`, `bytes.Buffer.String`,
  `*url.URL.String`, etc. The receiver-method fallback (#285) saw
  ToName="localVar.String" → QN miss → 1 project Method named String →
  bind. #410's `isStdlibReceiver` only blocked the case where the
  receiver itself was a stdlib package; this fix adds the parallel
  `isPolymorphicInterfaceMethodName` blocklist for `String`, `Error`,
  `Read`, `Write`, `Close`, `Lock`, `Unlock`, `Len`, `Less`, `Swap`,
  `ServeHTTP`, `MarshalJSON`/`UnmarshalJSON`, etc. — method names
  that overwhelmingly resolve to stdlib interfaces in real Go. The
  blocklist drops genuine cross-package calls to local `String()`
  methods too; documented under-counting trade-off, no better fix
  without receiver-type tracking (#423).

- **Variable-length BFS timed out at 10s when only the end-target had a
  predicate ([#426](https://github.com/kwad77/pincher/issues/426)).**
  `MATCH (a)-[:CALLS*1..3]->(b) WHERE b.name="X"` enumerated up to 100
  fromVar candidates and ran a 3-hop recursive CTE per start — fan-out
  exploded on a 2k-Function corpus and tripped the deadline before any
  results came back. Planner now detects the asymmetric-selectivity
  shape (constant predicate on toVar, none on fromVar) and inverts the
  walk: seed from the b-match, walk inbound, project the result in
  original orientation. Same answer, milliseconds instead of seconds.
  Mirrors the speed of the equivalent `trace direction=inbound` call
  that previously had to be hand-translated.

- **pinchQL couldn't see `docstring`, `signature`, `return_type`, or
  `is_test` properties — `WHERE n.docstring IS NULL` matched every
  Function, `IS NOT NULL` matched none
  ([#438](https://github.com/kwad77/pincher/issues/438)).** The cypher
  engine's row map didn't carry those columns even though they live in
  the `symbols` table. `n.docstring` evaluated to undefined for every
  hit, so the in-Go IS NULL path took the all-match branch. Fix loads
  the four columns through every code path (node scan, JOIN, BFS) and
  exposes them in `symRowToMap` as nullable values so IS NULL / IS NOT
  NULL distinguish unset from empty. `cypherPropToCol` now pushes them
  to SQL too, so the predicate filters at the table rather than after
  the scan LIMIT. Unlocks the canonical "find undocumented exported
  APIs" query.

- **`index` diagnosis conflated benign symbol-neutral re-indexes with
  extractor bugs ([#425](https://github.com/kwad77/pincher/issues/425)).**
  When `skipped > 0 AND files > 0 AND symbols == 0` — the normal case where
  an incremental run reprocesses files whose edits didn't add/remove
  declarations (comments, whitespace, body-only changes) — the diagnosis
  read "files were processed but no symbols extracted" and pointed at
  language-detection. Agents that followed that hint chased a non-bug.
  Diagnosis now splits the three zero-symbol cases at the source:
  incremental-symbol-neutral, all-unchanged-cached, and extractor-missing
  each get distinct text + hint.

## [v0.15.6] — 2026-05-11 — dogfood-driven hygiene patches

Patch — seven fixes from a continuous dogfood loop. Each one came
out of *using* pincher and noticing the friction; details below.

### Fixed
- **`binary_stale_message` told the agent to `/mcp reconnect` even when
  `PINCHER_AUTO_RESTART_ON_DRIFT=1` was set
  ([#449](https://github.com/kwad77/pincher/issues/449)).** The
  supervisor was already going to respawn on the next tool call, but
  the response text said "drive the reconnect yourself" — agents
  flailed at a non-existent /mcp tool or asked the user to act. Message
  now branches on the env var: supervised path announces the auto-
  respawn, unsupervised path keeps the manual `/mcp reconnect` hint and
  surfaces the env var as the opt-in.

- **`resolveImports` / `resolveCalls` / `resolveReads` picked the
  first matching symbol non-deterministically, inflating IMPORTS edge
  duplicates across re-index runs
  ([#428](https://github.com/kwad77/pincher/issues/428)).** SQLite
  returned matching rows in implementation-defined order without an
  `ORDER BY`, so the same logical `server → db` IMPORTS edge resolved
  to *different* `(from_module_file, to_module_file)` pairs across
  runs, each landing as a fresh row under the
  `UNIQUE(project_id, from_id, to_id, kind)` constraint. The
  re-resolution wasn't idempotent. Fix picks the lexicographically
  smallest matching symbol ID — stable across runs, dedup constraint
  finally does its job. On pincher-repo: 17 IMPORTS edges with visible
  duplicates → 13, no duplicates.

- **Multi-token unquoted `search` queries silently returned 0 even
  when each term existed
  ([#453](https://github.com/kwad77/pincher/issues/453)).** FTS5
  defaults to implicit AND between bare tokens; queries like
  `Watch poll` failed because no single symbol matched both. The
  handler now auto-retries with `" OR "` between the per-token
  sanitised tokens when the AND path returned 0 and the query wasn't
  user-quoted / didn't use an explicit operator. Surfaces
  `_meta.and_fallback_to_or=true` and `_meta.effective_query` so the
  agent knows what recovered. `diagnoseEmptySearch` also stops
  blaming `min_confidence` for the multi-token case.

- **Explicit FTS5 `OR` / `AND` / `NOT` operators got phrase-wrapped
  and silently neutralised
  ([#452](https://github.com/kwad77/pincher/issues/452)).** The #424
  safety net for prose-with-capitalised-operators (`handle AND NOT
  context`) was too aggressive — it also collapsed `Watch OR poll` and
  `auth* OR oauth*` into phrase searches. New
  `looksLikeDeliberateFTS5Expr` gate distinguishes the two: short
  query, identifier-shaped tokens, plus a code-not-prose signal
  (CamelCase / `.`/`-`/`_` / `*` suffix) lets the operator semantics
  pass through. All-lowercase prose still phrase-wraps.

- **Watcher dropped ~7% of cross-file edges on every fire
  ([#427](https://github.com/kwad77/pincher/issues/427), partial fix).**
  When file F changed, `DeleteSymbolsForFile(F)` cascade-deleted
  incoming edges from referencer files G/H/I; G/H/I were hash-skipped
  this run, so resolveCalls never re-collected their deferred edges
  to rebuild the cross-file relations to F. New
  `db.Store.FilesWithEdgesToFile` + `Indexer.invalidateReferencers`
  clear referencer hashes pre-Index, restoring the one-hop case.
  Full transitive fix tracked in [#457](https://github.com/kwad77/pincher/issues/457)
  via a persisted-deferred-edges table.

- **`changes scope=unstaged` returned untracked files instead of
  working-tree-modified files
  ([#422](https://github.com/kwad77/pincher/issues/422)).** The tool
  description's scope ladder pinned "(includes untracked)" to `all`
  alone, but the implementation folded untracked into both `unstaged`
  and `all`. Agents calling `changes` before a commit could see only
  untracked dotfiles when real edits sat unanalysed — `tests_to_run`
  then read "nothing to test, ship it". Fix moves the
  untracked-merge into the `all` branch only.

- **`list` defaulted to showing zero-edge worktree projects, crowding
  the orientation view
  ([#419](https://github.com/kwad77/pincher/issues/419)).** Dev
  machines with `.claude/worktrees/{adj-sci}` slugs from concurrent
  agent runs had 30+ empty-graph entries pushing the real project off
  the default 50-row page. New `min_edges` parameter (default 1) drops
  projects without a usable graph; pass `min_edges=0` for the legacy
  unfiltered shape.

## [v0.15.5] — 2026-05-11 — indexer cross-language scoping

Patch — closes the cross-language false-positive class in the
indexer. Same root-cause family as #410 (stdlib receiver), but
upstream of the resolver: name lookups themselves were
language-blind.

### Fixed
- **`READS` / `WRITES` edges crossed language boundaries
  ([#436](https://github.com/kwad77/pincher/issues/436)).** On
  pincher-repo's mixed Go/JSON/YAML/Markdown corpus, ~8% of the
  graph's edges resolved a Go identifier read to a same-named
  YAML key (or JSON setting, or Markdown heading) — silent noise
  that made `trace` and `query` results unreliable for any name
  collision across language boundaries. `lookupNameInLang` now
  filters name-lookup candidates by source symbol's language;
  belt-and-suspenders, the resolver also drops resolved edges
  where `from.lang != to.lang`. Re-indexing recommended on
  upgrade — the binary-version drift detector (#304) catches
  the mismatch on the next `health` call and prompts re-index.

## [v0.15.4] — 2026-05-11 — pinchQL bool predicates + aggregations + WITH/chained-edge rejection

Patch — five fixes from the v0.15.0 autoresearcher dogfood loop,
all in pinchQL. Closes the bool-coercion gap (sibling to #412 /
#430 / #434), implements the missing aggregation set, and turns
two silent-failure parser holes into explicit errors.

### Fixed
- **`WHERE n.is_entry_point="1"` returned 0 rows even when entry
  points existed ([#421](https://github.com/kwad77/pincher/issues/421)).**
  Two compounding bugs: `is_entry_point` and `is_exported` weren't
  mapped in `cypherPropToCol` (silent in-Go post-filter where
  `fmt.Sprint(true)="true" != "1"`); even after pushing to SQL,
  `"true"`/`"false"` string literals don't convert under SQLite
  affinity for INTEGER columns. Fix: `is_exported`,
  `is_entry_point`, `complexity`, `extraction_confidence`,
  `start_byte`, `end_byte` now map to their SQL columns;
  `condLeafToSQL` coerces `"true"`/`"false"` bind args to
  `"1"`/`"0"` when the target column is bool-typed (`isBoolCol`).
  The TRUE/FALSE/NULL keyword literals normalize to `"1"`/`"0"`/`""`
  (was `"true"`/`"false"`/`"null"`) so SQL push and in-Go fallback
  agree. New `boolCoerceEqual` in `evalCondition` handles the same
  equivalence for callers that bypass pushdown.

- **SQL LIMIT clamp under-scanned when the WHERE tree fell to
  in-Go evaluation ([#435](https://github.com/kwad77/pincher/issues/435)).**
  When `filter != nil` (e.g. `=~` regex predicate that can't push
  to SQL), the row-scan cap was still `maxRows*2` — too tight on
  real corpora. `scanLimitFor` now scales to `maxRows*50` (clamped
  10000) when an in-Go filter is active, so regex WHERE returns
  matching rows on a 4000-symbol corpus instead of stopping at
  row 400. Bounded so a 1M-symbol DB doesn't burn the whole
  symbols table.

- **`WHERE n.is_entry_point` (no `=value`) returned a useless
  operator error ([#431](https://github.com/kwad77/pincher/issues/431)).**
  Naked bool predicates now evaluate as truthy (`is_entry_point`
  true → row matches, false → row drops). And when the user does
  use an operator that's not supported on the property, the error
  message lists the supported ops for that type instead of
  `unknown operator`.

- **`RETURN AVG(n.complexity)` / `MIN` / `MAX` / `SUM` returned
  200 `NULL` rows instead of one aggregate value
  ([#432](https://github.com/kwad77/pincher/issues/432)).** Only
  `COUNT(*)` was wired up. The aggregation pipeline now recognises
  `AVG`, `MIN`, `MAX`, `SUM` over numeric columns and returns a
  single result row per query, matching Cypher semantics.

- **`WITH` clauses and chained-edge patterns silently returned
  garbage ([#433](https://github.com/kwad77/pincher/issues/433)).**
  `MATCH (a)-[:CALLS]->(b)-[:CALLS]->(c)` and any query containing
  `WITH` were tokenized but ignored — the parser dropped the
  intermediate clauses without warning, then projected `NULL` for
  unbound variables. Both shapes now fail-fast with a clear
  parse error pointing at the unsupported construct.

## [v0.15.3] — 2026-05-11 — pinchQL comparison-operator pushdown

Patch — closes the third silently-undercounting pushdown gap in
the pinchQL engine (after #412 fixed `id`-equality and #430 fixed
OR / paren / NOT trees).

### Fixed
- **Comparison operators (`>`, `<`, `>=`, `<=`, `<>`) now push to
  SQL ([#434](https://github.com/kwad77/pincher/issues/434)).** The
  pushdown gate excluded the comparison family, so a query like
  `WHERE n.start_line > 4000` scanned the first `maxRows()*2 = 400`
  rows from the symbols table and post-filtered in Go. When the
  matching rows lived past that clamp (every late-file symbol on
  any 4000+ line project), the result was silently 0. Same bug
  class as #412 / #430.

  Comparison operators now emit parameterised SQL (`col >= ?`).
  SQLite affinity converts the bind arg to the column's declared
  type, so numeric WHERE works against `start_line`, `end_line`,
  `complexity`, `extraction_confidence`, and any future numeric
  column with no extra plumbing. `<>` is special-cased to include
  NULL rows (`col IS NULL OR col <> ?`) — matches the prior
  in-Go semantics. Composes with the #430 OR pushdown so
  `WHERE start_line > 4000 OR start_line < 10` is one SQL clause.

## [v0.15.2] — 2026-05-11 — pinchQL OR pushdown + changes scope validation

Patch — two correctness fixes from the v0.15.0 dogfood loop.

### Fixed
- **pinchQL OR-chain returned 0 rows when matches sat past the SQL
  LIMIT clamp ([#430](https://github.com/kwad77/pincher/issues/430)).**
  `MATCH (n) WHERE n.file_path ENDS WITH ".js" OR n.file_path ENDS WITH ".jsx"`
  returned 0 rows on pincher-repo even though the .js branch had 8
  matches. Root cause: when the WHERE tree contained an `OR` (or
  paren / NOT-group), `pushdownAllowed` returned false and the
  engine fell to in-Go evaluation. The SQL scan still had the
  `maxRows()*2 = 400` safety clamp applied, so on a 4000-symbol
  corpus the matching rows past the clamp never reached the in-Go
  filter. Fix: added `whereExprToSQL` that recursively converts the
  full WHERE tree (OR / paren / NOT included) to SQL when every
  leaf uses a known column and a pushable operator (`=`, `CONTAINS`,
  `STARTS WITH`, `ENDS WITH`, `IS NULL`, `IS NOT NULL`). SQL
  handles OR natively so the LIMIT clamp becomes safe again.
  Falls back to the previous Go path only when a leaf has an
  unsupported operator (`=~`, `>`, `<`, `>=`, `<=`, `<>` — those
  remain in scope for #434).
- **`changes scope=<typo>` silently returned empty instead of
  erroring ([#437](https://github.com/kwad77/pincher/issues/437)).**
  `scope=complete_garbage` (or any typo of the legal values) used
  to fall through to a bare `git diff`, returning an empty
  changeset that looked identical to a clean working tree. The
  agent then assumes "no changes" and ships a regression. Now
  rejects unknown scopes with `unknown scope "X"; must be unstaged
  / staged / all / base:<branch>` — same shape as the existing
  `base:<branch>` validation path.

## [v0.15.1] — 2026-05-11 — FTS5 sanitizer hardening

Patch — extends `sanitizeFTS5Query` (added by #289) to cover the full
family of FTS5-special characters that were still raising raw
`fts5: syntax error` to callers.

### Fixed
- **FTS5 sanitizer covers parens, slash, at-sign, brackets, braces,
  comma, `!`, `?`, apostrophe, and bare boolean operators
  ([#424](https://github.com/kwad77/pincher/issues/424)).** Common
  search shapes that used to crash with `SQL logic error: fts5: syntax
  error near "..."`:
  - Call expressions: `parse(query)`, `http.Get(`, `json.Marshal(rows)`
  - MCP method names / paths: `notifications/tools/list_changed`, `pkg/sub`
  - Annotations / decorators: `@deprecated`, `@Component`, `@Override`
  - Boolean prose: `handle AND NOT context`, `foo OR bar`
  - Apostrophe inside tokens: `don't`

  Per-token wrap now triggers on any of `(`, `)`, `,`, `[`, `]`, `{`,
  `}`, `@`, `!`, `?`, `/`, `'` anywhere in the token (in addition to
  `.`, `-`, `:` between alphanumerics from #289 / #356). When a bare
  uppercase FTS5 boolean operator (`NOT`, `AND`, `OR`) appears as a
  standalone token in a multi-token query, the entire query is
  phrase-wrapped so FTS5's operator parser stays out of it. Apostrophes
  inside wrapped spans are stripped to avoid the `unterminated string`
  case. Already-quoted queries (`"login flow"`) still pass through
  verbatim; lowercase `and`/`or` aren't FTS5 operators and pass through
  unchanged.

## [v0.15.0] — 2026-05-10 — Autoresearcher dogfood loop enablers

Headline: three precision wins that make the autoresearcher dogfood loop
actually productive — supervised mode now refreshes the client's tool
registry live after binary swaps, pinchQL filters by symbol id without
silently undercounting, and `guide` knows when the task references a
pincher-domain concept and points at the actual file/symbol instead of
generic search recommendations.

### Added
- **`guide` task-shape-aware recommendations + concept dictionary
  ([#397](https://github.com/kwad77/pincher/issues/397) /
  [#417](https://github.com/kwad77/pincher/pull/417)).** Three deepening
  improvements:
  - "why does X" / "why is X" / "why are X" / "why do X" route to
    `shapeUnderstand` instead of falling through to `shapeUnknown` and
    the generic architecture+search recommendation.
  - Acronym tie-break in hint extraction: when run lengths and total
    chars tie, runs with all-caps tokens (INI, MCP, FTS5, BPE) win.
    "add support for INI file parsing" returned hint=`parsing` pre-fix;
    now returns `INI`.
  - 9-pattern domain-concept dictionary maps task substrings to
    concept-aware starter recommendations: "MCP tool" → `search registerTools`,
    "schema migration" → `search schemaMigrations`, "language extractor"
    → `search registerExtractor`, etc. The shape-default workflow follows.

### Fixed
- **Supervisor emits `notifications/tools/list_changed` after respawn
  ([#407](https://github.com/kwad77/pincher/issues/407) /
  [#416](https://github.com/kwad77/pincher/pull/416)).** When
  `PINCHER_AUTO_RESTART_ON_DRIFT=1` swaps the binary on disk and the
  supervisor respawns the inner, the new binary may have added or
  removed tools — but the client's tool registry was captured at
  handshake time. The supervisor now pushes the MCP-spec
  `notifications/tools/list_changed` notification after respawn settles
  so clients re-issue `tools/list` and pick up the new surface live.
  Unblocks the in-session dogfood workflow for any release that adds
  a tool — previously only augmentations to existing tools (new args,
  new defaults) survived an in-session binary swap; *new* tools needed
  a fresh session.

- **pinchQL `WHERE n.id="X"` pushes to SQL instead of post-filtering
  ([#412](https://github.com/kwad77/pincher/issues/412) /
  [#415](https://github.com/kwad77/pincher/pull/415)).**
  `cypherPropToCol` didn't map `id` to a column, so any WHERE predicate
  on `id` was post-filtered in Go. The SQL scan still applied `LIMIT
  e.maxRows()*2`, dropping matching rows past the cut. Two queries that
  should have returned the same inbound-edge count returned different
  totals depending on scan order. Fix maps `id` to the SQL `id` column
  so SQLite uses the primary-key index AND the LIMIT only applies to
  rows that already match.

## [v0.14.0] — 2026-05-10 — Token-savings + performance focus

Headline: every read tool now supports `fields` projection so callers can
strip unused keys, the symbol→symbol round trip avoids re-resolving the
project on each call, the reader pool warms up in parallel, and `trace`
auto-trims to the smallest depth with ≥5 hops instead of always returning
the requested depth. Two correctness fixes shipped from the post-v0.13
dogfood pass — both surfaced live mid-investigation.

### Added
- **`fields` parameter on `symbol`, `symbols`, `context`, `trace`,
  `changes` ([#400](https://github.com/kwad77/pincher/issues/400) /
  [#409](https://github.com/kwad77/pincher/pull/409)).** Comma-separated
  allow-list of response keys; skipping `source` avoids the byte-offset
  disk read entirely. Typical caller savings: 60-90% per response when
  the agent only needs IDs or signatures.

- **Project-ID resolution cache + reader-pool warmup
  ([#401](https://github.com/kwad77/pincher/issues/401) /
  [#405](https://github.com/kwad77/pincher/pull/405)).** Per-`sessionRoot`
  TTL cache eliminates the `projectFromArgs` SQL hop on every tool call;
  the reader pool's connections `Ping` in parallel at server start so
  the first concurrent batch doesn't serialize on connection setup.

- **Adaptive trace depth
  ([#402](https://github.com/kwad77/pincher/issues/402) /
  [#406](https://github.com/kwad77/pincher/pull/406)).** When `depth`
  is omitted, `trace` starts at the requested ceiling and auto-trims
  to the smallest depth that surfaces ≥5 hops. Surfaces
  `depth_used`/`depth_requested`/`auto_deepened` in `_meta` so the
  caller can see what happened. Explicit `depth=N` still pins the
  depth as before.

### Fixed
- **`changes.changed_files` emits `[]` not `null` on empty diff
  ([#408](https://github.com/kwad77/pincher/issues/408) /
  [#411](https://github.com/kwad77/pincher/pull/411)).** Same nil-slice
  class as #328 / #330 / #332 / #334 / #338 — `parseGitDiffFiles` now
  initialises with `[]string{}` so consumers iterating without a
  null-check don't break.

- **Receiver-method call resolution stops over-binding to stdlib calls
  ([#410](https://github.com/kwad77/pincher/issues/410) /
  [#413](https://github.com/kwad77/pincher/pull/413)).** The #285
  receiver-method fallback bound *any* `pkg.Name(...)` call to a
  local method named `Name` when only one such method existed in the
  index. In pincher-repo this meant `strings.Index(...)` calls were
  silently bound to `*Indexer.Index`, polluting `trace` BFS results.
  New stoplist of ~70 stdlib package names skips the fallback when
  the receiver is recognized as stdlib.

## [v0.13.0] — 2026-05-10 — JS AST + tool surface expansion + dogfood-driven precision

Headline: a pure-Go JavaScript AST extractor lands behind a feature flag,
two new MCP tools join the surface (`changes scope=base:<branch>` and
`dead_code`), and the dogfood pass that surfaced four precision fixes
also caught the supervisor's Ubuntu CI flake. Total tool count: 20
(was 18 in v0.12.0).

### Added
- **JS AST extractor (behind `PINCHER_EXPERIMENTAL_JS_AST=1`)
  ([#266](https://github.com/kwad77/pincher/issues/266) / [#388](https://github.com/kwad77/pincher/pull/388)).**
  Hybrid approach: `tdewolff/parse/v2/js` gives canonical kind + name;
  regex recovers byte positions tdewolff doesn't expose. Handles
  non-spec top-level `return` via IIFE wrap-recovery; preserves
  shorthand object methods that tdewolff parses with `Property.Name=nil`.
  Behind a flag while the AST shape stabilises against real-world
  corpora; flips to default-on once the v0.13 → v0.14 dogfood pass
  confirms zero regressions vs the regex extractor.

- **`changes scope=base:<branch>` — pre-PR blast-radius preview
  ([#394](https://github.com/kwad77/pincher/pull/394)).** Three-dot
  `git diff <branch>...HEAD` semantics — answers "what does this PR
  introduce" before the PR exists. Branch-name validation rejects
  flag-injection-shape (`-rf`), range syntax (`a..b`), and shell
  metachars before the subprocess runs.

- **Multi-project `query` via `project=*`
  ([#395](https://github.com/kwad77/pincher/pull/395)).** `search`
  has supported `project=*` since v0.4-ish; `query` did not, blocking
  cross-repo graph queries like "which services import library X?"
  The Cypher Executor gains an `AllowAllProjects` flag for explicit
  opt-in; the empty-ProjectID safety guard stays as defense-in-depth
  for in-code callers that forget to scope.

- **New `dead_code` MCP tool
  ([#396](https://github.com/kwad77/pincher/pull/396)).** Surfaces
  symbols with zero inbound edges (CALLS / READS / WRITES /
  REFERENCES / IMPORTS) that aren't exported, aren't entry points,
  and aren't tests — the inverse of `architecture` hotspots. The
  first pincher tool that surfaces *removable* code, not just
  navigable code. Defaults bias toward precision
  (`min_confidence=0.95`, `kinds=Function,Method`); testdata fixtures
  and scratch paths post-filtered.

### Fixed
- **`architecture` no longer reports testdata fixtures as entry points
  ([#392](https://github.com/kwad77/pincher/issues/392) / [#393](https://github.com/kwad77/pincher/pull/393)).**
  The indexer correctly flags `testdata/corpus/.../main.go` as
  `is_entry_point=1` (it declares `package main`), but it's a
  pinned-corpus fixture, not an entrypoint of *this* project. New
  `isTestFixturePath` helper filters fixture-input directories
  (`testdata/`, `__fixtures__/`, `fixtures/`) from both `entry_points`
  and `hotspots`. Fixture symbols stay searchable via `search` /
  `query` — the filter is orientation-only.

- **`trace` filters test files + testdata fixtures by default
  ([#398](https://github.com/kwad77/pincher/issues/398) / [#399](https://github.com/kwad77/pincher/pull/399)).**
  A single inbound trace of `Open` returned **127 hops**, ~95% of them
  test functions. Same noise problem #305 + #393 solved for
  architecture; this brings trace in line. `include_tests=true` opts
  back into the legacy mixed list. Also fixes adjacent name-
  resolution bug: `sortTraceCandidates` now ranks fixtures behind
  tests, so `name=Open` resolves to `db.Open` instead of
  `testdata/corpus/.../auth.Open`.

- **Supervisor flake on Ubuntu CI
  ([#383](https://github.com/kwad77/pincher/issues/383) / [#390](https://github.com/kwad77/pincher/pull/390)).**
  `TestSupervisor_ClientStdinEOFReturns` raced: closing the fake
  inner from the test goroutine immediately after launching `Run`
  let the inner pump see EOF on the pre-closed stdout 6× before the
  client pump caught the client-stdin EOF, tripping the respawn
  circuit breaker. The "probe_timeout 50ms" lines in the failure
  trace were misdirection from a parallel test bleeding into the
  global slog stream. Removing the racy `fake.Close()` (Run's
  `shutdownInner` already closes inner pipes) made the test
  deterministic across 200× stress runs under contention.

### Performance
- **CI Windows test job: ~7min → ~3:30
  ([#391](https://github.com/kwad77/pincher/pull/391)).** Bumped
  Windows `-p` from 1 to 2 — the original SQLite-contention
  justification didn't hold (tests use unique temp dirs per package).
  Also dropped the redundant standalone `go vet` step (`go test`
  runs vet by default). All OS jobs got 20-31% faster as a side
  benefit; Coverage gate dropped ~20%. Net effect across the v0.13.0
  PR cycle: 5 PRs from open to merge in ~10 minutes per round.

## [v0.12.0] — 2026-05-10 — pinchQL parens + dogfood-driven cleanup

One feature, five fixes — every fix surfaced by a single full-surface
dogfood pass against pincher's own repo. Each one had a self-incriminating
witness: a tool description that promised behaviour the code didn't
deliver, a test pinning a silent no-op, a watcher whose top-level scope
was the only thing standing between agents and stale results.

### Added
- **pinchQL: parenthesized `WHERE` groups
  ([#362](https://github.com/kwad77/pincher/issues/362)).** The flat
  `[]condition` representation couldn't express `A AND (B OR C)` — left-
  to-right composition collapsed it to `(A AND B) OR C`. New recursive-
  descent parser builds a `whereExpr` tree (condExpr / binaryExpr /
  notExpr) so parens and `NOT (...)` are first-class. Pure AND chains
  still push down to SQL; trees with OR / parens / group-NOT route
  through Go evaluation. Fixes a latent OR bug in `runBFS` along the
  way: the start-node prefilter pushed `fromVar` equalities even when
  the WHERE was OR-joined, so `WHERE a.name='X' OR a.name='Y'` started
  from zero rows.

### Fixed
- **`search corpus=docs` no longer floors out Markdown sections by
  default ([#379](https://github.com/kwad77/pincher/issues/379)).**
  The 0.71 confidence baseline filters doc-section noise from code-
  corpus searches, but it was wrong-way-around for explicit
  `corpus=docs` calls (Markdown sections extract at 0.7-0.81). Default
  flips to 0.0 when the caller asks for the docs corpus.

- **`architecture` hotspots no longer include script-local Variables
  ([#380](https://github.com/kwad77/pincher/issues/380)).** Pre-fix,
  the top hotspot in pincher-repo was `plugin/scripts/install.js::result#Variable`
  — a JS local accumulator, with a `next_steps` recommendation to
  read its source. New `isHotspotKind` filter restricts to Function /
  Method / Class / Interface / Type / Module so the change-risk
  surface is what surfaces.

- **Watcher detects edits in subdirectories
  ([#377](https://github.com/kwad77/pincher/issues/377)).** `hasChanges`
  used `os.ReadDir(p.Path)` — top-level only. Real Go projects keep
  source under `internal/`, so edits never triggered a re-index until
  an explicit `index` call. Replaced with `filepath.WalkDir` + the
  existing `isSkippedDir` set; early-exits on first newer file.

- **`list prune_dead=true` is orthogonal to `include_dead=true`
  ([#378](https://github.com/kwad77/pincher/issues/378)).** Pre-fix
  the prune branch was nested inside `if !includeDead { ... }`, so
  combining the two silently no-op'd the prune. The natural read is
  "show dead rows AND delete them" — audit + cleanup. Now both flags
  work together; the `pruned` field reports exactly what got removed.

- **`context` returns in-file callees, not just imports
  ([#381](https://github.com/kwad77/pincher/issues/381)).** The tool
  description promised "everything it directly imports/calls" but
  `handleContext` only followed IMPORTS edges. A function calling 3
  in-file helpers got back zero callees and the agent had to chase
  each one. New `callees` field follows CALLS edges, de-duplicated
  against `imports` so a symbol that's both imported and called
  appears once. The `suggestContextNextSteps` rationale ("context
  already showed callees") finally tells the truth.

## [v0.11.1] — 2026-05-10 — Supervisor: response-loss patch

Patch release for the in-flight-response loss that broke `pincher supervised`
on binary upgrade ([#371](https://github.com/kwad77/pincher/issues/371)).

The v0.11.0 supervisor was the right design but two bugs prevented it from
working end-to-end: a server-side ordering bug that lost every post-upgrade
response (#371's load-bearing root cause), and a supervisor-side bug that
forwarded the new inner's `initialize` reply with a stale id and broke
JSON-RPC framing. Both are fixed; supervised mode now works as advertised.

This release also adds `internal/supervisor/cmd/probe` — a maintained
diagnostic harness that drives a real pincher (bare or supervised) through
the post-bump auto-restart sequence and reports per-call response delivery.
This is the harness that surfaced #371; keeping it in tree saves future
maintainers from re-deriving it.

### Fixed
- **Supervisor respawn no longer leaks a duplicate `initialize`
  response or stray startup notifications to the client (#371,
  follow-up to the server-side fix in the same milestone).** The
  supervisor now replays `initialize` to the new inner with a
  supervisor-sentinel JSON-RPC id (`__pincher_supervisor_init_<n>`),
  intercepts the matching response in the inner→client pump, and
  drops server-initiated notifications during a 500ms post-respawn
  quiet window. Without this, the new inner's response carried the
  client's *original* `initialize` id (or, in S1.5's first attempt,
  no response interception at all) and broke the client's JSON-RPC
  framing assumptions even after the in-flight response loss was
  fixed. New `TestSupervisor_InitReplayResponseIsIntercepted` pins
  the contract with a faithful echo-fake.

### Added
- `internal/supervisor/cmd/probe` — maintained out-of-band diagnostic
  harness that drives a real `pincher` (or `pincher supervised`)
  process through the post-bump auto-restart sequence and reports
  per-call response delivery. Replaces ad-hoc `cmd/test-364` and
  `cmd/test-371*` reproducers; this is what surfaced #371.

- **Supervised mode lost the in-flight response when the inner
  self-restarted on binary drift (#371).** `Server.jsonResultWithMeta`
  called `s.checkAutoRestart()` before `return result`; the production
  exitFn (`os.Exit`) is synchronous, so the function never returned,
  the SDK never serialized the response, and the supervisor saw the
  inner exit before the response reached the client. The client timed
  out and dropped the stdio session — exactly the friction the
  supervisor was supposed to eliminate. Fix: `maybeAutoRestart` now
  schedules `exitFn(0)` via `time.AfterFunc(s.autoRestartDelay, …)`
  (default 100ms in `New()`). The 100ms grace period lets the SDK
  finish writing the response before the process exits. Tests reset
  the delay to 0 in `newTestServer` so existing exit-gate assertions
  stay deterministic; new `TestMaybeAutoRestart_DeferredExit_DoesNotBlockCaller`
  pins the deferred behaviour. Unit tests with a recording exitFn
  stub didn't catch this — only an integration probe driving real
  stdio does.

## [v0.11.0] — 2026-05-10 — Supervisor: auto-respawn for agent CLIs

Closes the multi-CLI / version-drift / manual-/mcp-reconnect concerns
that surfaced during v0.10.0 dogfooding. Six PRs land together: one
build-hygiene fix that exposed how often the symptom was being missed,
one drift-refusal safety net for the once-per-upgrade window every user
hits, and four supervisor slices (S1–S4) that wrap an inner pincher
MCP server with auto-respawn + initialize-replay so disconnects
self-heal without a human typing `/mcp`.

The recommended way to invoke pincher from any agent CLI is now:

```
command = "<pincher binary path>"
args = ["supervised"]
```

For Codex specifically: `pincher init --target=codex` writes the
config block + a Codex-isolated `PINCHER_DATA_DIR` in one step.

### Added
- **`pincher init --target=codex` (S4).** Closes the v0.11.0
  supervisor plan. Adds Codex (OpenAI's CLI) as an init target. Writes
  a marker-wrapped block into `~/.codex/config.toml` (or
  `$CODEX_HOME/config.toml`) registering pincher as an MCP server with
  two key choices baked in:
  - `command = "<pincher path>"`, `args = ["supervised"]` — uses the
    S1+S2 supervisor wrapper so MCP disconnects auto-recover without
    manual `/mcp`.
  - `[mcp_servers.pincher.env]` with `PINCHER_DATA_DIR` set to a
    Codex-specific path (`%APPDATA%\pincherMCP\codex` on Windows;
    `~/Library/Application Support/pincherMCP/codex` on macOS;
    `$XDG_DATA_HOME/pincherMCP/codex` or `~/.local/share/...` on
    Linux). Per-target isolation eliminates cross-CLI DB contention
    (the multi-process drift concern from S2's design notes).

  Refuses to write when an un-managed `[mcp_servers.pincher]` block is
  already present (would produce duplicate-table TOML and break Codex).
  Action surfaces as `skipped (existing un-managed [mcp_servers.pincher])`
  with a stderr message giving the operator the exact markers to wrap
  their existing block with if they want to opt into managed updates.

  Eight new tests cover registry registration, `CODEX_HOME` resolution,
  empty-existing write, append-without-markers, in-place update,
  refuse-on-unmanaged-existing, idempotent re-run, and data-dir
  computation.

### Added
- **Supervisor health surface + `pincher health-check` CLI (S3).**
  Two pieces:
  - **`pincher.supervisor.status` MCP tool.** The supervisor intercepts
    tool calls with this name (does NOT forward to inner) and responds
    with a `SupervisorStatus` JSON: `{alive, uptime_sec, restarts,
    probes_sent, probes_answered, probes_timed_out,
    last_restart_reason}`. Probe-timeout-triggered restarts surface as
    "probe timeout (inner unresponsive)" via a one-shot reason override
    so the natural-exit case ("inner exited (code=N)") doesn't mask
    the actual cause. Out-of-band knowledge for now — the tool is NOT
    auto-injected into `tools/list` responses.
  - **`pincher health-check` subcommand.** External-watchdog probe
    (cron, launchd, k8s liveness) that spawns a pincher MCP server
    short-lived, completes initialize + tools/list within `--timeout`,
    and exits 0/1 accordingly. Supports `--binary PATH`, `--supervised`
    (probe through `pincher supervised` instead of bare), and
    `--verbose` for JSON-RPC trace. Handles MCP server-initiated
    requests like `roots/list` by replying with an empty array — the
    initial implementation deadlocked on this until #X surfaced it
    via the smoke test.

  Three new supervisor tests cover status-tool-returns-response,
  non-status-tool-passes-through, and status-reflects-restart-reason.

- **Supervisor liveness probe + circuit breaker (S2).** Builds on S1's
  `pincher supervised`. Adds two protections against pathological
  inner states:
  - **Liveness probe.** Every 30s (configurable via
    `Supervisor.ProbeInterval`), the supervisor sends a
    JSON-RPC `tools/list` to the inner with a sentinel `id`
    (`__pincher_supervisor_probe_<n>`). The inner→client pump now
    reads stdout line-by-line and intercepts probe responses (so
    they never reach the client). If the response doesn't arrive
    within 5s (configurable), the supervisor kills the inner —
    triggering the existing EOF→respawn flow. Catches "process is
    alive but stuck" cases that EOF-only respawn misses.
  - **Circuit breaker.** Restart timestamps go into a windowed ring
    buffer (default: 5 restarts within 60s). When the threshold is
    exceeded, `Run()` returns a clear error rather than hot-looping
    forever — useful when the underlying issue (corrupt DB, missing
    dep, persistent crash) can't be fixed by restarting.
  - **Bonus fix (real bug, not just a test fix):** when the
    supervisor decides to shut down internally (breaker tripped,
    unrecoverable respawn), `pumpClientToInner` was blocked on
    `Read(client.Stdin)` which context cancellation can't interrupt.
    Run now closes Stdin (when it's a Closer — os.Stdin, pipes, etc.)
    and drains the pump with a 2s timeout. Without this, supervisor
    self-shutdown could hang forever waiting on a client that wasn't
    talking.

  Four new tests cover probe-sent-and-answered, probe-timeout-kills-
  inner, breaker-trips-and-returns-error, and recordRestart's age-out.

- **`pincher supervised` subcommand (S1).** Runs an inner pincher MCP
  server with auto-respawn + initialize-replay, so the MCP client
  (Claude Code, Codex, etc.) sees an unbroken stdio session even when
  the inner exits — whether from `PINCHER_AUTO_RESTART_ON_DRIFT`
  firing on a binary upgrade, an unrecoverable panic, or an OS-level
  kill. Configure your MCP client to invoke `pincher supervised`
  instead of `pincher`, and the manual `/mcp` reconnect dance
  disappears for the disconnect cases pincher can detect itself.
  Currently MVP scope: spawn/forward + replay captured initialize and
  notifications/initialized on inner exit. Known limitation: requests
  in flight during the ~100ms respawn window may be lost (no buffered
  retry yet). Liveness probe + circuit breaker (S2) and a
  supervisor-level health meta tool (S3) follow in subsequent PRs.
  Implementation in `internal/supervisor/`. Five integration-style
  tests using a fake-inner pipe pair cover forward, init capture +
  replay, client-EOF clean shutdown, spawn-failure error, and
  non-init-line non-replay.

- **Bidirectional binary-version drift detection (F1).** The existing
  `index_drift` flag in `health` only catches one direction (newer
  server on older-indexed project — informational). The reverse case —
  an older pincher binary running against a project a newer binary
  already touched — was silent until now, even though it can produce
  inconsistent results when the older binary's extraction logic differs
  from what produced the stored symbols. Two-way fix:
  - `index` (and other write-class tools) refuse cleanly with a
    diagnostic naming both versions when the project was stamped by a
    newer binary. Prevents older parsing logic from rewriting data the
    newer binary correctly produced.
  - Read-class tools (search, architecture as the high-traffic
    starters; more handlers to follow) attach a
    `_meta.binary_version_warning` so agents can see the inconsistency
    and decide whether to trust the result. Reads continue (refusing
    would be too aggressive for the once-per-upgrade window every user
    hits).
  - Drift detection is no-op when either side is dev/unstamped or
    unparseable — the bias is conservative against false positives.
  - Normalization strips git-describe and `-dirty` suffixes so dirty
    builds of a release don't falsely flag drift against the same
    release.
  Implementation in `internal/server/drift.go` (140 lines including
  comments + 11 tests). Mostly relevant during version transitions and
  multi-process scenarios where two pincher binaries share a DB.

### Fixed
- **Single-source versioning + CI gate.** Local builds via bare `go build` had
  a hardcoded `var version = "0.6.0"` fallback, so `pincher --version` lied
  about the binary's actual provenance during dogfooding (the v0.6.0 string
  persisted across multiple v0.7–v0.10 sessions before being noticed). The
  default is now `"dev"`, `make build` derives the version from
  `git describe --tags --dirty --always`, CLAUDE.md documents both stamped
  and bare paths, and `release.yml` gains a post-build assertion that fails
  CI if the stamped `--version` output doesn't match the tag exactly. Caught
  via a v0.10.0 release-prep dogfood — no functional change to released
  binaries (release.yml already stamped correctly), purely closes the
  developer-build provenance gap.

## [v0.10.0] — 2026-05-10 — pinchQL hardening, drift recovery, language coverage

> Note: v0.7.0, v0.8.0, and v0.9.0 were retro-tagged from existing
> master commits without per-version CHANGELOG entries. The work that
> shipped under those tags (~75 commits since v0.6.0 — JSON-shape
> sweep, JS/TS regex hardening, pinchQL operator additions, search
> sanitization, drift detection, etc.) is consolidated under this
> v0.10.0 entry. Future releases follow the per-version section
> convention from the start.

### Fixed
- **Auto-restart on every tool call, not just `health`** (#364,
  follow-up to #352). When `PINCHER_AUTO_RESTART_ON_DRIFT=1` was set
  and a fresh binary landed on disk, the restart only fired if the
  user happened to call `health`. Now `(*Server).checkAutoRestart`
  runs from `jsonResultWithMeta` / `textResultWithMeta` so any tool
  response is a restart opportunity. Per-call cost when env var
  unset (default): one `os.Getenv` early-exit (sub-µs); when set,
  same plus one `os.Stat` on the binary path. `sync.Once` still
  gates the actual exit. Three new tests cover the broader entry
  point.

- **OR connector in WHERE clauses** (#358, #359). `WHERE A OR B` was
  silently treated as `A AND B` — for equality on a single property
  that's always zero rows. The parser stamped no connector on
  conditions and matchesConditions evaluated as conjunction. Fixed
  with documented left-to-right composition for mixed AND/OR. Pure-AND
  queries still SQL-pushdown unchanged (the common case).

- **`LIMIT 0` returned arbitrary rows; missing MATCH/RETURN silently
  parsed** (#360, #361, #363). Two grammar correctness bugs found by
  dogfooding the parser surface: `LIMIT 0` clamped to nothing instead
  of returning zero rows, and queries missing `MATCH` or `RETURN` were
  accepted instead of producing a parse error. Both gated now with
  regression tests.

- **`search` colon-bearing queries leaked SQLite errors** (#356, #357).
  An `a:b` query hit FTS5's column-prefix syntax and surfaced "no
  such column: a" to the agent. Sanitizer now wraps colon-bearing
  tokens so they're searched as text rather than treated as a column
  selector.

- **`pinchql` NOT prefix on conditions** (#354, #355). `WHERE NOT x`
  was previously rejected as "unsupported operator: <varname>"; now
  parsed as a first-class unary negation on the condition.

- **Self-restart on schema/binary drift** (#352, #353). Opt-in via
  `PINCHER_AUTO_RESTART_ON_DRIFT=1`. When `health` (now any tool —
  see #364 above) detects index drift AND the on-disk binary's mtime
  has advanced past startup-mtime, the server exits 0; Claude Code's
  MCP transport respawns into the rebuilt binary. `sync.Once` gates
  the exit so concurrent in-flight calls don't race.

- **`search` exact-name match across kinds** (#350, #351). When a
  `kind:` filter excluded the exact-name match, the result list was
  empty even though the symbol was in the index under a different
  kind. Fixed: surface the cross-kind match with a hint when the
  filter would otherwise hide it.

- **Implicit `GROUP BY` when mixing non-aggregate with `COUNT`** (#348,
  #349). The non-aggregate column was silently dropped; now an
  implicit GROUP BY surfaces it.

- **JSON-shape sweep — empty slices marshal as `[]`, never `null`**
  (#328 health, #330 changes, #332 trace/context/architecture, #334
  search/list/sessions, #336 symbols batch, #338 query rows). Six
  separate fixes for the same recurring class of bug: a `var x []T`
  declaration marshals to JSON `null`; consumers iterating without
  null-check break. Pattern flagged in CLAUDE.md as a JSON response
  invariant — always allocate as `[]T{}`.

- **`changes` symbols stable shape** (#326, #327). Files deleted
  from disk left orphan symbols + stale `file_hash` rows. Tail-pass
  GC after `wg.Wait` prunes both per index pass.

- **Index-vs-binary version drift detection** (#304, schema v18). New
  `projects.binary_version` column captures the running binary's
  version at index time; `health` compares against `s.version` and
  emits `index_drift: true` + a `_meta.next_steps` entry pointing at
  `index --force` to refresh resolution-dependent edges.

- **Boolean equality compares case-insensitively** (#323, #324). `where
  exported = true` and `where Exported = TRUE` now compare equally;
  previously the literal-case difference yielded zero rows.

- **`IN` operator hint** (#321, #322). `WHERE x IN [a, b]` returns a
  parse error with a hint pointing at `WHERE x = a OR x = b` —
  pinchQL's supported fallback. The IN parser is on the v1.x roadmap.

- **`trace` ambiguity-tiebreaking** (#319, #320). When multiple symbols
  share a name, prefer callable kinds (Function/Method) and skip
  scratch/test files, surfacing the intended target rather than a
  test stub.

- **`symbol`/`context` warn on out-of-date file** (#317, #318). When
  the file on disk has been modified since the last index pass,
  responses now include a staleness warning so byte-offset reads
  aren't blindly trusted.

- **`next_steps` args use `json.Marshal` for proper escaping** (#315,
  #316). String args containing quotes / backslashes / unicode
  previously broke the JSON snippet a downstream agent would have
  to copy-paste.

- **`ORDER BY` numeric compare on numeric columns** (#313, #314).
  Lexical compare of strings encoded as numbers — "10" < "9" — gave
  wrong ordering for confidence/score/edges columns.

- **`pincher list --prune-dead=true` permanently removes dead-on-disk
  projects** (#302, #312). Previously the prune flag only filtered
  output; the projects came back on the next `list`.

- **`pincher index` fails fast when path doesn't exist** (#310, #311).
  Previously an empty index was created with a confusing "0 symbols"
  log line.

- **`COUNT()` returns cardinality, not LIMIT clamp** (#308, #309). When
  the query had `LIMIT N`, COUNT was capped at N instead of returning
  the true cardinality of the matching set.

- **`pincher web` auto-start on Windows** (#232). The detached child
  spawned by `web_windows.go startDetached` had no inherited console
  (DETACHED_PROCESS), so the always-on MCP stdio reader hit
  `INVALID_HANDLE_VALUE` and `log.Fatalf` tore the whole process
  down. Fixed via a new `--no-stdio` flag that `pincher web`'s spawn
  path passes; refuses to run without `--http` (the process would
  have nothing to do).

- **`pincher update` standalone-mode GitHub URL** — `updateGitHubRepo`
  in `cmd/pinch/update.go` still pointed at the pre-rename
  `pincherMCP` slug. Calls succeeded only via GitHub's repo-rename
  redirect, which would break the day someone deletes the alias.
  Bumped to canonical `pincher`.

### Added
- **Per-language call counts in `pincher stats`** (#240, schema v16).
  Surfaces "is the agent calling pincher on the file types it
  works with?" as a one-line check. The server tallies the
  `language` field on every tool response in-memory (sync.Map of
  atomic int64s keyed by language) and flushes the JSON-encoded
  map to a new `calls_by_language` column on the sessions table
  every 10 s alongside the existing call/token counters. The
  `pincher stats` text and JSON outputs render a LANGUAGES
  section between STORAGE and PROJECTS, sorted by count
  descending with a lexical tie-breaker. Pre-v16 sessions or
  v16 sessions with no language data render exactly as before —
  no empty section, no shape change. Driven by an empirical
  session-A vs session-B comparison nbarari measured (~$74k
  tokens of value present-or-absent depending solely on whether
  the agent invoked pincher); without per-language counts there
  was no way to detect bypass on a known file type. Direction
  Option A from the issue: counter columns on sessions, no
  per-call log table — promotable later if richer analytics
  warrant it.
- **`pincher index` warns on nested-under-existing-project** (#235,
  reported by @nbarari). Indexing a subdirectory of an
  already-indexed project no longer silently stores symbols twice.
  New `Store.ProjectsContainingPath(target)` finds every existing
  project whose canonical path is a strict ancestor of `target`; the
  CLI prints a stderr warning naming each parent project (with file
  + symbol counts) and a suggested `pincher project rm` command. The
  index still proceeds — silent stderr preserves scriptability per
  the chosen Option A. Catches the real-world Proxmox / monorepo
  case nbarari hit during validation: a 745MB DB with a parent
  project at 447k symbols and two nested duplicates re-storing 12k
  symbols and their FTS5 index entries.

### Changed
- **HTTP dashboard polish** (#203). `/v1/health` now exposes
  `auth_required` so the dashboard can show a one-time amber banner
  when pincher is running without `--http-key` (loopback default-deny
  still applies server-side per #199 — this is purely informational
  to flag "no auth in place" for users about to expose the API). The
  banner is dismissed-to-localStorage so it doesn't nag on reload.
  Added `@media (max-width:720px)` rules so the dashboard renders on
  phones / narrow split-pane editors: header wraps, tab nav scrolls
  horizontally, project + search toolbars stack, grids collapse to
  single column. Tutorial corrected: the previously-claimed "Tools"
  panel doesn't exist; the dashboard's actual five tabs (Overview /
  Projects / Search / ADRs / Sessions) are now documented and the
  OpenAPI spec at `/v1/openapi.json` is pointed to as the API
  explorer surface.
- **Coverage gate 84% → 85%** (#221). The remaining path-to-85%
  identified in #200's close (network-bound update paths at 0%
  coverage) closed by splitting `downloadAndSwap` into a tiny
  os.Executable-resolving outer + an `downloadAndInstallAt(out, url,
  exePath)` inner that's exercised against `httptest.Server`, plus a
  `goInstallRunner` package-level indirection so `runGoInstall`'s exec
  call can be unit-tested without shelling out. Local Linux measures
  85.2% post-#221; the 0.2pt headroom over the 85.0 floor leaves
  margin for OS-specific branches that don't fire on Linux. The
  separately-tracked `main()` bootstrap refactor remains future work
  (deferred — current 75% on cmd/pinch is enough for the gate).

### Fixed
- **`pincher web` auto-start fails on Windows** (#232). The detached
  child spawned by `web_windows.go startDetached` had no inherited
  console (DETACHED_PROCESS), so the always-on MCP stdio reader hit
  `INVALID_HANDLE_VALUE` immediately, errored, and `log.Fatalf` tore
  the whole process down — including the in-flight HTTP server,
  before the readiness probe fired. Fix: a new `--no-stdio` flag
  skips the MCP stdio loop entirely; `pincher web`'s spawn path now
  passes it. The flag refuses to run without `--http` (the process
  would have nothing to do). Same fix benefits Unix detached spawns.

### Added
- **Stale-project detection in `pincher list` and `pincher doctor`**
  (#236, reported by @nbarari). Schema migration v15 adds
  `projects.schema_version_at_index INTEGER` — stamped by
  `UpsertProject` on every index. Pre-v15 rows stay NULL
  (unknowable). `pincher list` and `pincher doctor` flag projects
  whose stamped version is below the running binary's max-known
  schema with a `[stale]` marker; doctor adds a dedicated "Stale
  projects (would benefit from re-index)" section that names each
  project with the precise reason (`indexed at v12, current is v15`)
  so users know which to re-index. The `--json` output for both
  surfaces `schema_version_at_index`, `stale`, and `stale_reason`
  fields. Closes the observability gap where long-lived indexes
  silently miss data added by later extractor or migration work
  (TOML, HTML, XML, etc.).
- **`$PINCHER_DATA_DIR` environment variable** — when set, `db.DataDir()`
  returns the env var's value verbatim instead of the platform default
  (`%APPDATA%\pincherMCP\` / `~/Library/Application Support/pincherMCP/`
  / `$XDG_DATA_HOME/pincherMCP/`). Lets a dev shell pin its pincher
  binary to a separate data dir from the user's stable install — dev
  migrations can never taint the stable DB. `--data-dir` flag still
  takes precedence (every CLI subcommand checks the flag first, falls
  back to `DataDir()` only if empty), so scripted callers that always
  pass `--data-dir` are unaffected. The fix is in `db.DataDir()` so it
  applies uniformly to every subcommand without per-callsite changes.
- **XML extractor** (#101). Pure-Go via stdlib `encoding/xml`, confidence
  1.0. Emits one `Setting` symbol per element with a hierarchical
  dotted-path qualified name (`config.database.host`); attributes become
  `parent_path@attr` Settings (`config.resource@id`). Multi-instance
  same-name siblings disambiguate via positional suffix (`<usb>` × 4 →
  `usb.0`, `usb.1`, `usb.2`, `usb.3`) — mirrors the #88 HCL fix and
  prevents the QN-collision sanity heuristic from firing on real Spring
  beans / web.xml / .csproj inputs. Namespaced elements strip the prefix
  in QN (`<android:intent-filter>` → `intent-filter`); the original
  source text survives in `Signature` so `symbol get` returns the
  literal element. Templated XML is permissively parsed — partial
  output beats no output. Routes to the `config` corpus alongside
  YAML/JSON/HCL/TOML.
- **Extension scope**: `.xml`, `.xsd`, `.xsl`, `.xslt`, `.config` (the
  .NET app/web config). Explicitly NOT `.html` (#100 owns that) and NOT
  `.svg` (the structural attribute space — `d=`, `viewBox=`, transform
  matrices — is noise from a code-search standpoint).
- Schema v14 — drop + recreate the per-corpus FTS5 sync triggers with
  XML in the config-include / code-exclude predicates so existing v13
  DBs route XML symbols to the config corpus correctly. The vtabs
  themselves are unchanged. Fresh installs hit the updated baseline
  schema directly.
- **HTML extractor** (#100). Pure-Go via `golang.org/x/net/html`,
  confidence 1.0. Emits one `Section` symbol per heading (h1–h6) with
  hierarchical dotted-path qualified names matching the Markdown
  extractor's pattern (e.g. `installation.from_source.windows`). The
  document `<title>` produces a `Section` with QN `title` so SPA-style
  pages with no h1 are still searchable. `<script src>`, `<link href>`,
  and local `<a href>` produce `IMPORTS` edges; external URLs
  (`http://`, `https://`, `//cdn.example/...`), anchor fragments, and
  `mailto:`/`javascript:`/`tel:` schemes are skipped. `id=` /  `name=`
  attributes are NOT extracted as Setting symbols (modern frameworks
  generate IDs aggressively; the noise dilutes the symbol space).
  Templated HTML is permissively parsed — partial output beats no
  output. Routes to the `docs` corpus alongside Markdown.
- Schema v13 — drop + recreate the per-corpus FTS5 sync triggers with
  HTML in both predicates so existing v12 DBs route HTML symbols to
  the docs corpus correctly. The vtabs themselves are unchanged. Fresh
  installs hit the updated baseline schema directly.

### Changed
- **`query` tool's grammar renamed Cypher-like → pinchQL** (#206).
  Same engine, same supported subset (MATCH / WHERE / RETURN /
  ORDER BY / LIMIT, single-hop joins, bounded BFS) — but the language
  now has a name we'll commit to instead of an open-ended "Cypher
  subset" framing that implied an ever-pending feature backlog. The
  MCP `query` tool's `pinchql` parameter is the new canonical name;
  the `cypher` parameter is still accepted as a soft alias for one
  release to ease transition. REFERENCE.md gains a "Why pinchQL and
  not Cypher" rationale block. `internal/cypher/` package keeps its
  filesystem name for git-blame continuity (the user-facing rename
  doesn't require an internal-name churn).
- **Two-process stats lag dropped from ≤10s to ≤1s** when an HTTP
  dashboard peer is detected (#204). The session flusher now adapts
  its cadence: 10s steady-state when running solo (no dashboard), 1s
  when another pincher process has flushed an `http_url` sessions
  row within 30s. The peer query filters by `http_pid != self`, so
  the same process running stdio + HTTP doesn't ping-pong its own
  flusher. Detection happens after every flush, so transitions land
  at most one slow-tick after the peer appears or disappears — a
  one-time settling cost, not steady-state lag. Implementation in
  `internal/server/server.go` `StartSessionFlusher`.

### Fixed
- **`pincher update` standalone-mode GitHub URL** — the
  `updateGitHubRepo` constant in `cmd/pinch/update.go` still pointed
  at the pre-rename `pincherMCP` slug. Calls were succeeding only via
  GitHub's repo-rename redirect, which would break the day someone
  deletes the redirecting alias. Bumped to the canonical `pincher`.
  Functional bug; thanks to the post-rename audit for catching it.

### Documentation
- **Post-rename audit** — fixed remaining stale references after the
  v0.5.0 `kwad77/pincherMCP` → `kwad77/pincher` repo rename:
  - `ghcr.io/kwad77/pinchermcp:latest` → `ghcr.io/kwad77/pincher:latest`
    in `docs/REFERENCE.md`, `packaging/README.md`, `RELEASING.md`. The
    release workflow has always built `ghcr.io/${GITHUB_REPOSITORY,,}`
    so the actual image since v0.5.1 has been `kwad77/pincher`; the
    docs were the only thing still pointing at the old name.
  - `https://kwad77.github.io/pincherMCP/` → `…/pincher/` in
    `docs/index.html` (og:url, og:image, twitter:image meta tags) and
    `docs/README.md`.
  - The `pincherMCP` brand name itself is preserved everywhere it's
    used as a product name (banner alt text, version output,
    REFERENCE.md title, doctor banner, ADR records). The data
    directory (`%APPDATA%\pincherMCP\`, `~/.local/share/pincherMCP/`,
    `~/Library/Application Support/pincherMCP/`) is also unchanged
    so existing user DBs survive the rename. Same for the launchd
    plist filename (`com.pinchermcp.pincher.plist`) — preserves
    install compatibility.
- **YAML/JSON sequence-rename ID instability decided as won't-fix** for
  v0.7.0 (#205). REFERENCE.md, CLAUDE.md, and README's known-limitations
  sections rewritten with the full rationale: a content-hash ID scheme
  (deterministic across reorders) is real engineering work — symbol-ID
  format change, migration path, full re-index of every existing DB —
  for a problem whose blast radius is mostly Ansible/k8s manifests,
  which are typically searched via `corpus=config` BM25 anyway, where
  qualified-name churn is invisible to FTS5. Practical workarounds
  documented (search by name rather than storing the id; prefer
  named-list YAML where the schema allows). Revisit trigger: real
  complaints with reproducible churn — v0.8/v1.1 territory.
- **Bench-regression gate decided to stay advisory** (#207). Variance
  data captured at `testdata/bench/variance-ci-2026-05-09.md` (N=10)
  shows 20 of 21 benchmarks at <10% CV but one I/O-bound outlier at
  21.5%. The standing project rule is N≥20 before flipping a
  noise-prone gate to required, and the prior promotion (#160)
  blocked a docs-only PR (#161) with an unexplainable +109% / +276%
  spike. Workflow comment in `.github/workflows/ci.yml` updated with
  the formal decision and the re-promotion checklist (capture N≥20
  across weeks, identify new noisy benchmarks for `BENCH_EXCLUDE`,
  then flip in a dedicated PR).

## [v0.6.0] — 2026-05-09 — Multi-client adoption

The "any agent, any editor" milestone. Closes the gap between
"pincher works great in Claude Code" and "pincher works great
wherever an LLM agent talks to a codebase."

Highlights:

- **Multi-IDE init writers** — `pincher init --target=...` now seeds
  policy files for six editors and agents (Claude Code, Cursor modern
  + legacy, Windsurf, Aider, Continue), not just Claude. The cursor
  modern target writes `.cursor/rules/pincher.mdc` with YAML
  frontmatter and preserves user customisations on re-runs. The
  continue target merges into `~/.continue/config.json` without
  touching unknown keys. `--target=detect` writes only to detected
  editors; `--target=all` writes every project-scoped target.
- **Three end-to-end tutorials** under `docs/tutorials/` — Claude
  Code, Cursor, and the HTTP dashboard, each a ~10 minute cold-read
  walkthrough from install to first query.
- **`pincher project list` / `pincher project rm`** — surface the
  existing `DELETE /v1/projects` HTTP route and the `list` MCP tool
  as CLI verbs. Ambiguous `rm` substrings error with a disambiguation
  list; `--json` mode requires `--force`.
- **Coverage gate restored 83% → 84%** — pre-#92 floor recovered via
  subprocess-binary tests for the runXxxCLI dispatch wrappers. The
  path-to-85% (main() bootstrap + network-bound update paths) is
  tracked at #221 against v0.7.0.
- **Honest token-savings accounting** — `architecture` no longer
  over-claims by 5800× on metadata-only responses (#219); `symbols`
  batch now uses real `os.Stat` file sizes with file-path dedup
  instead of a 20000-byte constant (#220). Both reported by
  @nbarari with cross-corpus validation; the headline `tokens_saved`
  metric is now defensible across config-heavy and code-heavy
  workloads.

### Fixed
- **`architecture` no longer over-claims `tokens_saved` by 4-6 orders
  of magnitude** (#219, reported by @nbarari). The handler previously
  ran `savedVsFullRead(symCount, …)` which attributed `symCount ×
  avgFileSize / 4` per call — but `architecture` returns metadata
  only (counts, histograms, hotspot symbol names), so there is no
  file-read alternative an agent would have used. Cross-corpus
  validation found this single tool dominating ~97% of the
  cumulative session counter on real corpora. The handler now
  returns `tokens_saved=0` (the honest baseline); `tokens_used` (the
  response payload size) is still tracked. README's "typical per-call
  savings" line revised to drop the prior fictional `architecture
  ~99.99%` claim.
- **`symbols` batch now uses real file sizes instead of a 20000-byte
  constant** (#220, reported by @nbarari). The handler previously ran
  `savedVsFullRead(len(results), …)` which credited every result
  as a hypothetical 20k-byte file; on config-heavy corpora that
  over-claimed by 5-16× (real YAML/HCL files average 1-5k tokens), on
  Go-heavy corpora it under-claimed by ~2× (real Go files in this
  repo average ~30k+ tokens). The handler now uses
  `savedVsFileSizes(root, paths, …)` — real `os.Stat` sizes per file
  path, dedup'd by file path so an N-ID batch hitting M unique files
  attributes M file sizes, not N × per-file estimate. Mirrors what
  `search` and `trace` already do. Document-kind symbols (fetched
  URLs) are correctly excluded from the file-size baseline since
  they have no on-disk file.

### Changed
- Coverage gate restored 83% → 84% (#200). Subprocess-coverage tests
  added across `runInitCLI` / `runStatsCLI` / `runWebCLI` / `runDoctorCLI`
  / `runIndexCLI` dispatch paths brought the floor from the temporary
  v0.5.0 dip back up to 84.3% on Linux CI. The remaining gap to 85%+
  lives in `main()`'s HTTP/MCP server bootstrap and the network-bound
  update paths (`downloadAndSwap`, `runGoInstall`) — both deferred to a
  follow-up that restructures `main()` for unit testability. README
  badge bumped from 83% → 84%.

### Added
- Three end-to-end tutorials under `docs/tutorials/` (#201) —
  `claude-code.md`, `cursor.md`, `http-dashboard.md`. Each is ~10
  minutes of cold reading: install, index, wire your client, send a
  first query, watch the savings accumulate. Linked from README and
  REFERENCE.md.
- `pincher project list` / `pincher project rm` (#202) — CLI surface
  for the existing HTTP `DELETE /v1/projects` and the `list` MCP tool,
  so stdio-binary users can inspect and prune their index without a
  SQL or curl one-liner.
  - `list` (alias `ls`) prints a table or `--json`.
  - `rm` (aliases `remove`, `delete`) accepts a project id, exact name,
    or substring of name/path. Ambiguous substrings error with a
    disambiguation list rather than guessing.
  - `rm` confirms via Y/n unless `--force`. `--json` mode requires
    `--force` (no interactive prompt fits a scripted workflow).
- `pincher init --target` (#191) — multi-IDE rules-file writer. The
  init subcommand now seeds policy files for six editors and agents,
  not just Claude Code:
  - `--target=claude` — `./CLAUDE.md` or `~/.claude/CLAUDE.md` (with
    `--global`); unchanged from prior behaviour and still the default.
  - `--target=cursor` — `./.cursor/rules/pincher.mdc` with YAML
    frontmatter (`description`/`globs`/`alwaysApply`); preserves any
    user edits to the frontmatter on re-runs.
  - `--target=cursor-legacy` — `./.cursorrules` plain text, for
    pre-rules-directory Cursor.
  - `--target=windsurf` — `./.windsurfrules` plain markdown.
  - `--target=aider` — `./CONVENTIONS.md` (Aider's documented
    convention).
  - `--target=continue` — `~/.continue/config.json`, merged into the
    `systemMessage` field with line-prefixed `// pincher:start` /
    `// pincher:end` markers; preserves all unknown JSON keys.
  - `--target=detect` — write to every editor whose marker file
    (`.cursor/`, `.windsurfrules`, etc.) already exists under cwd.
  - `--target=all` — write every project-scoped target.
  All targets share the same idempotent marker-block pattern; re-runs
  replace in place rather than duplicating. Closes #191.

## [v0.5.0] — 2026-05-09 — Trustworthy single-binary release

The "you can install this anywhere and run it confidently" milestone.
Closes the install-correctness, deployment-safety, and data-integrity
gaps that blocked pre-1.0 adoption.

Highlights:

- **`go install` works** — the longstanding module-path / URL mismatch
  is fixed. `go install github.com/kwad77/pincher/cmd/pinch@latest`
  now resolves cleanly.
- **Default-deny remote HTTP** — `pincher --http :PORT` without
  `--http-key` refuses to bind a non-loopback interface (escalates the
  prior #149 warning to a hard refuse). Three escape hatches:
  `--http-key`, loopback bind, or explicit `--http-allow-open`.
- **`project_id` correctness on macOS / Windows** — duplicate project
  rows on case-insensitive filesystems are gone. Existing databases
  with the duplication get merged automatically on `Open()`.
- **Legacy FTS5 footprint removed** — the v9-introduced per-corpus
  split is now the only FTS5 path; the legacy `symbols_fts` table
  drops on first `Open()` after upgrade, reclaiming approximately half
  the FTS5 disk footprint on long-running daily DBs.
- **Release artifact pipeline live** — every `git push origin v*` now
  produces 6 platform binaries + multi-arch Docker image + Homebrew
  formula auto-bump (this kicked in for v0.4.1; v0.5.0 carries the
  workflow forward unchanged).

### Added
- `--http-allow-open` / `$PINCHER_HTTP_ALLOW_OPEN=1` (#199) — explicit
  opt-in to bind HTTP on a non-loopback interface without `--http-key`.
  For deployments where out-of-band auth is in place (reverse proxy,
  trusted Docker network, firewall-restricted environment). The #149
  open-bind warning still fires on this path so operators see the
  state in logs.
- `recomputeProjectCounts(projectID)` helper on `*db.Store` (#84) —
  refreshes denormalised counts after a dedup merge so `pincher list`
  reports post-merge reality.

### Changed
- **Repository renamed `kwad77/pincherMCP` → `kwad77/pincher`**, and
  the Go module path bumped `github.com/pincherMCP/pincher` →
  `github.com/kwad77/pincher` (#198 / #212). Closes the long-standing
  module-vs-URL mismatch that broke `go install` for the entire
  pre-v0.5 era. After this release:
  - `go install github.com/kwad77/pincher/cmd/pinch@latest` works.
  - The old GitHub URL redirects to the new one, so existing checkouts
    keep pulling/pushing without intervention; `git remote set-url
    origin https://github.com/kwad77/pincher.git` is recommended for
    clean clones going forward.
  - The Homebrew formula, plugin manifests, dashboard URL refs, and
    workflow files were updated alongside the import paths.
  - **Old import path is dead** — code that imports
    `github.com/pincherMCP/pincher/...` will fail to resolve at
    v0.5.0+.
- **HTTP server refuses non-loopback bind without auth** (#199). See
  the highlights above. Pre-bind check means the port never even
  briefly comes up for an unsafe configuration.
- **CI coverage gate temporarily lowered 84% → 83%** to land #92's
  patch (which adds 700+ lines including dedup/merge/rename and a
  schema migration; natural Linux CI coverage landed at 83.9%).
  Restoration tracked at #200 — bump to 85% will land in v0.6.0
  alongside the test-infrastructure investment needed to exercise
  SQL-error paths cleanly.

### Removed
- **Legacy `symbols_fts` virtual table dropped** (#106 / #211). The
  per-corpus FTS5 split (#32, landed at v9) has carried every search
  query for two minor-version cycles via `symbols_code_fts` /
  `symbols_config_fts` / `symbols_docs_fts`. The legacy mixed-corpus
  index has been double-populated alongside since then, paying a 4×
  write-amplification tax for callers nobody actually has — the MCP
  search handler soft-redirects `corpus=all` (the only caller-facing
  path to the legacy index) to `corpus=code` since #78. Schema v12
  migration drops the legacy table and its three sync triggers
  (`sym_fts_insert` / `sym_fts_delete` / `sym_fts_update`); the
  baseline schema no longer creates them on fresh installs.
  Long-running daily DBs reclaim approximately half the FTS5 disk
  footprint immediately on first `Open()` after upgrade.
- `corpus="all"` removed from the `corpusVtab()` routing table. The
  MCP search handler still soft-redirects `corpus=all` →
  `corpus=code` with a deprecation log line, so older callers keep
  working at the API layer; direct callers of
  `SearchSymbolsByCorpus` passing `"all"` now get an
  `unknown corpus` error.

### Fixed
- **`project_id` no longer duplicates rows on case-insensitive
  filesystems** (#84 / #92). On macOS (APFS) and Windows (NTFS
  default), opening the same project via two casings
  (`/Users/Foo/Project` and `/users/foo/project`) previously produced
  two distinct project rows pointing at the same physical directory.
  The fix canonicalises `project_id` to a deterministic form
  (symlink-resolved + casing-folded on case-insensitive FSes) and
  migrates existing duplicate-project databases by merging on
  `Open()`. The migration:
  - picks a winner per duplicate group (prefers row already at
    canonical form; otherwise highest sym_count + most recent
    indexed_at)
  - re-keys all symbols / edges / files / adrs / extraction_failures
    onto the winner; conflicts (same symbol id on both rows) drop
    the loser row, recoverable by re-indexing
  - recomputes `projects.sym_count` / `file_count` / `edge_count` on
    the survivor so `pincher list` reports post-merge reality
  - is idempotent on second `Open()`

  Thanks to @nbarari for validating the migration against a
  real-world duplicate-projects DB (5281 symbols across two casings)
  and surfacing the stale-counts and macOS test-pinning issues
  during review.

## [v0.4.1] — 2026-05-09 — Dockerfile go-version fix

Patch release. v0.4.0 was tagged with the new milestone-driven release
process but the Release workflow's Docker job failed because the
Dockerfile pinned `golang:1.24-alpine` while go.mod requires `1.25.0`.
Result: v0.4.0 didn't produce platform binaries.

This patch:

- Bumps the Dockerfile to `golang:1.25-alpine` with a comment tying
  the pin to go.mod's `go` directive.
- Adds a `workflow_dispatch` trigger to `.github/workflows/release.yml`
  so we can re-run the binary build against an existing tag (selecting
  the tag as the run's ref) without re-tagging when a transient
  infrastructure flake takes the run down.

### Fixed
- Release workflow's Docker image build no longer fails on
  `go mod download` due to toolchain-mismatch.

### Added
- `workflow_dispatch` trigger on the Release workflow.

## [v0.4.0] — 2026-05-09 — Capture-what-shipped

First release under the milestone-driven cadence (#193). Closes the
gap between v0.3.0 and the feature work that accumulated on master
since 2026-05-08. No single "theme" — this is a tag-and-release of
4 new CLI subcommands, a schema migration, expanded HCL edges, and
the per-corpus snapshot harness picking up Terraform.

Highlights:
- **Schema v11** — `sessions.http_url` / `sessions.http_pid` added so
  the HTTP dashboard process can be discovered by the MCP stdio
  process (and vice versa) for live stats.
- **Four new CLI subcommands**:
  - `pincher update` — in-repo `git pull` + rebuild OR standalone
    download from GH releases (the standalone path becomes useful
    once #197 ships release artifacts in v0.5.0).
  - `pincher web` — print the dashboard URL of a live HTTP server
    (auto-start one if none exists).
  - `pincher init` — write a marker-block-delimited pincher policy
    section into `CLAUDE.md` (or `~/.claude/CLAUDE.md` with `--global`).
  - `pincher stats` — persisted savings + per-project counts; supports
    `--json` and `--reset`.
- **HCL REFERENCES edges, complete**: var.NAME (#178) plus local /
  module / data / resource (#188).
- **Plugin SessionStart hook**: `pinchermcp` plugin install now runs
  `pincher index --hook` after install to prime the index for the
  current workspace (#138 / #187).
- **Subprocess coverage instrumentation** (#190) — `cmd/pinch`
  integration-style tests that exec the binary now contribute to the
  coverage profile. Closes the dispatcher 0% gap.
- **README split** (#184) — pitch + quickstart in README, full manual
  in `docs/REFERENCE.md`. The README is now a 5-minute read.
- **Terraform pinned corpus** (#189 / #195) — fifth corpus, exercises
  all five HCL reference-edge shapes plus nested modules.
- **Milestone-driven release process** (#196) — every PR now carries
  a milestone at create time; releases ship when their milestone hits
  100% closed.

### Added
- `testdata/corpus/terraform-stack/` — fifth pinned corpus exercising
  HCL extractor coverage (#189). Closes a gap exposed by #178/#188:
  both reference-edge PRs shipped with all gates green even though
  they materially change graph shape on real Terraform, because none
  of the pre-existing corpora contained `.tf`/`.tfvars` files. The
  new corpus pins all five reference shapes (var/local/module/data/
  resource), .tfvars Settings, multi-file resolution, nested blocks,
  and a nested module.
- New `guide` MCP tool (#139). Takes a free-form task description
  ("fix login retry bug", "refactor auth middleware", "understand
  indexing"), returns 2-3 recommended pincher tool calls with reasoning.
  Removes decision friction at session start — agents call `guide`
  first instead of choosing between search/context/trace from scratch.
  Keyword-based classifier; pure heuristic, no model.
- Schema v10: TOML routing for the config corpus (#108). The TOML
  extractor is parser-backed via `github.com/BurntSushi/toml` and emits
  `Setting` symbols mirroring the YAML/JSON shape.
- `db.GetSymbolsByIDs(projectID, ids)` — single-roundtrip batch lookup
  used by the MCP `symbols` tool. Was N round trips, now one IN-clause
  query (#129).
- `ast.RegisteredConfidence(language)` — exposes the extractor's
  registered confidence for parser identity. The `health` tool uses this
  to label parsers as `AST` vs `Regex` instead of inferring from the
  per-symbol AVG, which path penalties drag below 0.99 (#124).
- `fields=` projection on the MCP `symbol` tool — pass a comma-separated
  allow-list to project specific keys; skipping `source` also skips the
  byte-offset disk read (#124).
- `BenchmarkHandleSymbols_Batch20_GoProject` pins the batch handler cost
  for the bench-regression gate (#129).
- `pincher self-test` subcommand — end-to-end smoke check (open db,
  create synthetic project, index, search, byte-offset retrieve)
  against a temporary data dir. Exits non-zero on any failure. Use after
  install/upgrade to verify the binary works end-to-end before pointing
  it at a real project (#151).
- `pincher --help` now lists subcommands (`index`, `doctor`,
  `self-test`, `rebuild-fts`) instead of dumping flag.PrintDefaults
  alone (#152).
- `_meta.savings` — human-readable one-liner on every tool response
  ("saved ~14k tokens vs reading files…"). Trains agents and humans
  alike that pincher is cheaper than reading whole files (#144).
- `_meta.next_steps` on `search`/`architecture`/`trace`/`changes`/
  `index`/`context` — concrete next-tool suggestions tailored to the
  result shape (e.g. search Function result → `context(id=…)` and
  `trace name=…`). Removes one decision the agent would otherwise
  make from scratch every call (#146/#148/#150/#156).
- `_meta.ambiguous_match` on `trace` — when the symbol name resolves
  to multiple symbols in the project, surface the alternates so
  agents can refine instead of silently picking one (#145).
- `_meta.diagnosis` on `index` zero-symbol runs — explains why no
  symbols were extracted (only blocked files, only unsupported
  languages, all files unchanged, etc.) instead of returning an
  unannotated `symbols=0` (#147).
- `pincher doctor` rolls up extraction failures by reason once the
  per-file list crosses 5 entries — surfaces the dominant failure
  mode at a glance ("→ by reason: 12 file_too_large, 8 byte_range_negative")
  (#159).
- HTTP server logs a loud warning when started without `--http-key`
  bound to a non-loopback address — the API is open by default and
  this catches accidental exposure (#149).
- `cmd/benchcmp` gains `--ns-threshold` and `--allocs-threshold`
  flags. Defaults unchanged so local `make corpus-bench` keeps the
  tight gate; CI sets wider values to absorb runner-to-runner
  variance (#157).
- `pincher doctor` reports `binary_version` next to `schema_version`
  (#164). Surfaces in support paste-ins without a separate
  `pincher --version` invocation; suppressed when blank so a
  directly-built binary doesn't print an empty `v`.
- `_meta.diagnosis` + `_meta.next_steps` on **search** zero-result
  responses (#165). Mirrors the handleIndex empty-state pattern —
  agents no longer get a bare `count: 0`; they get a best-guess
  cause (most-specific filter first: min_confidence beats kind
  beats language beats non-default corpus) and concrete recovery
  tool calls (drop the filter, lower the threshold, try wildcard,
  always-`list` fallback).
- `_meta.diagnosis` + `_meta.next_steps` on **list** empty
  responses (#167). First-contact agents on a fresh install see
  "no projects indexed yet" with a concrete `index` next-step,
  instead of silent `count: 0`.
- New manual GH Actions workflow `.github/workflows/bench-variance.yml`
  for characterising bench variance on CI hardware (#166).
  workflow_dispatch-only — does not run on PRs / push. The
  prerequisite for re-promoting bench-regression to required: pull
  the artifact, set thresholds from observed CV, then drop
  continue-on-error.
- **Makefile extractor** (#170, closes #103). Regex-tier at confidence
  0.85. Rule targets at column 0 → Function symbols; `.PHONY:` lists
  mark targets `IsExported=true`; `=` / `:=` / `::=` / `?=` / `+=`
  variable assignments → Setting symbols. Detected by both extension
  (`.mk`, `.mak`) and filename (`Makefile`, `GNUmakefile`,
  case-insensitive `makefile`). Skips pattern rules (`%.o: %.c`),
  variable-expanded names, and recipe content.
- **SQL extractor** (#171, closes #102). Regex-tier at confidence 0.85
  across all major dialects (MySQL / Postgres / SQLite / MSSQL /
  Oracle). `CREATE TABLE` / `CREATE [MATERIALIZED] VIEW` → Class;
  `CREATE FUNCTION` / `CREATE PROCEDURE` / `CREATE TRIGGER` →
  Function. Schema prefix splits into `qualified_name` (`auth.users`)
  with bare `name` (`users`). Dialect-aware quoting (backticks,
  double-quotes, square brackets stripped). Comment-aware: `--` line
  and `/* */` block comments don't emit phantom symbols. DML / ALTER /
  DROP / CREATE INDEX deliberately out of scope. Covers `.sql`,
  `.ddl`.
- New `FilenameExtractor` interface (#170) — optional extension to
  `Extractor` for filename-based detection (`Makefile`, future
  `Dockerfile`). The registry stores both basenames and extensions;
  filename matches take precedence. Existing extractors unaffected.
- **HCL `var.NAME` reference edges** (#178, minimum-viable for #86).
  Resource / data / output / module / provider / variable blocks
  emit `REFERENCES` edges to `Variable` symbols when their attributes
  reference `var.NAME`. Nested-block refs (e.g. `provisioner` inside
  a resource) are attributed to the outermost symbol-emitting block,
  so agents reasoning about a resource see all its var dependencies
  in one place. Per-source-block dedup. `local.X` / `data.X` /
  `module.X` / cross-resource refs deferred to follow-ups.
- SQL extractor (#176): `IF NOT EXISTS` recognised on `CREATE
  FUNCTION` / `PROCEDURE` / `TRIGGER` (was already on `TABLE`/`VIEW`).
  MariaDB and SQLite dialect support.
- `SECURITY.md`, `CHANGELOG.md`, `RELEASING.md` (this PR).

### Changed
- `min_confidence` default on `search` bumped from 0.7 to 0.71 to address
  #112. Real corpora produced a confidence floor at exactly 0.70 (README
  H1 sections under the Markdown extractor: kindBaseline 0.80 averaged
  with BaseExtractor 1.00 minus PathPenalty -0.20 = 0.70 exactly), so the
  former 0.7 default was a no-op. The 0.71 threshold filters those
  bottom-floor cases (~3.6% of symbols on typical mixed corpora) without
  clipping the next tier (`.pb.go` generated code lands at 0.75).
- `corpus=all` on the MCP `search` tool is **deprecated** (#106 / #130).
  No longer in the public InputSchema enum; the handler soft-redirects
  to `code` and emits a deprecation warning. Schema-level removal of
  the legacy `symbols_fts` table is tracked at #106.
- HTTP unknown-tool error now lists tools from the live handler registry
  rather than a hand-maintained string (which had drifted past `fetch`)
  (#124).
- Pre-1.0 cleanup: removed the always-zero `BreadthPenalty` and
  `LeafPenalty` fields from `ast.Signals` (#119 / #131). The four
  populated signals (BaseExtractor + KindBaseline + PathPenalty +
  IdentBonus + GeneratedPen) carry the quality gradient on real
  corpora; the removed fields would have needed a wiring pass through
  every extractor for marginal benefit.
- `handleSymbols` batch lookup uses one IN-clause query instead of N
  per-ID `GetSymbol` calls; the byte-offset disk reads still happen
  per-symbol (they have to — byte ranges are file-local) (#129).
- README's "cross-project leakage is structurally impossible" softened
  to "structurally inaccessible from project-scoped paths" with a
  pointer to #92 for the schema-level fix that closes it at the PK
  level (#125).

### Fixed
- `handleSymbol` and `handleSymbols` now resolve the project up-front
  and use scoped DB lookups when a project is passed (#125 — closes #2,
  #7 lookup-layer defense). The composite primary-key fix is the
  schema migration in #92.
- `Trace` split into `Trace(name)` (back-compat) + `TraceByID(id)`
  (#122). `handleChanges` now uses the exact ID rather than picking
  whichever same-named symbol resolves first (#5).
- `runGitDiff` includes untracked files for `unstaged` and `all` scopes
  (#122 — closes #6). Pre-commit safety analysis can no longer miss new
  files.
- Dashboard `dashboardTemplate` no longer embeds the file's own Go
  prelude before `<!DOCTYPE html>` (#121 — closes #4). 22 inline event
  handlers migrated to `data-action*` attributes + a four-listener
  delegation block; the dashboard CSP claim (`script-src 'self'`
  without `'unsafe-inline'`) is now actually enforceable.
- Indexer per-file size cap (#116 — closes #111). 4 MB default,
  configurable via `--max-file-size-mb` or `PINCHER_MAX_FILE_SIZE_MB`.
- Search corpus fall-through (#118 — closes #113). When the user
  doesn't pass an explicit corpus and the default `code` returns zero
  results, the handler retries `config` then `docs`, surfacing the
  fallthrough chain in `_meta.fellthrough_to`. Fixes the 0-hit problem
  on Terraform/Ansible/docs-only projects.
- QN disambiguation across all regex-based extractors (#120 — closes
  #115). When the same qualified name appears twice in a file, the
  disambiguator suffixes with `~<startLine>` so all symbols survive;
  pre-fix, the second symbol clobbered the first via primary key.

### Test coverage / CI
- `internal/db` coverage 81.0% → 83.8% (#126).
- `internal/index` coverage 81.4% → 84.1% (#127).
- CI coverage gate ratcheted 83% → 84% (#128).
- TOML integration tests + `extraction_failures_by_reason` corpus
  snapshot gate (#108).
- HTTP `unknown-tool` test asserts `fetch` appears in the available list
  (caught the pre-existing drift) (#124).
- Bench-regression CI gate **calibrated and stabilised**
  (still advisory; ready for re-promotion pending green-run
  accumulation). Final shape:
  - Re-baselined on CI hardware (#158) so deltas reflect
    runner variance, not dev-vs-CI hardware mismatch.
  - Thresholds 0.30 ns / 0.45 allocs against CI baselines (#157).
  - `--exclude` flag (#174) skips two benchmark families that
    don't fit a percentage-based gate: Index_Incremental_NoChange_GoProject
    (21.5% within-run CV per #173, I/O-bound) and
    Auth_TimingProfile/* (sub-100µs absolute ns shifts 2x across
    CI runner-pool reallocations regardless of <1% within-run CV).
    Excluded benches still appear in CI output with `[EXCLUDED]`
    marker so a real regression remains visible.
  - CI variance harness landed (#166) and run committed to
    `testdata/bench/variance-ci-2026-05-09.md` (#173). 20 of 21
    benchmarks at <10% CV on CI; the 1 outlier is in the
    exclude list.
  Failed-promotion path documented inline: short-lived
  required-gate promotion (#160) reverted in #162 after a single
  outlier (Cold_NodeMonorepo +109% / Incremental_K8sOps +276%);
  three green runs weren't a sufficient sample. Re-promotion now
  awaits accumulation, not characterisation.
- Bench warmup pass on the noisy server-package benchmarks dropped
  per-bench coefficient of variation from 36% → ~3% (#141).

## [v0.3.0] — 2026-05-08 — Trust + observability

Per-symbol confidence scoring (#34, all 4 phases). `pincher doctor`
diagnostic surface (#42). Per-corpus FTS5 split (#32) with code/config/
docs routing and zero-result fall-through. Reader pool (#51).
Pinned-corpus benchmarks (#50) and snapshots (#33). Six-item security
audit (#41).

New extractors: HCL/Terraform (#67), Markdown (goldmark), Bash (shfmt),
Jinja2 (gonja), YAML/JSON Settings, C macro/forward-decl/#ifdef polish.

Recent CRITICAL fixes:
- #111 — indexer per-file size cap (was: hang on large JSON)
- #113 — search fall-through (was: 0 hits on Terraform/Ansible)
- #115 — QN disambiguation (was: silent symbol loss on regex langs)

Behaviour changes (semver minor signals):
- `min_confidence` default 0.0 → 0.7 on `search`/`query`
- `corpus` default routes to `code` (mixed needs `corpus=all`)
- Symbol QNs may contain `~<line>` for same-file duplicates

Schema v9. `extraction_failures` gains `file_too_large` reason.

149 commits since v0.2.1.

## [v0.2.1] — Downgrade-safety fix

`migrate()` refuses to open a database at a schema version newer than
this binary understands, instead of silently proceeding and corrupting
newer columns. Upgrade path is unchanged; only the previously-undefined
downgrade case is now handled explicitly.

Load-bearing for the Claude plugin, which pins its own pincher version
and downloads it into the plugin's `bin/`. Users may end up with
multiple pincher binaries on one machine (plugin + Homebrew + stray
binary download); this fix makes sure they all coexist safely around
the shared `pincher.db`.

## [v0.2.0] — First binaries + Docker

First release with prebuilt binaries and Docker image.

Highlights:
- Release workflow: linux/darwin/windows × amd64/arm64 binaries +
  multi-arch `ghcr.io/kwad77/pinchermcp` image, SHA256SUMS,
  auto-generated release notes.
- IMPORTS edges for Go — cross-file dependency queries via Module
  symbols keyed by go.mod's within-module paths.
- `--http :0` auto-pick + `PINCHER_HTTP_ADDR` / `PINCHER_HTTP_KEY` env
  fallback so Docker/systemd/launchd configuration works without
  rewriting argv.
- Dashboard: ghost-project bulk cleanup, name/path filter, parallel
  initial loads, copy-symbol-ID button, first-run onboarding,
  bearer-token auth flow with auto-prompt on 401, sessions tab total
  row, ADR tab default project.
- `handleStats` restored ALL-TIME and PROJECT sections.
- `packaging/`: Homebrew formula, systemd user unit, launchd
  LaunchAgent, Windows `sc.exe` install script — all driven by the
  same env-var contract.
- `docs/index.html`: single-file GitHub Pages landing page.
- CI coverage gate lowered to 83% to match reality.

[Unreleased]: https://github.com/kwad77/pincher/compare/v0.11.0...HEAD
[v0.11.0]: https://github.com/kwad77/pincher/compare/v0.10.0...v0.11.0
[v0.10.0]: https://github.com/kwad77/pincher/compare/v0.9.0...v0.10.0
[v0.9.0]: https://github.com/kwad77/pincher/compare/v0.8.0...v0.9.0
[v0.8.0]: https://github.com/kwad77/pincher/compare/v0.7.0...v0.8.0
[v0.7.0]: https://github.com/kwad77/pincher/compare/v0.6.0...v0.7.0
[v0.6.0]: https://github.com/kwad77/pincher/compare/v0.5.0...v0.6.0
[v0.5.0]: https://github.com/kwad77/pincher/compare/v0.4.1...v0.5.0
[v0.4.1]: https://github.com/kwad77/pincher/compare/v0.4.0...v0.4.1
[v0.4.0]: https://github.com/kwad77/pincher/compare/v0.3.0...v0.4.0
[v0.3.0]: https://github.com/kwad77/pincher/compare/v0.2.1...v0.3.0
[v0.2.1]: https://github.com/kwad77/pincher/compare/v0.2.0...v0.2.1
[v0.2.0]: https://github.com/kwad77/pincher/releases/tag/v0.2.0
