# Changelog

All notable changes to pincherMCP. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/) — once 1.0 ships, schema
breaking changes will be major bumps and tool-contract additions will be
minors.

## [Unreleased]

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

[Unreleased]: https://github.com/kwad77/pincherMCP/compare/v0.3.0...HEAD
[v0.3.0]: https://github.com/kwad77/pincherMCP/compare/v0.2.1...v0.3.0
[v0.2.1]: https://github.com/kwad77/pincherMCP/compare/v0.2.0...v0.2.1
[v0.2.0]: https://github.com/kwad77/pincherMCP/releases/tag/v0.2.0
