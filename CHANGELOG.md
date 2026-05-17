# Changelog

All notable changes to pincherMCP. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/) — once 1.0 ships, schema
breaking changes will be major bumps and tool-contract additions will be
minors.

## [Unreleased]

## [0.71.0] — 2026-05-17 — Ansible graph completion + multi-branch foundation + structured _meta diagnostics

v0.71 closes long-standing gaps across three axes. (1) **Ansible graph completion (#71 Phase 1 + Phase 2 in one cycle):** `INCLUDES` / `LOADS` structural edges for playbook→role and host→host_vars plus `USES_VAR` dataflow edges from `{{ var_name }}` Jinja substitutions to their canonical `Setting` declarations in `group_vars/` / `host_vars/` / `vars/` / `roles/*/defaults/main.yml` / `roles/*/vars/main.yml`. Ansible-shaped audits ("what uses var X?", "is var Y dead config?") are now answerable. (2) **Multi-branch coexistence foundation (#1303 Phase 1 + 2a):** schema v31 adds a `branch` column to `symbols` / `edges` / `files` / `pending_edges`; v32 adds `projects.current_branch`; the indexer detects + stamps the current branch on every row; doctor surfaces a branch-drift advisory when the on-disk branch diverges from the last-indexed branch — catches the silent "I switched branches and forgot to re-index → wrong byte-offsets" footgun. Phase 2b PK widening + Phase 3 query-side filter ship in v0.72. (3) **Structured _meta diagnostics (#1098):** `_meta.warnings_v2` and `_meta.diagnosis_v2` land side-by-side with the existing string fields for one release cycle (option (a) back-compat path) — entries carry `code`, `severity`, `message`, and typed `data` so clients can act on diagnostics programmatically instead of regex-matching prose. **Cross-cutting:** `pincher hook-stats --export-7d` ships the field-data sharing CLI (#662, downstream of #640); `pincher init --target=jetbrains` lands JetBrains AI Assistant rules at `.idea/.junie/guidelines.md` (#1335); typed-SDK codegen scaffolding under `sdks/` with openapi-generator config for TS/Python/Go (#1262 first slice — registry publishing deferred); IMPORTS edges for Bash `source` (#1341), HCL Terraform `module` blocks (#1342), Markdown inter-doc links (#1343), Makefile rule-to-rule deps (#1344) — five regex-tier extractors join the IMPORTS graph; `@external/` synthetic Module symbols (#1340) so previously-dropped non-Go IMPORTS edges persist; MCP `StartSchemaDriftWatcher` (#1374) exits the process when DB schema_version exceeds the binary's compiled-in version so supervised mode respawns onto a fresh instance; migration guide v0.4 → v1.0 first draft at `docs/migration/v0.4-to-v1.0.md` (#1332, v0.73 deliverable shipped early). **Dogfood-found fixes:** TSX `qualified_name_collision` flood from Next.js App Router sibling handlers each declaring `const res = await fetch(...)` collapsed under `<module>.res` (6060+ failures in workfixd corpus) — TS/TSX/JS regex extractor now scopes locals to enclosing function (#1375); cold-path index resolver had four other small fixes that shipped throughout the cycle.

### Added
- `_meta.warnings_v2` and `_meta.diagnosis_v2` structured-shape fields land side-by-side with the existing string `_meta.warnings: []string` and `diagnosis: string` fields for one release cycle (option (a) back-compat path). Entries carry `code` (machine-actionable, stable across releases), `severity` (`info` / `warning` / `error`, aligned with MCP 2025-11-25 `notifications/message`), `message` (human-readable prose), and optional `data` (typed payload — e.g. for `unknown_arg`, the typo'd key + accepted-keys list, so clients can auto-suggest corrections instead of regex-matching prose). `unknownArgs` (#499) is the first emit site upgraded; subsequent PRs migrate the remaining sites. Closes #1098.
- Ansible playbooks now emit `INCLUDES` edges for `roles:`, `import_role:`, and `include_role:` references, and YAML inventories emit `LOADS` edges from host entries to their `host_vars/<host>.yml` files. Completes the unfinished #71 Phase 1 scope (the original four-edge table shipped only the `RENDERS` slice). YAML-format inventories under `inventory/`, `inventories/`, or named `inventory.yml` / `hosts.yml` are detected; INI-format inventories remain out of scope. `group_vars/<G>.yml` for groups also deferred — group resolution requires walking the `children:` tree structurally and the value-add over host-only edges is marginal for the audit queries the edges feed. Bare hosts referenced under multiple groups dedupe to a single LOADS edge. Closes #1160.
- Ansible `USES_VAR` dataflow edges from Jinja `{{ var_name }}` substitutions to their declaration sites. Jinja `.j2` templates and Ansible task/playbook YAML string values are scanned for the leftmost identifier of each substitution; the resolver binds the reference to the canonical `Setting` symbol in `group_vars/`, `host_vars/`, `vars/`, `roles/*/defaults/main.yml`, or `roles/*/vars/main.yml`. Multiple var-files declaring the same name resolve to the lexicographically-smallest symbol ID (mirrors the #428 IMPORTS canonical-pick); the full 22-level Ansible host-context precedence remains out of scope. Jinja-reserved tokens (`loop`, `super`, `caller`, `true`, ...) skip extraction so loop-local references don't dangle. Templates that contain only `{{ var }}` outputs and no `{% macro %}` / `{% block %}` / `{% set %}` now get a synthetic per-file `Module` symbol so the resolver has a stable from-side anchor — previously the per-file goroutine short-circuited on `len(Symbols) == 0` and dropped the edges before persistence. Completes the unfinished #71 Phase 2 scope (predecessor closed having shipped only Phase 1 structural edges). Closes #1165.
- Typed-SDK codegen scaffolding lands under `sdks/` — openapi-generator config files for TypeScript / Python / Go plus a `scripts/generate-sdks.sh` wrapper that fetches `/v1/openapi.json` from a running pincher and feeds it to whichever generator is on PATH (openapi-generator-cli, openapi-generator, or `docker openapitools/openapi-generator-cli`). Generated SDK trees are gitignored — these are thin clients regenerated on demand whenever the spec changes. Each language has an `examples/search.ts` / `search.py` / `search.go` calling the SDK's SearchApi. The OpenAPI 3.1 spec-validity gate (`internal/server/openapi_spec_validity_test.go`) ensures the spec stays codegen-clean on every PR. **Registry publishing (npm / PyPI / pkg.go.dev auto-publish on release tag) intentionally deferred** — needs registry credentials wired into the release workflow that aren't yet available; tracked under #1262 follow-up. Locally-generated SDKs work today; users who want to ship under their own namespace can vendor the generated output. Closes #1262 first slice.
- Indexer detects the git branch at index time and stamps it on `projects.current_branch` plus every Symbol/Edge it writes (schema v32, #1303 Phase 2a). Doctor (CLI and MCP) gains a branch-drift advisory that fires when the on-disk branch differs from the last-indexed branch — catches the silent "I switched branches and forgot to re-index → wrong byte-offsets" footgun. Detection wraps `git rev-parse --abbrev-ref HEAD` with a 2s timeout, falls back to a short commit SHA on detached HEAD, and caches per-project for 30s to keep the watcher's no-change-tick allocation budget intact. `--force` index calls bypass the cache so post-checkout re-index picks up the new branch immediately. Phase 2b (UNIQUE/PK widening for true multi-branch coexistence) intentionally deferred — that needs the v28-style table rebuild plus coordinated query-layer changes and ships in its own PR. Branch-drift advisory mirrored in both `internal/server/admin.go` and `cmd/pinch/doctor.go` per the CLAUDE.md bounded-duplication convention.
- Schema v31 adds a `branch` column to `symbols`, `edges`, `files`, and `pending_edges` — foundation for #1303 multi-branch coexistence. Phase 1 only: the column exists and round-trips through `BulkUpsertSymbols`/`BulkUpsertEdges`, but the indexer doesn't stamp it yet (Phase 2 wires `git rev-parse --abbrev-ref HEAD` at index time and widens the UNIQUE/PK constraints). Pre-existing rows default to '' meaning "indexed before branch-awareness landed; treat as current branch for queries." Phase 3 will add the query-side default-to-current-branch filter and Phase 2 + 3 land in subsequent PRs to keep the table-rebuild risk reviewable in isolation. Capability tag bumped `schema_v30` → `schema_v31`; 7 pinned corpus snapshots regenerated.
- Migration guide first draft lands at `docs/migration/v0.4-to-v1.0.md` (#1332, v0.73 deliverable, Phase 3 / #668). Single document covering: a 30-second walkthrough by starting version; the schema-version map (v1 → v32 with the user-impact column on each migration); tool-contract changes (additive list of new tools since v0.4 + the v0.66 silent-cross-project guard semantic change); the `_meta` envelope evolution; CLI flag additions; the `PINCHER_*` env-var matrix; filesystem layout per OS; and a common-path walkthrough ("I'm on v0.6.0, what do I do?"). Cross-linked from README's v1.0 roadmap row. Acceptance per #1332 needs ≥1 external user review before final — the doc explicitly marks itself "first draft" pending that review.
- `pincher init --target=jetbrains` writes `./.idea/.junie/guidelines.md` — the project-rules path JetBrains AI Assistant (the Junie companion shipped in IntelliJ IDEA, PyCharm, GoLand, WebStorm, RubyMine, and the rest of the JetBrains IDE family) inlines into its system prompt when present (#1335 v0.76 parity wave 2 — first slice). Detection fires on `.idea/` (the universal JetBrains project marker — every JetBrains IDE creates it on first open), so `pincher init --target=detect` now surfaces JetBrains alongside the other detected hosts. `--global` is rejected with a loud error explaining that JetBrains AI Assistant's global rules live in the IDE's Preferences > Tools > AI Assistant UI rather than a stable filesystem path; matches the cursor / windsurf / aider pattern. Writer is MergePolicyBlockBare — same shape as zed / gemini — so re-runs replace the marker block in place (idempotent). README quickstart + `TargetNames()` + `TestInitTargets_RegistryShape` snapshot all updated. The other #1335 targets (windsurf / aider / continue / vscode-copilot=vscode) already exist; wave-2 reduced to JetBrains once existing-target verification confirmed paths are still current.
- IMPORTS edges whose `to_name` doesn't resolve to an in-project symbol no longer silently drop at `resolveImports` — pincher synthesizes a lightweight `Module` symbol at sentinel file_path `@external/<sanitized-qn>` so the edge always binds. Option (a) of #1340. The tail-pass GC skips `@external/` paths so the synthetics aren't reaped between resolve passes. Cascade: #1341 (Bash) / #1342 (HCL) / #1343 (Markdown) IMPORTS edges now persist (previously emitted by extractor, dropped by resolver). Python/JS/Jinja2 external imports also persist now. The clutter tradeoff (synthetic Module symbols show up in `search` by default) is the explicit option (a) cost — a future filter PR will add the opt-out. Pinned corpus snapshots (go-project / python-web / terraform-stack) regenerated to account for the new symbols + edges. Closes #1340.
- Bash extractor now emits CALLS and IMPORTS edges in addition to Function symbols. CALLS fires when a CallExpr's first word matches an in-file Function name (cross-file commands intentionally drop until the resolver supports Bash, mirroring regex-tier language policy). IMPORTS fires on POSIX `source other.sh` and `. other.sh` includes. Top-level invocations attach to the file scope (FromQN=""), function-scoped invocations attach to the enclosing function. Closes #1341.
- HCL extractor now emits IMPORTS edges for Terraform `module "x" { source = "..." }` declarations. The edge goes from the module block's QN (`module.NAME`) to the literal source string (local path, registry shorthand, or git URL). Interpolated sources (`source = "${var.path}"`) and non-module blocks with a `source` attribute (e.g. `required_providers`) are deliberately ignored. Cross-file resolution of external sources (registry / git URLs) drops at the resolver until #1340 lands non-Go IMPORTS persistence — the edge is extracted in either case. Closes #1342.
- Markdown extractor now emits REFERENCES edges for inter-doc links (`[text](other.md)`, `[text](other.md#section)`) and intra-doc anchors (`[text](#section)`). External URLs (http/https/mailto/tel/data/javascript/protocol-relative) and non-docs extensions (.png, .pdf, etc.) are deliberately skipped — they don't resolve to docs symbols. Self-edges (link to the same section that contains it) are filtered. The cross-file resolver does not yet bind these targets to existing Section symbols (same fate as #1340's Python/JS/Jinja2 IMPORTS); the edges are extracted in either case. Closes #1343.
- Makefile extractor now emits CALLS edges for rule-to-rule dependencies — `build: deps fmt` produces CALLS from `build` to each in-file rule named in the prereq list. Variable references (`$(BUILD_DEPS)`), pattern stems (`%.o`), and prerequisites that don't resolve to in-file rules are skipped intentionally. Closes #1344. Also tightens `makeRuleRE`: the pre-existing `s*` clauses around the colon were newline-tolerant, causing recipe lines to fold into the prior rule's prerequisite list — invisible while only Function symbols were emitted, exposed as spurious self-edges once CALLS landed.
- MCP server gains a `StartSchemaDriftWatcher` background goroutine (60s poll) that compares the DB's stored `schema_version` against the running binary's compiled-in `db.CurrentSchemaVersion()`. When the DB has been migrated past what the binary understands — typically because a sibling pincher process or a `pincher index` CLI built from a newer source tree ran a migration the running MCP isn't aware of — the watcher logs `pincher.schema_drift.detected` at ERROR severity and exits the process with code 1 so supervised mode brings up a fresh instance. The next process either understands the new schema (auto-restart-on-drift binary swap) or fails informatively at startup via the existing `db.migrate()` "newer than this binary understands — upgrade pincher" guard. Pre-fix the running binary served requests against shape it didn't fully understand indefinitely, with `_meta.capabilities` advertising the binary's compile-time `schema_v$(N)` tag while the DB sat at `vN+M` — capabilities lying, write paths running blind. Found during #1370 dogfood: doctor showed `schema_version: 32` alongside `capabilities: [schema_v30]` on a long-running MCP that watched a sibling test run migrate the DB past it. Closes #1374.
- New `pincher hook-stats --export-7d` subcommand emits a shareable JSON snapshot of the trailing 7-day PreToolUse hook conversion-rate metrics — the same data the `/v1/hook-stats` dashboard panel reads. Anonymized by default (no project paths, no file paths, no hostnames); `--include-host` is an opt-in flag to attach pincher version + OS/arch for #640 outlier triage. Telemetry stays local per #626 — this is a shim that helps users contribute their numbers to the field-data thread, not a phone-home channel. CLI-only by deliberate choice; no MCP surface. Closes #662.

### Fixed
- Extended #1208's `qualified_name_collision` suppression carve-out to TSX — the `.tsx` extension maps to language tag "TSX" (distinct from "TypeScript"), so TSX files were falling through and emitting per-file failure rows on object-property keys (`data`, `res`, `api`, `graph` repeated across handler bodies; 15+ collisions per file observed in Codex-corpus dogfood). Same UX rationale as the original TypeScript carve-out — disambiguation by line still keeps every symbol individually addressable; the suppression keeps the diagnostic surface focused on real regex-scope blindness in code corpora.
- Project-lock contention errors now record the holder's `binary_version` and surface a version-skew hint when it differs from the caller's — pre-fix the error just named a PID, leaving the operator unable to tell whether the holder was a legitimate concurrent indexer of the same binary or an orphan watcher from a prior `make install` that should be killed. User repro: fresh v0.68 MCP child blocked four minutes by three v0.58 orphan watchers sequentially racing the lock; with the skew hint each "kill PID" loop step is now self-documenting. Legacy lockfiles written by pre-#1312 binaries gracefully degrade to the prior message shape. #1312
- Incremental index now correctly skips files whose extraction yields zero symbols (empty Markdown, header-less prose, YAML/JSON with no extractable settings) — previously these were re-walked + re-extracted on every Watch tick forever because the `files` hash row was only written from inside `flushBuffers`'s symbol-iteration loop. Reported on pincherMCP-on-Mac as 16 ghost files re-extracted every pass. #1313
- `extraction_failures` rows are now garbage-collected when the underlying reason no longer fires on re-extraction — `recordExtractionHeuristics` collects the reasons it emits this pass and the new `Store.PruneExtractionFailuresForFile` deletes any other rows for the same `(project_id, file_path)`. Pre-fix a fix-the-bug PR left historical evidence in the table forever; user repro: `README.md` `qualified_name_collision` row 8 days old after #1207's Markdown suppression. Rows for files skipped by content-hash (no re-extraction this pass) are retained intentionally — the prune is gated on actual re-extraction, not on the absence of a row in the current pass. #1319
- `parentWatchLoop` now layers a kernel-reparent signal on top of the existing PID-liveness probe — Unix stdio MCP children compare the current `os.Getppid()` against the captured ppid every poll cycle, and fire the shutdown path the moment it changes (typically to 1 / init / launchd after the real parent dies). Pre-fix `pidIsAlive(originalPpid)` returned true whenever the kernel had recycled the original PID to some unrelated process, leaving long-lived orphans (10h / 14h / 25h+ observed in dogfood) to race-stomp the project lock. Windows behaviour unchanged — `Getppid()` doesn't reparent there; the PID-liveness probe remains the sole signal. #1321
- C extractor no longer emits `qualified_name_collision` on kernel-style files that pair `static int foo(...) { ... }` with `EXPORT_SYMBOL(foo);` further down — `extractCBareMacros` now dedupes by Function name in addition to start-byte, so the macro form is suppressed when the real definition is the symbol of record. Bare-macro exports without an in-file definition still emit normally. Closes the kernel-idiom residual of #1067 that #1148's reserved-keyword filter didn't cover. #1324
- `scripts/swap-active-binary.sh` no longer prints "no pincher or pincher found in PATH" on Unix when both lookups fail — the empty `EXE_SUFFIX` produced a confusing message that read like a build-time substitution bug. Now emits "no pincher found in PATH" when `EXE_SUFFIX` is empty. #1325
- JavaScript symbols extracted via the default-on AST path (#266, v0.20.0) now stamp `extraction_confidence=1.0` instead of the regex-tier 0.85; `pincher health` reports `JavaScript: parser="AST"` when the AST path is enabled (mirroring Python's #944 carve-out); CLAUDE.md's extractor catalogue moves JavaScript/JSX from regex-tier to parser-backed AST. Closes the three-way drift between runtime, registered confidence, health-report label, and contributor docs that accumulated for ~12 releases after the default-on switch. #1328
- TS/TSX/JS regex extractor scopes `const`/`let`/`var` declarations to their enclosing top-level function instead of always emitting them at module level. Pre-fix every Variable got QN = `<module>.<name>` regardless of whether it lived inside `GET()` or `POST()` or `generateMetadata()`, so the canonical Next.js App Router page.tsx pattern — sibling handlers each declaring `const res = await fetch(...)` and `const json = await res.json()` — collided on `<module>.res` × N. The `qualified_name_collision` guard dropped all but one, silently disappearing real symbols from the index. Workfixd corpus surfaced 6060+ such failures in dogfooding pincher doctor. Post-fix the QN includes the enclosing function (`app.admin.page.GET.res` vs `app.admin.page.POST.res`); `Parent` is stamped to the function's QN so consumers can drill from the function to its locals. True module-level constants (`export const config = {...}`) keep their module-scoped QN; the tracker resets past each function's end-line so a top-level Variable after a function isn't spuriously scoped to it. Tests pin: positive Next.js shape, parent stamping, module-level unchanged, after-function-end reset. Closes #1375.
## [0.69.0] — 2026-05-17 — Hardening: watcher hot-path -85% allocs + every server handler below v0.60 baseline + Windows install path + dogfood-found HTML correctness

v0.69 is the Phase 2 hardening release. (1) **Perf regression validation against the v0.60 baseline (#670 §2)** drained the largest open gaps: the watcher hot-path (`BenchmarkIndex_Incremental_NoChange_GoProject`) had silently regressed 331 → 4806 allocs/op since v0.60 because the cross-file resolve block + PythonSourceRoots WalkDir ran unconditionally even on no-change ticks; gated on `force || totalFiles > 0`, the path is back to 716 allocs/op (-85%, 5.5× faster) — pincher's watcher poll is now cheap. The companion regression on the server side — `BenchmarkAuth_TimingProfile/correct` at 150 → 1062 allocs/op — was 60% `ApproxTokens` running the cl100k_base BPE encoder on every API call; switching to a char/4 heuristic by default (opt back in via `PINCHER_TOKEN_ACCOUNTING=exact`) closes every handler bench (Symbol / Search / Query / Architecture / Symbols batch — all now below v0.60 baseline). Cold-path resolve gets its own threshold-gated pre-load (#1338) for large projects. (2) **Distribution polish (#1260)** completed three slices: Scoop manifest for Windows installs via raw GitHub URL, Homebrew dispatch from `pincher update` (Mac users no longer get useless `go install` instructions), and ADR-0001 recording the deferred-to-v1.0 code-signing decision with the v0.x bypass documented; `pincher doctor --fix` shipped earlier in the cycle for the safe-action allowlist (VACUUM-when-bloated). (3) **Coverage targets met (#1164)** — internal/index 84.2 → 86.1% via test-only progress-helper coverage; OTLP tracer + emitEvent pushed earlier in the cycle. (4) **Dogfood-found HTML bug** — `pincher doctor` against pincher's own `docs/index.html` surfaced a `byte_range_negative` Section symbol from `bytesFindHeadingAfter`'s no-match-vs-found ambiguity; fixed via -1 sentinel skip. (5) **Phase 3 backlog (#670 §6)** — every v0.71-v0.78 milestone now has ≥1 issue queued. (6) **Cross-cutting infrastructure** — OpenAPI 3.1.0 spec-validity test gate ahead of typed-SDK generation (#1262), branch-aware git hook no-op signals (#1303 §2a), capability + tool-description opt-outs for heavy-traffic aggregators (#1087/#1088), `adr action=export` + four Mermaid architecture diagrams at docs/architecture/ (#1331 §1+§2) bridging runtime ADRs to the cross-release pattern, smart-skip git hooks (#1303 §2a). Schema stays at v30 (no migrations this cycle). Deferred to v0.70 stable promotion: cross-platform smoke validation (#670 §3). Deferred to v0.71+: #1338 cold-resolve representative-corpus retuning, #1260 §4 uninstall (destructive scope), #1162 closure-tables-default-on (gated on 10× p50 bar that current corpora don't meet).

### Added
- `PINCHER_META_CAPABILITIES=off` (or `false`/`0`/`none`/`no`) env opt-out at server start: drops the per-call `_meta.capabilities` stamp (#649) for heavy-traffic aggregators that want to skip the ~50 tokens/call overhead. Default behavior unchanged — every consumer reading `_meta.capabilities` today keeps working. Companion `GET /v1/capabilities` endpoint returns the same slice in a single fetch for HTTP clients that opted out and need to query once. Unknown env values default to "on" (failure-as-pedagogy: a typo'd opt-out keeps current behavior so the operator notices when their measurement shows no cost change). (#1087)
- `PINCHER_TOOL_DESCRIPTIONS=short` env opt-in at server start: swaps the 5 longest tool descriptions (`trace` / `search` / `neighborhood` / `query` / `changes`) for one-sentence variants, trimming ~3 KB / ~750 tokens off every session-start `tools/list` handshake. Default-off preserves the dense pedagogical descriptions every consumer reading `tools/list` today gets. Unknown env values default to long-form (failure-as-pedagogy: typo'd opt-in keeps current behavior so the operator notices when their payload measurement shows no change). Long-form content stays available via `docs/REFERENCE.md` per-tool sections — agents running with short descriptions can still fetch the full guidance via `pincher_fetch` or by reading REFERENCE directly. (#1088)
- internal/index `emitEvent` coverage 50% → 100% (#1164 deliverable). Two new tests pin the SetEventHook + index_started/index_complete event flow that was only being exercised by MCP integration tests — the bare CLI path (nil onEvent callback) and the wired-callback path (MCP server pattern) both now have unit coverage in the index package itself. Small slice toward #1164's `internal/index ≥85%` target (84.1% → 84.2%).
- `internal/index` coverage 84.2% → 86.1%, crossing the #1164 §1 ≥85% target. Three test-only progress helpers (`MarkActiveForTest`, `UnmarkActiveForTest`, `GetProgressDetail`) had real consumers in `internal/server` but no in-package tests, so per-package coverage reported 0% on all three despite the server-side suite using them. Tests pin the round-trip contract (Mark stamps active=true and a populated progress entry; Unmark reverts both — progress-entry leak on Unmark would silently break server-side test isolation) plus both branches of `GetProgressDetail` (empty + populated with non-zero `StartedAtUnix`, mirroring the `/v1/index-progress` ETA path from #535). Three uncovered exported APIs at session start; all now in the regression net.
- `pincher doctor --fix` auto-resolves the safe subset of advisories (#1260 §3). Tight allowlist for v0.69 hardening: VACUUM the DB when >50 MB of reclaimable space exists (gates the cost on clean installs). Each action reports `applied` / `noop` (criterion not met) / `skipped` (precondition like an open WAL reader blocks the fix) / `error` (fix attempted and failed). Destructive remediations — project deletion, force-reindex, prune-stale — stay explicit-action and require the targeted subcommand so their cost/destructiveness isn't silently absorbed into a generic `--fix`. `--json` for CI integration mirrors the existing diagnose-side shape.
- Scoop manifest published in-repo at `packaging/scoop/pincher.json` (#1260 §1). Windows users can install via the raw GitHub URL — `scoop install https://raw.githubusercontent.com/kwad77/pincher/master/packaging/scoop/pincher.json` — without waiting for a dedicated `scoop-pincher` bucket repo. Manifest pins to the v0.60.0 stable release (matching the in-repo Homebrew formula's pin discipline) with autoupdate hooks targeting future stable releases via `$version` URL templates + SHA256SUMS regex lookup. Covers both 64bit and arm64 Windows builds; bin alias exposes `pincher` regardless of host arch. `.pincher` user data dir persists across `scoop update pincher`. Contract pinned by `scripts/scoop-manifest_test.sh` (gated on jq availability; SKIPs cleanly on Windows test runners where jq isn't shipped): validates JSON, required fields, sha256 hex format, per-arch bin aliases, and autoupdate version-template consistency. Documented in `packaging/README.md`'s layout table + per-platform quick start. Auto-bump-on-release wiring tracked as a separate slice — current manifest stays static until the next stable promotion, matching today's Homebrew cadence.
- Binary signing decision recorded as ADR-0001 (#1260 §2): defer Apple Developer ID + Windows Authenticode purchase until v1.0+ promotion; document the curl-based bypass for v0.x. Pre-1.0 with ~2 users, $300-500/yr recurring certificate cost + enrollment friction outweighs the user-acquisition value of removing macOS Gatekeeper / Windows SmartScreen friction. Most install paths (Homebrew, Scoop #1260 §1, Docker) bypass the gates naturally; direct-download users hit the single-click "Allow" / "Run anyway" flow. Re-evaluation triggers documented in the ADR: v1.0 release-prep cycle, user-reported install-friction issues exceeding 5 in a release window, or an OSS-targeted free signing program becoming available. `packaging/README.md` gains the macOS `xattr -d com.apple.quarantine` one-liner + Windows SmartScreen click-through guidance. Codifies the first cross-release architectural decision in `docs/adr/` — the project's first ADR.
- `pincher update` detects Homebrew installs and dispatches to `brew upgrade pincher` instead of `go install` (#1260 §5). Pre-fix: a Mac user who ran `brew install pincher` and then `pincher update` got a fallback message telling them to run `go install github.com/kwad77/pincher/cmd/pinch@latest` — useless without a Go toolchain on their machine. Now: when the running binary's resolved path lives under `/opt/homebrew/`, `/usr/local/`, or `/home/linuxbrew/.linuxbrew/`, `pincher update` prints the brew command (`brew update && brew upgrade pincher`) and exits without invoking it by default. Pass `--yes` to invoke brew directly; advisory-by-default because brew is the kind of tool whose output the user generally wants to see live (and re-running it from inside an unrelated process confuses users who then re-run brew themselves). `--check` and `--dry-run` flags carry over and respect the brew path the same way. Pinned by `TestDetectInstallMethod_HomebrewPrefixes` (path classification across Apple-silicon Cellar, Intel-Mac, Linuxbrew, and non-brew binaries), `TestUpgradeViaHomebrew_AdvisoryByDefault` (no auto-invoke without --yes), `TestUpgradeViaHomebrew_DryRunDoesNotInvoke` (--dry-run preempts --yes), `TestUpgradeViaHomebrew_YesInvokesBoth` (argv ordering: update before upgrade), and `TestUpgradeViaHomebrew_PropagatesBrewError` (brew failures surface with context). Documented in `docs/REFERENCE.md` `pincher update` section.
- OpenAPI 3.1.0 spec-validity test gate (#1262 prerequisite). Three new pure-Go tests pin the structural invariants downstream code generators rely on: top-level `openapi` + `info` + `paths` fields, every path item has ≥1 HTTP method with a `responses` block, every `$ref` resolves to an actual `components/` entry. Catches the failure modes that would break SDK generation (openapi-generator / swagger-codegen) earlier in the loop than the full release-workflow codegen step. Hardening addition ahead of the typed-SDK release pipeline shipping the actual npm / PyPI / pkg.go.dev publishes.
- `adr` MCP tool gains `action=export` — renders the project's runtime ADR map as a single Markdown document for piping into `docs/adr/` (#1331 §1 v0.72 first slice). Bridges the runtime ADR storage (SQLite `adrs` table — per-session decisions captured via `adr action=set`) with the cross-release ADR pattern established by ADR-0001 at `docs/adr/0001-binary-signing-decision.md` (#1260 §2). The maintainer can pipe `pincher` MCP responses or HTTP `/v1/adr action=export` output to a Markdown file when runtime decisions accumulate enough to warrant a checked-in artifact. Deliberate non-feature: the server doesn't write files itself — a tool-can-modify-the-repo footgun would be worse than letting the user pipe. Output format: one H2 section per key, lexicographic ordering (deterministic across re-runs so diff churn is zero when the file is checked in), `---` separator between sections (not after the last). Empty-project response advises how to record. Pinned by `TestRenderADRsAsMarkdown_*` (empty / ordering / structure / plural-grammar) and `TestHandleADR_ExportEndToEnd` (full handler wiring). Tool description + InputSchema both updated; `TestToolContract` snapshot regenerated.
- `docs/architecture/` ships with four Mermaid diagrams (#1331 §2, v0.72). Source-controlled architecture reference rendered inline by GitHub — no SVG build step, no toolchain. Diagrams: **storage-layers** (the single-table three-index design: byte-offset retrieval + knowledge graph + FTS5 BM25, all populated in one `ast.Extract()` pass), **indexer-pipeline** (walker → per-file goroutines → flushBuffers → #1314 no-change gate → resolve passes with #1338 QN preload → tail GC), **mcp-stack** (stdin / HTTP entry → handler dispatch → `jsonResultWithMeta` envelope → session-stats atomic increment → #1320 char/4 token accounting), **watcher-lifecycle** (2s active / 30s idle polling, drift-detect auto-restart, the no-change skip gate). Authoring discipline: update the diagram in the same PR as the code change, per the v0.69 inline-update lockstep rule. Mermaid was chosen over SVG so the diagrams travel with the repo, get reviewed inline, and survive the GitHub Pages render without a build artifact (#1331 acceptance: diagrams render cleanly in Markdown previews).

### Changed
- post-checkout git hook (installed via `pincher init --git-hooks`, #1261 §1 in v0.68) now respects git's no-op signals (#1303 §2a). File checkouts (`git checkout README.md`, where git passes `$3=0`) and re-checkouts of the current branch (where `$1=$2`) skip the reindex entirely — saves the per-call BuildClosure cost (~500 ms on pincher-repo) on every routine file-level operation. Only real branch movement triggers a reindex. post-merge / post-rewrite continue to always fire — their arg shapes have no useful no-op signals worth optimizing for in shell. §2b (schema `branch` column for branch-aware queries) split out to follow-up.
- `_meta.tokens_used` / `tokens_saved_pct` now use a char/4 heuristic by default; cl100k_base BPE is opt-in via `PINCHER_TOKEN_ACCOUNTING=exact` (#1320, v0.69 perf hardening, slice of #670 §2). Bench-driven discovery: `BenchmarkAuth_TimingProfile/correct` had regressed 150 → 1062 allocs/op vs the v0.60 baseline. Profile traced 60% of post-auth allocations to `db.ApproxTokens` running the regex-driven tiktoken encoder on every API call — 895 MB of `regexp2.newMatch` objects per bench run. The cheap default cuts per-call allocations by 64% (1062 → 378 allocs/op, 1.5× faster end-to-end) and aligns with the long-standing user feedback that the per-call savings panel is too noisy. Operators benchmarking real token consumption can opt back in at server start; the session-flush aggregator and the per-call envelope use the same code path, so opt-in mode restores exact BPE counts everywhere. Per-call envelopes shift by ~5-15% under cheap mode (char/4 averages ~4 chars/token for English JSON + symbol IDs + snippets). Documented in `docs/REFERENCE.md` "Server-side env knobs". Pinned by `TestApproxTokens_DefaultHeuristic` (default behavior), `TestApproxTokens_ExactBPE_OptIn` (env-set BPE path), and `TestApproxTokens_HeuristicAvoidsBPEAlloc` (allocation regression guard).
- cold-path resolve passes pre-load the project's symbol-by-QN map once when `sum(pending)` exceeds the threshold gate (#1338 v0.71 perf). Pre-fix `resolveCalls` / `resolveReads` / `resolveImports` each ran one `GetSymbolsByQN` DB query per unique QN — measured at ~20% of cold-path allocations on a Go-heavy project. Adds `db.LoadAllSymbolsByQN(projectID)` for the bulk SELECT and routes the three resolve closures' lookups through a wrapper that prefers the pre-loaded map when present, falling back to per-call queries otherwise. Threshold-gated at 1000 pending edges so small / sparse projects (YAML-heavy K8s, JSON-heavy NodeMonorepo bench fixtures) keep the lighter per-call path; bulk-scan cost would dominate on those. Pinned by `TestLoadAllSymbolsByQN_GroupsCorrectly` (sibling-QN grouping correctness) + `TestQNPreloadThreshold_GateFiresAboveThreshold` (integration: 550-caller corpus exceeds threshold, pre-loaded resolve produces same edge count as per-call path). Threshold value can be retuned once a representative large-corpus benchmark lands.

### Fixed
- watcher no-change tick now skips the cross-file resolve pass + the Python-source-root WalkDir (#1314 + #1317, both slices of #670 §2). Pre-fix: `loadOrFallback + resolveImports/resolveCalls/resolveReads` plus `ast.PythonSourceRoots` ran unconditionally at the tail of every `Index()` call, pulling the full pending_edges table, re-running QN lookups, and re-walking the file tree for `__init__.py`. Caught by bench regression validation vs v0.60 baseline: 14× allocation regression on `BenchmarkIndex_Incremental_NoChange_GoProject` (331 → 4806 allocs/op). Gates now trip on `force || totalFiles > 0` for both the resolve block and the Python-roots scan; safe because pending edges only enter the table via per-file goroutines that re-extract (which increment `totalFiles`), and the roots are consumed only inside the resolve block. Post-fix measurement on the same corpus: 4806 → 716 allocs/op (-85%), 5,215,078 → 948,573 ns/op (5.5× faster). Cold + force paths unchanged. Regression pinned by `TestIndex_NoChange_SkipsResolvePass` (allocation ceiling) + `TestIndex_FileChange_TriggersResolvePass` (positive control, edge correctness).
- `tracelatencybench` now falls back progressively (5 → 3 → 1 outbound-edge floor) when the strict candidate pool is empty (#1316). Pre-fix: hard-coded `minOutboundEdges = 5` always wins on real corpora but yields zero samples on small smoke-test fixtures, so `TestRun_E2E_TinyProject` failed with "no symbols with edges in project" against a 3-symbol / 2-edge corpus — masking the bench tool's CI verification before each release. The fall-back keeps the rigorous floor for #1162 / #685 measurement runs (degenerate shallow roots still excluded on >5k-symbol projects) while letting the E2E test exercise the full code path on a 3-symbol corpus. Discovered during #670 §2 perf-regression validation; the dormant E2E failure surfaced on every PR run for the duration of v0.69 hardening.
- HTML extractor no longer emits Section symbols with zero byte ranges when `bytesFindHeadingAfter` can't locate the heading in the raw source. Surfaced by `pincher doctor` against pincher's own `docs/index.html`: a Section symbol named `codebase_intelligencefor_llm_agents.where_pincher_is_going.v1_0_schema_freeze_migration_guide_planned` had `start_byte=0, end_byte=0` — flagged by `recordExtractionHeuristics` as `byte_range_negative`. Root cause: `bytesFindHeadingAfter` returned `0` for both "match at offset 0" and "not found"; when consecutive headings both failed the lookup (rendered inner text diverging from raw bytes for multi-element headings — nested `<span>`, `<br>`, or templated content), both got `startByte=0`, and the hierarchy loop computed `endByte=0` from the next heading's offset. Fix: return `-1` as the not-found sentinel (impossible to be a real heading offset — no HTML doc starts with `<h1>` at byte 0), and skip the heading at the caller rather than emit a malformed Section. Pinned by `TestHTML_UnlocatableHeadingSkipped` against a fixture that mirrors the failing shape (`<h2>` with `<br>`-separated inner text). The skip drops one symbol on pathological headings instead of crashing the file's symbol space; the rest of the file's hierarchy still extracts cleanly.
## [0.68.0] — 2026-05-16 — Falsifiable savings + closure correctness + branch-switch reindex

v0.68 is the Phase 2 testing-depth release. (1) **Falsifiable savings measurement** — `pincher bench` ships as the runs-on-your-own-project artifact answering "is pincher saving me tokens on MY code?" Times search/context/trace against a real sample, computes the full-file Read baseline, reports per-tool latency + savings%. `--persist` writes to schema v29 `bench_runs`/`bench_results` tables; `GET /v1/bench-results` + dashboard Bench History panel surface predicted-vs-actual savings over time per project. The bench tool itself was found broken mid-cycle (passing nil edgeKinds → 0× ratio for an entire release); fix + empty-result accounting shipped before any measurement claims. (2) **Closure correctness (#685 phase 2)** — schema v30 records `via_kind` on closure rows so the trace fast-path populates `Via` identically to the CTE path; `BuildClosure` now filters source edges to the default trace kind set, making closure data semantically equivalent to CTE for default-kinds queries (pre-fix the closure traversed all edge kinds and returned a superset disagreeing with CTE — caught by the #1162 measurement run). (3) **#1162 measurement decision** — re-ran the bench against four 10k-40k file corpora (pincher-repo, ClaudeCode, Codex, sniffer); mean ratios 2.3-5.6×, p50 sub-microsecond on both paths, empty-result parity now perfect post-#685. The 10× p50 bar isn't met on real-world corpora; closure tables stay opt-in via `PINCHER_CLOSURE_TABLES=1` for users with massive call graphs where the Codex-scale 5.6× speedup matters. (4) **Ergonomics + correctness** — `pincher init --git-hooks` installs post-checkout / post-merge / post-rewrite hooks so branch switches trigger eager reindex instead of leaving the index in a mixed-branch state. (5) **Observability gates** — `traces_otlp` capability runtime probe added so a future regression silently demoting the live exporter back to noop fails CI loudly; OTLP tracer coverage push 12.5% → 87.5%. Schema v28 → v30 (composite PK retained from v28; bench tables v29; closure via_kind v30). Deferred to v0.69 (hardening): remaining slices of #1164 testing depth + #1260 distribution polish. Deferred to v0.71+ (next phase-2 feature window): #1177 TS receiver-type, #1182 Rust AST, #1183 Java AST (heavy AST work doesn't fit hardening cadence).

### Added
- Runtime probe for the `traces_otlp` capability (#1164 deliverable). Closes the gap left by #1163's OTLP-traces release — every advertised capability now has a probe that fires when the tag is present, catching regressions where the live exporter silently demotes to noop while the capability stays advertised.
- OTLP tracer coverage push (#1164 deliverable): `newOTLPTracer` 12.5% → 87.5%, `tracerOrNoop` 40% → 100%, total server package 92.0% → 92.4%. Six new tests exercise the live-exporter init path against a localhost stub collector — covers scheme strip, insecure-via-env-flag, trailing-slash endpoint, noop fallback contract, and the nil-server / nil-tracer defensive branches.
- `pincher init --git-hooks` installs post-checkout / post-merge / post-rewrite git hooks into `.git/hooks/` so branch switches, fast-forward merges, and rebases trigger an eager reindex instead of relying on `Watch()` to catch the changes one diff-pass at a time (#1261 §1). Each hook carries a `pincher.io/managed` marker so future runs can safely replace them without clobbering hand-written user hooks; non-pincher hooks are skipped unless `--force` is set (then backed up to `.pincher-backup`). The hook is a POSIX sh script with a `command -v pincher` guard, so missing pincher never breaks the git workflow. §2 (schema `branch` column for branch-aware queries) deferred to its own issue.
- `pincher bench --persist` flag + schema v29 `bench_runs` / `bench_results` tables + `GET /v1/bench-results?project=ID&limit=N` HTTP endpoint + dashboard Bench History panel rendering predicted-vs-actual savings% per tool per project (#1263 follow-up). Per user mid-v0.67 ask: "having the results pop up on the http would probably be good. then long term you keep estimated results with actual result for any project that pincher bench ran on."
- `pincher bench` subcommand — falsifiable token-savings measurement against the user's own indexed corpus (#1263 §1). Times search/context/trace against the largest project (or `--project ID`), computes a full-file-Read baseline per touched file, reports per-tool p50/p95 latency + actual-vs-baseline tokens + savings%. `--json` for CI; `--seed` for reproducibility. The eval-harness §2 (canonical workflow corpus + external comparator implementations) rolls forward to v0.69+.

### Changed
- Closure phase 2 (#685): closure rows now record the last-hop edge kind in a new `via_kind` column (schema v30). Trace fast-path populates the `Via` field from this column — closes the v0.54 phase-1 trade-off where `Via` was empty when closure tables were enabled. `BuildClosure` now also filters source edges to the default trace kind set (`{CALLS, HTTP_CALLS, ASYNC_CALLS}`) so closure semantics match what the CTE returns under the trace tool's default kinds filter — pre-fix the closure traversed ALL edge kinds while the CTE filtered to CALLS family, so the fast-path returned a superset that silently disagreed with the CTE path (#1162 measurement caught this). The trace fast-path gate (`isDefaultTraceKinds`) is retained: non-default-kind queries fall through to CTE because closure data is now intentionally scoped to the default set. Pre-v30 closure rows surface `Via=""`; rebuild via the next `Index()` pass or `pincher closure rebuild` to backfill. Unblocks #1162 default-flip re-measurement on apples-to-apples data.

### Fixed
- `cmd/tracelatencybench` was passing `nil` edgeKinds to `TraceViaCTEScoped`, which errors with "edgeKinds must not be empty" — every CTE call returned `0 ns + empty results`, flooring the ratio to `0.0×`. The tool reported "0.0× p50 improvement" for a release cycle while measuring nothing. Caught during the #1162 default-on measurement run. Fix: pass the same `{CALLS, HTTP_CALLS, ASYNC_CALLS}` default the trace tool uses (`internal/index/indexer.go::TraceByID`). Also: report empty-result + error counts so the next regression of this shape can't hide; filter samples to ≥5 outbound edges so p50 isn't buried below timer resolution; fall back to mean-based ratio when closure p50 is sub-microsecond.
## [0.67.0] — 2026-05-16 — Dashboard panel triad + OTLP observability + #1134 resolver fix

v0.67 ships in four arcs. (1) **Dashboard substrate becomes user-facing** — the schema v27 `session_tool_calls` event log lit up four Overview panels (per-tool call breakdown, per-complexity-tier mix, per-tool payload-size distribution, and Tool-Mix Health entropy) plus a Backend Status strip that surfaces the live observability state. Each panel pairs with a doctor advisory (payload-outliers + tool-mix stuck-loop) so CLI-only users get the same actionable signal. (2) **OTLP traces complete #1163's observability story** — per-tool-call spans and per-index-pass spans flow through `OTEL_EXPORTER_OTLP_ENDPOINT` via `otlptracehttp`, zero-allocation no-op when unconfigured, graceful shutdown wired at process exit, `res.IsError=true` correctly stamped as Error status, capability `traces_otlp` advertised only when the exporter initialized. Pincher now exposes all four standard observability surfaces (metrics, traces, events, correlation IDs) a production router expects. (3) **Multi-language extractor scoping** — Rust `impl` blocks and Swift `extension` blocks now scope methods to their receiver type via the new `scopeRE` framework field; Kotlin extension functions stop extracting fake `String`/`List`/`Map` symbols. Plus #1134's Go resolver fix (range over receiver.Field type inference). (4) **Ergonomics + community surface** — `search snippet_lines` knob with query-aware default (exact-id queries skip the snippet read entirely), the `cmd/tracelatencybench` utility unblocks #1162's closure-default-on measurement, CONTRIBUTING.md + docs/troubleshooting.md ship for human contributors, 5 new `area:*` issue labels applied to triage open work, README leading paragraph re-framed to lead with the token-savings outcome. Deferred to v0.68: #1183 Java AST, #1182 Rust AST (impl-scoping shipped partial), #1177 TS receiver-type, #1162 closure-tables-default-on (bench tool shipped; awaiting 10k-file corpus measurement run).

### Added
- `search`: new `snippet_lines` argument (int, 0–20, default 5) controls the per-result source-snippet line count. Pass `snippet_lines=0` to skip the per-result byte-offset disk read entirely when the agent already knows what it's looking for (exact-identifier queries where the snippet is dead weight) — saves ~75–100 tokens per result. Pass up to 20 for triage queries needing more context. Out-of-range values are clamped with a warning. Default of 5 preserves the historical behaviour; back-compat for callers that depend on snippet presence. Distinct from `fields=` projection — the projection still pays the read cost, `snippet_lines=0` is the cleaner skip. Read cap scales with the requested line count so a 20-line ask doesn't get truncated by the historical 2 KB ceiling. ([#1091](https://github.com/kwad77/pincher/issues/1091))
- New CLI utility: `cmd/tracelatencybench` — measures trace-query latency with and without the closure-table fast-path on a real pincher DB. Reports p50 / p95 / max / mean latency for both paths plus the ratio (the headline number for the #1162 default-on decision: acceptance gate is ≥10× p50 improvement at 10k+ files). Builds closure tables on the fly so it can be run against any indexed project; samples 200 Function/Method symbols by default (random, biased to symbols with at least one edge so degenerate roots don't floor the measurement). Markdown-row mode (`-md`) for direct paste into the #1162 acceptance comment. Companion to `cmd/closurebench` (storage measurement, #639); together they cover both acceptance dimensions for closure-tables-default-on. ([#1162](https://github.com/kwad77/pincher/issues/1162))
- Dashboard: **Backend Status strip** at the top of the Overview tab — compact horizontal row of chips showing the active schema version plus the live state of the three observability surfaces (metrics_prometheus, event_stream_sse, traces_otlp). Reads from `/v1/health`'s new `observability` field (#1280). Green border + green value when "on"; muted text when "off." Hover for the full surface description including the active endpoint. Bridges the discoverability gap shipped in PR #1280 (health-tool surface) to the visual dashboard so users see whether traces are flowing without dropping to the CLI. ([#1163](https://github.com/kwad77/pincher/issues/1163))
- `health` tool surfaces a new `observability` field reporting the live state of the three observability surfaces (#1163): `metrics_prometheus`, `event_stream_sse`, and `traces_otlp`. Always-on surfaces render as `"on (GET /v1/metrics)"` etc.; OTLP traces render either as `"on (OTLP/HTTP → <endpoint>)"` when configured or `"off (unset OTEL_EXPORTER_OTLP_ENDPOINT to enable)"` so routers can see at a glance whether spans are flowing. Same signal as `_meta.capabilities` but with the active endpoint added — load-bearing for diagnosing OTLP routing problems without parsing capability tags. ([#1163](https://github.com/kwad77/pincher/issues/1163))
- OTLP traces (indexer scope): per-index-pass span emitted by `Index()` under instrumentation library `pincher.index` with span name `pincher.index.pass`. Stamps the post-pass outcome attributes (`pincher.files_indexed`, `pincher.symbols_total`, `pincher.edges_total`, `pincher.files_skipped`, `pincher.files_blocked`, `pincher.files_deleted`, `pincher.duration_ms`) plus the pre-pass context (`pincher.project_id`, `pincher.project_name`, `pincher.repo_path`, `pincher.force`). Routes through the global OTel tracer provider — when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset, the span pair costs effectively nothing (no-op provider). Pairs with the per-tool-call spans shipped earlier in v0.67; together they cover both indexing and serving on the same OTLP/HTTP transport. ([#1163](https://github.com/kwad77/pincher/issues/1163))
- OTLP traces — one span per tool call, exported via the standard `OTEL_EXPORTER_OTLP_ENDPOINT` env var (completes the traces half of #1163; Prometheus shipped previously). Spans are named `pincher.tool.<name>` with `rpc.system=mcp`, `rpc.method=<tool>`, `pincher.complexity_tier`, `pincher.request_id` (correlated with #657's request-ID middleware so traces and `_meta.request_id` join cleanly), and `pincher.response_bytes` attributes plus OTel-standard `service.name=pincher` resource attrs. When `OTEL_EXPORTER_OTLP_ENDPOINT` is unset (the default) the tracer is a zero-allocation no-op and no spans are emitted — observability never breaks the hot path. The `traces_otlp` capability is advertised only when the OTLP exporter successfully initialized so consumers can distinguish "configured + working" from "no-op fallback." HTTP transport via `otlptracehttp` (no gRPC dep). ([#1163](https://github.com/kwad77/pincher/issues/1163))
- Prometheus metrics endpoint at `/v1/metrics` (#1163). In-process registry exposes `pincher_tool_calls_total{tool,outcome}` (counter), `pincher_tool_latency_seconds{tool}` (summary with `_count` + `_sum` — averages today, bucketed histograms in v0.68), `pincher_tool_tokens_saved_total{tool}` (counter), `pincher_db_size_bytes` / `pincher_wal_size_bytes` (gauges refreshed synchronously on each scrape). Standard Prometheus 0.0.4 exposition format, `text/plain; version=0.0.4` content-type. Hand-rolled registry — no `prometheus/client_golang` dependency — keeps the single-binary install footprint unchanged. New capability tag `metrics_prometheus` advertised in `_meta.capabilities`; runtime probe asserts the endpoint serves 200 with the expected gauges. Routers can scrape pincher with the same tooling they use for any other production service. OTLP traces ship separately in a follow-up PR. ([#1163](https://github.com/kwad77/pincher/issues/1163))
- Rust extraction: **methods inside `impl` blocks now scope to their receiver type**. Both `impl Type { ... }` (inherent impl) and `impl Trait for Type { ... }` (trait impl) forms set the inner methods' Parent to `Type` and upgrade their Kind from `Function` to `Method`. Pre-fix, every method inside an impl block was emitted as a freestanding function with no parent — `dead_code` surfaced them as candidates regardless of whether they were called as methods, `trace`/`neighborhood` couldn't bind them, and qualified-name lookups failed. Implemented as a new `scopeRE` field on the generic regex extractor that lets a language declare "this syntax is a scope container but emits no symbol of its own" — Rust uses it; other regex-tier languages without that shape leave it unset. Also fixed: Rust `funcRE` now accepts leading whitespace so indented impl methods match at all (regression from when indented `fn` declarations were never tested). ([#1183](https://github.com/kwad77/pincher/issues/1183))
- Swift extraction: **methods inside `extension Type { ... }` blocks scope to their receiver type**. Both `extension Type { ... }` and `extension Type: Protocol { ... }` forms set inner methods' Parent to `Type` and upgrade Kind from `Function` to `Method` — parallel to the Rust `impl` scoping shipped earlier in v0.67. Pre-fix Swift extensions emitted methods as freestanding functions with no parent, breaking `dead_code`/`trace`/QN-resolution against types that exposed their public API via extensions (idiomatic Swift). Reuses the `scopeRE` field on the regex extractor introduced for Rust impl. ([#1183](https://github.com/kwad77/pincher/issues/1183))
- `LICENSE` file at repo root. README declared MIT and the badge linked to `LICENSE`, but the file was missing — package managers, code-scanners, and license-aware integrations (Homebrew formula audit, OSI tooling, GitHub's license detector) all read the file itself, not the README claim. ([#1250](https://github.com/kwad77/pincher/issues/1250))
- `_meta.empty_reason` — stable machine-readable enum stamped alongside the existing `_meta.diagnosis` text on every empty-response branch. Agents, aggregators, and fallback chains consume the code; humans read the diagnosis. Twelve-value taxonomy (`no_project_indexed`, `stale_index`, `unsupported_language`, `low_confidence_extractor`, `same_file_only`, `cross_file_unavailable`, `query_too_narrow`, `no_results_in_corpus`, `cap_dropped_all`, `incremental_no_change`, `all_files_blocked`, `extractor_emitted_nothing`) stamped by `search`, `query`, `trace`, `neighborhood`, `dead_code`, `architecture`, `schema`, `list`, `index`, `changes`. `meta=lite` preserves both fields — they're per-call actionable, not dogfood-only. Gate test pins the enum: a stamp site using a literal string instead of the constant fails loud. ([#1252](https://github.com/kwad77/pincher/issues/1252))
- 9-axis per-language capability matrix in `docs/REFERENCE.md` under Language Support. Each row covers: file detection, symbol extraction, IMPORTS, same-file CALLS, cross-file CALLS, type/receiver resolution, docstrings, test-file detection, and confidence tier. The matrix is hand-maintained today; the read-the-extractor-registry-and-resolver-gates source-of-truth note in the section header documents how to keep it accurate when shipping new extractors. Surfaces honest gaps (TypeScript / Rust / Java cross-file resolution is the next AST roadmap target; Haskell remains the only stub-tier language) instead of the prior 4-column overview that under-specified what each language actually supports. ([#1253](https://github.com/kwad77/pincher/issues/1253))
- New MCP tool `context_for_task`: the composite-context tool that takes a free-form task description OR a `seed_id` from a prior `search`, then composes top-N matching seeds via `search`, each seed's source + direct deps via `context`, callers + callees up to `trace_depth=2` via `trace direction=both`, and any `changes` overlap with the resolved seeds — returns one envelope `{seeds, neighbors, callers, callees, recent_changes}`. Replaces the typical 5-10 atomic calls an agent loop fires when picking up an investigation. Empty branches stamp `_meta.empty_reason=no_results_in_corpus` (#1252 enum) with a `next_steps` recovery list pointing at `search` + `guide`. Registered as `heavy` complexity tier with full OpenAPI response schema + tool-contract entry. Tool count 22→23. ([#1259](https://github.com/kwad77/pincher/issues/1259))
- Two new top-level docs: **`CONTRIBUTING.md`** mirrors `CLAUDE.md`'s dev-loop guidance for human contributors (branch + PR shape, CI gates, test conventions, JSON invariants, release process), and **`docs/troubleshooting.md`** captures the top ~10 recurring friction items from the dogfood log with concrete remediation (tool-not-appearing, stale index, WAL bloat, ghost projects, OTLP-not-flowing, cross-file-edges-empty-on-non-Go, etc.). Closes two of the three acceptance items on #1264 (issue-label taxonomy still pending — needs maintainer triage pass to apply). ([#1264](https://github.com/kwad77/pincher/issues/1264))
- Dashboard panel: **Tool-Mix Health (last 7 days)** — fourth and final v0.67 panel landing on the #635 substrate. Computes Shannon entropy of the per-tool call distribution and surfaces a quality band (`rich` / `healthy` / `narrow` / `stuck`) so a glance answers "is the agent exploring or stuck in a 2-tool loop?" Shows entropy in bits, an evenness bar (entropy / log₂(distinct-tools) so the visualization is independent of catalog size), the top-1 and top-3 concentration ratios, and the top-3 tools by call volume. Computed client-side from the existing `/v1/tool-call-stats` response — no new endpoint needed, no new DB query. This closes #635's planned panel triad (the dashboard issue originally listed entropy, payload size, and per-tier %; all three now ship). ([#635](https://github.com/kwad77/pincher/issues/635))
- Doctor advisory: **payload outliers in the last 7 days**. Surfaces tools whose worst-case response is ≥10× their average AND ≥100 KB absolute — the calls that occasionally blow up agent context windows. Top 3 (by spread ratio) are named with `max / avg / Nx spread` and a remediation pointer to `/v1/tool-payload-stats` or the dashboard's Response Payload Size panel. Adds the new advisory to both the MCP `doctor` tool (internal/server/admin.go) and the `pincher doctor` CLI (cmd/pinch/doctor.go) per the bounded-duplication convention. Same data source as PR #1270's dashboard panel; CLI users who don't visit the dashboard get the same actionable signal. Silent on healthy data — never noisy. ([#635](https://github.com/kwad77/pincher/issues/635))
- Dashboard panel: **Response Payload Size by Tool (last 7 days)** — third v0.67 panel from the #635 substrate. Surfaces min/avg/max response_bytes per tool plus a max:avg "spread" badge (`tight`/`wide`/`spike`) so users can spot tools that occasionally return outsized payloads — the silent token-bill blowers. Sorted by max_bytes DESC server-side so the loudest tools are at the top. Backed by new `/v1/tool-payload-stats?window_seconds=…&limit=…` endpoint and reader-routed `ToolCallPayloadSizeByTool` query (GROUP BY tool with MIN/AVG/MAX/SUM over the trailing window). Empty-store returns `tallies:[]` not `null` (the #330 invariant the dashboard JS relies on). Completes the planned Overview-tab triad: per-tool call count + per-tier complexity + per-tool payload spread, all reading the schema v27 session_tool_calls event log. ([#635](https://github.com/kwad77/pincher/issues/635))
- Dashboard panels: **Tool Call Breakdown (last 7 days)** and **Calls by Complexity Tier (last 7 days)**. First two v0.67 panels rendering data from the schema v27 `session_tool_calls` event log (#635 substrate landed in v0.64; this is the panel layer). The per-tool panel shows one row per tool with call count, average tokens used, cumulative tokens saved, and average savings percentage, plus a 7-day total footer. The per-tier panel shows a stacked horizontal bar splitting calls across the `lite`/`standard`/`heavy` complexity tiers (the routing dimension #1191 added) plus a table with call count, average tokens used, and cumulative tokens saved per tier. Admin-shape tools without a Read/Grep baseline (architecture/list/schema) show `—` in the saved% column rather than 0% so users don't misread them as "no savings." Backed by new `/v1/tool-call-stats?window_seconds=…&limit=…` and `/v1/tool-tier-stats?window_seconds=…` HTTP endpoints and `ToolCallStatsByTool`/`ToolCallStatsByTier` DB queries — all reader-routed; GROUP BY tool/tier with `AVG()` over the trailing window. Empty-store path returns `tallies:[]` not `null` (the #330 invariant the dashboard JS relies on). NULL `tokens_saved_pct` rows excluded from the average so admin tools don't drag the search/symbol numbers down. Empty-tier rows (pre-#1191 shape) filtered from the tier panel so legacy data doesn't show up as an "unknown" bucket. ([#635](https://github.com/kwad77/pincher/issues/635))
- Doctor advisory: **tool-mix stuck loop**. Fires when the agent has been essentially repeating one tool over the trailing 7-day window — entropy <1.0 bits AND ≥100 total calls AND top-1 tool share >80%. The triple-gate keeps the advisory silent on fresh installs and legitimately narrow workloads; when it fires, it names the dominant tool, its concentration percentage, and the Tool-Mix Health dashboard panel for follow-up context. Pairs with the entropy panel from PR #1273 — same signal in the CLI/MCP doctor surface. Added to both `internal/server/admin.go` (MCP) and `cmd/pinch/doctor.go` (CLI) per the bounded-duplication convention. ([#635](https://github.com/kwad77/pincher/issues/635))

### Changed
- `search`: `snippet_lines` now uses a **query-aware default** (Option B from #1091). Exact-identifier queries (single token, no wildcards/spaces/quotes) default to `snippet_lines=0` — the agent already knows the target name, the snippet is dead weight. Phrase / wildcard / multi-word queries keep the historical default of 5. Same heuristic as `min_confidence`'s query-aware default per #247. Closes #1091. ([#1091](https://github.com/kwad77/pincher/issues/1091))
- OTLP traces: per-tool-call spans now record `Error` status when `res.IsError=true` (the standard pincher protocol-level error shape — `errResult` / `errResultRich`), not only when a Go-level error is returned. Pre-fix the span showed `Ok` for every protocol-level error, making OTLP latency dashboards over-optimistic by ~80% on bursty error sessions. The new `pincher.is_error=true` attribute lets routers filter the two paths apart when needed. Extends the per-tool-call span shape shipped in #1272. ([#1163](https://github.com/kwad77/pincher/issues/1163))
- OTLP traces: graceful shutdown wired at process exit. cmd/pinch's signal-shutdown defer now flushes pending spans via `srv.ShutdownTracer(ctx)` (5-second timeout bound) before exit — without this, the `BatchSpanProcessor`'s final spans could be lost when the user pressed Ctrl-C or the supervisor sent SIGTERM. Also drops two unused helpers from `internal/server/otlp_tracer.go` (`Tracer()` accessor + `formatOTLPEndpoint()` formatter) that were dead since the tracer landed in #1272. Coverage: ShutdownTracer now exercises no-op, nil-server, and wired-provider paths. ([#1163](https://github.com/kwad77/pincher/issues/1163))
- README leading paragraph re-framed to lead with the **token-savings outcome** (the grab) and the **routing pitch** (the retention) instead of storage architecture. The 80×+ savings example block and the `_meta`-envelope sample remain prominent; the byte-offset / graph / FTS5 layer breakdown moves to a supporting paragraph below since architecture details are not what an evaluator scanning the README for 30 seconds is looking for. ([#1251](https://github.com/kwad77/pincher/issues/1251))
- Documentation: bump stale "22 MCP tools" references to **23** across README, docs/REFERENCE.md, docs/index.html, and inline server.go package/doc comments. The tool count drifted when `context_for_task` shipped in #1259 (v0.67) but the surface counters didn't follow — a stale headline number erodes trust faster than missing details, per the release-prep checklist. ([#1276](https://github.com/kwad77/pincher/pull/1276))
- Documentation: new **Observability** section in `docs/REFERENCE.md` documenting the four standard surfaces (metrics_prometheus, traces_otlp, event_stream_sse, request_id_correlation) with the full metric catalog, the standard env-var setup for OTLP, and the span-attribute reference. Also extends the "Additional HTTP endpoints" table to cover the five panel-helper endpoints shipped in v0.67 (`/v1/hook-stats`, `/v1/tool-call-stats`, `/v1/tool-tier-stats`, `/v1/tool-payload-stats`, `/v1/metrics`) — previously discoverable only by reading the dashboard JS. ([#1277](https://github.com/kwad77/pincher/pull/1277))

### Fixed
- Resolver: `context.callees` no longer lists an unrelated project Method when a function body iterates `for _, x := range receiver.Field` and then accesses `x.fieldName` where `fieldName` is also the name of some Method elsewhere in the project. Root cause was in the Go AST extractor's local-variable type-inference pre-pass — the `RangeStmt` handler only handled the `*ast.Ident` form of the iterable (`for _, x := range slice`), not the `*ast.SelectorExpr` form (`for _, x := range receiver.Field`). The element variable's type stayed unknown, the later dotted-name READS edge had empty `BaseType`, and the binding pass's `isStructFieldRead` suppression couldn't activate. Added a per-file struct-field-type table built once at `extractGo` entry; the `RangeStmt` SelectorExpr branch resolves the iterable's element type through it. Same-file struct types only (cross-file is a follow-up). ([#1134](https://github.com/kwad77/pincher/issues/1134))
- Kotlin extraction: **extension functions now extract under the real method name**, not the receiver type. Pre-fix `fun String.toSlug()` extracted a fake `String` symbol because the regex captured the first identifier after `fun` — the receiver-type prefix — and silently dropped the real method name. Same family of regex-extraction bug as the v0.66 `extractor_rust_java_calls_test.go` shape. Receiver types with generics (`fun List<Int>.firstOrDefault()`) tolerated. The receiver type is currently skipped without setting `Parent` — full extension-as-method scoping (parallel to the Rust `impl` work in #1282 and Swift `extension` in #1283) tracked separately as a follow-up. ([#1183](https://github.com/kwad77/pincher/issues/1183))
- Cross-project symbol ID collision in shared DBs (#1231). Pre-v28, `symbols.id` was the primary key without `project_id`; `MakeSymbolID` returns `"{file_path}::{qualified_name}#{kind}"` with no project scope. Two projects sharing the same relative file path with the same symbol produced identical IDs, and `INSERT OR REPLACE` silently flipped the row's `project_id` to the latest writer — on a shared DB, pincher-repo's `server.go` showed only 8 of 75 Methods because sniffer (an older pincher mirror) clobbered the 67 pre-existing rows. Schema v28 makes the primary key composite `(project_id, id)`; the same id in two projects is now two rows. JOIN sites in `db.go` and `cypher/engine.go` updated to include `AND edges.project_id = symbols.project_id` so cross-project edge traversal can't surface a different project's symbol row. Regression test `TestSymbol_CrossProjectIDCollision_BothMustSurvive` pins the contract. Existing shared DBs will see the 8GB+ migration take 30-90s on first open; the data preserved is whatever `INSERT OR IGNORE` retained pre-fix (the bug damage from years of overwrites isn't recoverable, but new writes will never collide again). ([#1231](https://github.com/kwad77/pincher/issues/1231))
## [0.66.0] — 2026-05-16 — Thin-client envelope + silent-cross-project guard + DOGFOOD haul

v0.66 ships in three groups. (1) **Thin-client payload umbrella (#1224, four PRs)** — `trace compact` / `search compact` drop dogfood-only fields from per-hit + per-hop entries; `_meta=lite` (env + per-call) prunes ~150-200 tokens off every response envelope; `trace max_hops` (default 50) + `neighborhood include_fixtures` close the last two bloat sources. Real perf win for search compact is the skipped per-hit snippet disk read (20× fewer byte-offset reads on bulk searches). (2) **Silent-cross-project guard (#1232, three PRs)** — `symbol` / `context` / `neighborhood` now error on omitted-project requests whose ID lives only in an off-session project (sniffer mirrors, MCP_Combine staging). Pre-fix the warning string was the only signal — agents that don't parse warnings consumed cross-project data unintentionally. `cross_project=true` preserves the legacy silent-fallback shape; same silent-confidently-wrong family as #935 / #1217. (3) **DOGFOOD bug drain** — 16 fixes from probing the v0.65 surface: doctor cross-project query collapses 130 round-trips into 1 (#1205), per-project byte estimate answers "WHICH project to prune" on 11GB DBs (#1220), C/C++ macro over-extraction (#1204), TS overload duplicate suppression (#1208), Markdown sibling-heading false-positive (#1207), pinchQL column-vs-column silent-drop (#1217), guide audit-shape routing (#1230), WAL bloat advisory (#1206), pincher vacuum 4-step flow (#1219), parity-check guard for #1231 (#1233/#1234), schema_vN runtime probe (#1202), and more. Plus #1097 (Markdown preamble extraction) closes the self-dogfood gap where pincher couldn't find its own README tagline. Deferred to v0.67: #1183 Java AST, #1182 Rust AST, #1177 TS receiver-type, #1162 closure-tables-default-on (needs measurement), #635 dashboard panels, #1231 root cause (observability shipped, awaiting live repro).

### Added
- MCP spec 2025-11-25 compliance — three tool-metadata fields now populated. (1) `CallToolResult.StructuredContent` set alongside `TextContent` in every `jsonResultWithMeta` response (#1077) — compliant clients consume the parsed object directly and skip re-parsing the JSON text on every call. (2) `Tool.Title` (display name) declared for 8 ambiguously-named tools (neighborhood = "Same-file symbols", query = "pinchQL graph query", context = "Symbol with imports & callees", dead_code = "Unreachable symbols", init = "Seed editor MCP config", adr = "Project decision store", rebuild_fts = "Admin: rebuild FTS5 indexes", self_test = "Admin: install smoke test") — #1078; non-breaking, `Name` stays as the stable identifier. (3) `Tool.Annotations` (`readOnlyHint` / `destructiveHint` / `idempotentHint` / `openWorldHint`) declared for every tool via four behavioral presets (readOnly / write / destructive / external), letting MCP hosts skip confirmations on read-only paths and warn on destructive (rebuild_fts) or open-world (fetch) ones — #1076. Metadata maintained in a side `toolMetadata` map so each tool literal stays focused on Name + Description + InputSchema.
- Markdown extractor now emits a synthetic `preamble` Section covering content before the first heading. Pre-fix, README banners + badges + tagline + nav (and design-doc lead paragraphs) were invisible to `search corpus=docs` — pincher's own README tagline (`"Codebase intelligence server for LLM agents"`) returned 0 results because the extractor only emitted Section symbols for actual headings, and lines 1-20 of every README had no `#` heading. The synthetic preamble covers byte 0 through `lineStartAt(headings[0])` (or end-of-file when there are no headings at all), giving FTS5 something to match for the front-of-document content users land on first. Whitespace-only preambles are skipped (no point emitting an empty Section). Files that open with `# Title` on line 1 have no preamble symbol — there's no pre-heading content. Closes the self-dogfood gap where pincher couldn't find its own tagline (#1097).
- **doctor advisory: WAL bloat detection** (#1206, v0.66 DOGFOOD). Pincher's WAL is supposed to stay bounded by SQLite's `journal_size_limit=256 MiB` pragma + the indexer-tail `CheckpointTruncate()`. Under sustained concurrent indexing pressure, busy readers can pin the WAL across the truncate cycle and it grows unbounded — real-world DOGFOOD observation: an 11GB DB reached a 2.3GB WAL (9× the configured limit). The user had no signal until a tool call latency blew out (every read touches the WAL first) or vacuum reported `wal_reader_busy` (#1149). New advisory fires when WAL > 512 MiB OR WAL > 10% of DB (with DB ≥ 100 MiB so tiny test/fresh installs don't trip the percent rule). Five tests pin: positive absolute and percent thresholds, negative healthy WAL and tiny-DB skip, cross-check advisory names `pincher vacuum` as remediation, integration confirms wiring into handleDoctor response.
- `doctor` emits a `nested-project` advisory when one indexed project's path is a strict subdirectory of another's (e.g. `warp_rc/` and `warp_rc/warp-fork/` both registered). Pre-fix the duplication was invisible — same source files were extracted twice under different project IDs, doubling DB load and surfacing duplicates in cross-project queries. Advisory names the inner project + recommends pruning the redundant root. Pairs with the per-project `qualified_name_collision` detector (already correct) by surfacing the upstream registration mistake the user actually has to fix (#1209).
- `pincher vacuum` now runs the full 4-step reclaim flow advertised by the doctor advisories. Pre-fix the command ran VACUUM + wal_checkpoint(TRUNCATE) only — the two load-bearing steps. The `pincher vacuum` advisory text in #732 (large-DB) and #1206 (WAL-bloat) named the command as the one-shot remediation, but two of the four steps from the original spec were missing: PRAGMA optimize (re-analyzes table stats so subsequent query plans are fresh post-rewrite) and per-vtab FTS5 `INSERT INTO symbols_{code,config,docs}_fts(...) VALUES('optimize')` (compacts the inverted-index segments). These are *advisory* — failures populate `VacuumResult.OptimizeError` and `VacuumResult.FTSOptimizeError` (surfaced in the CLI receipt + `--json` payload as `optimize_error` / `fts_optimize_error`) but don't gate the load-bearing reclaim from succeeding. Net effect: a vacuum on a long-running install now leaves the query planner and FTS5 index in a fresh state too, not just the data file (#1219).
- `doctor` per-project `db_bytes_estimate` answers "WHICH project should I delete first?" on multi-GB DBs. Pre-fix doctor reported db_size_bytes as a bare total — on an 11.9 GB DB with 120 projects, a user couldn't tell whether warp_rc was 100 MB or 4 GB, so the bloat advisory (#732) pointed at the symptom without quantifying contributors. Each project summary now carries a best-effort `db_bytes_estimate` (int64): per-row SUM of text-column LENGTHs in `symbols` + `edges`, plus per-row index overhead, plus a ~50% FTS5 contribution heuristic. New `Store.EstimateProjectBytes()` reader-pool method (two grouped SUMs, one call per doctor invocation). Sum across projects undershoots `db_size_bytes` by 10-40% — the gap is page-fragmentation slack + WAL + schema overhead that can't be cheaply attributed per project without parsing the b-tree. The load-bearing property is *relative ordering*, not absolute bytes: agents pick the biggest project to prune first, then `pincher vacuum` (when #1219 lands) reclaims the space. Doctor tool description updated to call this out so agents know the field is an estimate (#1220).
- `trace` gains two thin-client knobs from the #1224 umbrella. **`compact=true`** drops per-hop `kind` + `via` fields and the top-level `risk_summary` block — on hot-path traces these account for 30-50% of payload that thin-client consumers (Cursor / Continue / Claude Desktop) don't render. The per-hop minimum shape becomes `{id, name, file_path, start_line}` plus the wrapper's `depth`; risk-aware consumers (dashboards, dogfood, risk-aware reviews) keep the default non-compact shape. **`include_fixtures=true`** (default `false`) splits the pre-#1225 combined `include_tests` filter so callers can selectively unlock real tests (a true source-of-truth signal on "who exercises this symbol?") without also unlocking pincher's pinned-corpus fixtures (pure inputs to snapshot tests). Pre-#1225, `include_tests=true` unlocked both; post-#1225 it unlocks tests only. Mirrors #1212's pinchQL fixture filter. **Breaking** for callers that depended on `include_tests=true` to also surface fixture-path hops — pass `include_fixtures=true` alongside to preserve the legacy combined behaviour. Tool description updated with both flags + the #1212 cross-link (#1225).
- `search compact=true` ships PR 2 of the #1224 thin-client umbrella. Drops per-hit `extraction_confidence` + `language` + `snippet` and the top-level `confidence_distribution` summary. The compact response also **skips the per-hit snippet disk read entirely** — that's the real perf win on bulk searches (default 20 hits × snippet-read = 20 byte-offset disk reads avoided per call). The thin-client minimum shape becomes `{id, name, qualified_name, kind, file_path, start_line, end_line, signature, score}` per hit — enough to drive a follow-up `symbol`/`context`/`trace` call. `language` is dropped because per-hit `kind` already disambiguates Function/Method/Class etc. and the language is a per-project constant for most real searches. Default `false` preserves the current shape for dashboard / dogfood / quality-aware consumers that inspect `score`/`extraction_confidence` to refine queries. Mirrors #1225's `trace compact` naming. Compact takes precedence over `fields=` projection — a caller passing both gets the compact shape (#1226).
- PR 3 of 4 from the #1224 thin-client umbrella. `_meta=lite` envelope drops the dogfood-only fields from every tool response. Triggered EITHER via `PINCHER_META=lite` env var on the MCP child (sticky for the session, ideal for thin-client deployments like Cursor / Continue / Claude Desktop) OR via per-call `meta=lite` arg (per-call escape hatch — useful when a dogfood probe wants the full envelope in a mostly-thin-client session). Lite drops: `capabilities`, `baseline_method`, `complexity_tier`, `tokens_used`, `tokens_saved`, `tokens_saved_pct`. Keeps the actionable per-call signal: `latency_ms`, `request_id`, `warnings`, `diagnosis`, `next_steps`, `index_in_progress`. Per-call cost reduction: ~150-200 tokens off every response. Pattern parallel to #622's `verbose` universal arg + #1089's `PINCHER_DEBUG_META=1` env. Session-level stats accumulation (`statsTokensUsed` / `statsTokensSaved`) uses the count BEFORE the prune, so session metrics stay honest even when individual responses don't carry the field. `meta` joins `verbose` as a universal arg recognized by every tool — no per-tool InputSchema updates needed (#1227).
- PR 4 of 4 closing the #1224 thin-client umbrella. Two changes. **`trace max_hops`** (default 50, configurable) caps the total hop count returned. A hub function (logging utility, error helper called from 200+ sites) returns 100+ hops at depth=1, and the auto-deepen trim doesn't help because depth=1 already crosses the ≥5-hop threshold. When the cap fires, `_meta.truncated=true` + `_meta.total_before_cap` + `_meta.max_hops` surface so the caller knows the response was capped and can re-issue with a wider `max_hops`. Same shape as architecture's hotspot cap. **`neighborhood include_fixtures`** (default false) refuses seeds whose `file_path` lives in a pinned-corpus fixture path (`testdata/`, `__fixtures__/`, `test-fixtures/`) — neighborhood scopes by file, so a fixture-path seed means the entire neighbor list is test inputs and the agent likely meant a real source-tree symbol. Returns a rich-error with the `include_fixtures=true` opt-in remediation. Mirrors #1212's pinchQL fixture filter + #1225's trace fixture filter. With #1228 landing, all four thin-client umbrella PRs (#1225 trace compact + fixture, #1226 search compact, #1227 _meta=lite — deferred, this PR) are shipped or accounted for (#1228).
- MCP `index` tool surfaces #1233's parity-check counts. Without this, the per-file `pincher.index.parity.mismatch` warnings fired in the daemon's stderr but were invisible to MCP callers — the agent ran an index, saw a clean response, and assumed the run was healthy. Now: when `IndexResult.ParityMismatchFiles` is non-zero, the response carries `parity_mismatch_files` + `parity_missing_symbols` fields plus a `_meta.warnings` entry naming #1231 with a stderr-grep hint for the per-file detail. Healthy runs omit both fields so the response stays clean. Discovered when the parity guard from #1233 fired in production against pincher-repo's own re-index (lost 1945 of 5781 expected symbols) but the MCP response showed no warning — extracted helper `buildIndexResponseData` makes the shape testable in isolation (#1231 follow-up to #1233).
- Post-pass parity-check guard surfaces silent symbol loss during indexing. The per-file goroutine now records its extracted-symbol count under `bufMu`; after the final flush + resolvers, `runParityCheck` compares against `Store.SymbolCountsByFile` (a new reader-routed COUNT(*) GROUP BY file_path helper) and logs `pincher.index.parity.mismatch` for any file with <90% retention. A single `pincher.index.parity.summary` warning fires when any losses are detected, pointing at #1231 with remediation hint. `IndexResult.ParityMismatchFiles` + `IndexResult.ParityMissingSymbols` carry the counts so CLI `--json-summary` (omits both when zero) and the MCP `index` tool can surface them. Observation-only — never gates a successful index run. The underlying persistence-loss root cause is still under investigation; the guard makes the symptom visible in real time so the next instance of #1231 is debuggable rather than silent.

### Changed
- **Breaking:** `context` no longer silently returns cross-project data. Mirror of the `symbol` arm shipped in PR #1241 — extended to `context`, which is the more dangerous shape because its `EdgesFrom` walk also returns callees + imports from the cross-project tree (not just the symbol itself). Pre-fix, when the requested ID didn't exist in the session project but DID in some other indexed project, `context` returned a coherent-looking response built entirely from the wrong project — symbol source, callee source, import paths — with only a warning string. Now: rich-error with three `next_steps` actions (re-issue with `project=<found-in>`, `search` in session, or opt back into legacy with `cross_project=true`). The `cross_project=true` opt-in flag preserves the legacy warning shape. `project="*"` and explicit `project=<name>` continue to bypass the strict guard. Follow-up will close the third arm (`neighborhood`) (#1232 context arm).
- **Breaking + closes #1232 entirely:** `neighborhood` no longer silently returns cross-project data. Third and final arm of the #1232 strict-error flip (after symbol in PR #1241 and context in PR #1242). Neighborhood is the most dangerous of the three: it returns up to 500 in-file siblings, every one of them belonging to the cross-project tree if the seed resolved off-session. Pre-fix, an agent planning an in-file refactor (the canonical neighborhood use case) would plan against the wrong file entirely. The error message specifically calls out the in-file refactor hazard — that's why neighborhood's strict guard matters more than symbol's. New `cross_project=true` flag opts back into the legacy silent-fallback-with-warning shape; `project="*"` and explicit `project=<name>` continue to bypass the guard. With this arm landing, #1232 is fully resolved — all three handlers (symbol/context/neighborhood) default to strict-error on silent-cross-project (#1232 neighborhood arm).
- **Breaking:** `symbol` no longer silently returns cross-project data. Pre-fix, when the requested ID didn't exist in the session project but DID exist in some other indexed project (sniffer mirrors, MCP_Combine staging, .pincher-supported snapshots), the handler returned that other project's row with only a warning string in `_meta.warnings`. Agents that don't parse warnings consumed cross-project data with no programmatic signal — same silent-confidently-wrong family as #935 (corpus="all" redirect) and #1217 (cypher column-vs-column dropped). Now the handler returns a rich-error envelope naming the project where the symbol actually lives, with three `next_steps` actions: (1) re-issue with `project=<found-in>`, (2) `search` by short name in the session project (which is probably where you meant), (3) opt back into legacy silent-fallback with `cross_project=true`. The legacy warning shape is preserved for opt-in callers. `project="*"` and explicit `project=<name>` bypass the strict guard — they asked for cross-project / specific-project lookup deliberately. Follow-ups will mirror this to `context` and `neighborhood` (#1232).

### Fixed
- **`_meta.capabilities` schema_vN tag is now a runtime probe, not a compiled-in constant** (#1202, v0.66 DOGFOOD round 1). Pre-fix the tag was hardcoded to whatever schema version the binary was built against (`"schema_v27"` post-#1192). When a NEW binary shipped a migration that bumped the schema but an OLDER binary was still running (auto-restart-on-drift hadn't fired yet; or a concurrent process applied migrations while a different binary held the DB), the advertised tag stopped matching reality. A router reading `_meta.capabilities` to decide "do I have schema vN features available" got the wrong answer. Now computed as `fmt.Sprintf("schema_v%d", db.CurrentSchemaVersion())` so the tag always reflects the live migration head. Three contract tests pin the property: positive runtime tag matches CurrentSchemaVersion(), negative exactly-one schema_vN tag in capabilities, cross-check CurrentSchemaVersion() agrees with the live schema_version row. Discovery context: surfaced in v0.66 DOGFOOD round 1 — first health call after the v0.65 ship showed `schema_v26` advertised against `schema_version: 27`.
- **Empty `__init__.py` no longer floods extraction_failures with byte_range_negative noise** (#1203, v0.66 DOGFOOD). The Python AST extractor emits a Module symbol per file. For a zero-byte `__init__.py` (a common Python package-marker convention), the Module symbol lands at `StartByte=0, EndByte=0` — legitimately zero-span, not a bug. The pre-fix invariant `EndByte <= StartByte` caught this as a failure, flooding the failures list with low-signal rows that drowned legitimate failures. Now: Module kind specifically is allowed to have `EndByte == StartByte`; other kinds (Function / Method / Class / etc.) still reject zero-or-negative spans. Genuinely inverted ranges (`EndByte < StartByte`) on ANY kind still fail. Four contract tests pin: positive empty Module emits no failure, negative zero-span Function still fails, control inverted-span Module still fails, cross-check healthy positive-span Module emits no failure. Discovery context: v0.66 DOGFOOD round 1 — doctor returned 60+ extraction_failures, two visible rows were `hermes/.../__init__.py` and `stoa/tooling/tests/__init__.py` byte_range_negative.
- C/C++ extractor no longer over-emits one Symbol per macro call site. `cMacroRE` pre-fix accepted leading whitespace, so `extractCBareMacros`'s full-source scan matched indented `UE_LOG(LogTemp, ...)` invocations inside function bodies as if they were column-0 bare-prefix declarations like `EXPORT_SYMBOL(foo)`. On Unreal Engine corpora that produced 4-11 `LogTemp` Symbols per file and a `qualified_name_collision` extraction failure. Restricted the regex to column 0 — the intended declaration shape — which keeps `EXPORT_SYMBOL` / `MODULE_PARM` / `DEFINE_LOG_CATEGORY` working while dropping call-site false positives. `rewriteCMacroSymbols` (the funcRE-driven `static DEVICE_ATTR(...)` rename path) is unchanged: funcRE is column-0-only so the line it inspects already starts at column 0 (#1204).
- `doctor` latency on multi-project installs: dropped from O(N projects) round-trips to one cross-project SELECT + one COUNT. Pre-fix the handler looped `ListExtractionFailures(p.ID, top)` per project — on a 130-project / 11GB DB with WAL contention the loop alone burned ~60s, making `doctor` an emergency-only tool instead of the cheap diagnostic poll it was meant to be. The fix adds `Store.ListRecentExtractionFailuresAcrossProjects(cutoffUnix, limit)` (single SELECT with `WHERE last_seen_at >= ? ORDER BY last_seen_at DESC LIMIT ?`) and `Store.CountRecentExtractionFailuresAcrossProjects(cutoffUnix)` for the honest `extraction_failures_truncated` tally — both reader-pool routed. Project-name join now happens in-memory from the `plist` already fetched at the top of the handler. The cutoff filter moves from Go to SQL so older-than-lookback rows are excluded at the index, not after a full per-project pull (#1205).
- **Markdown sibling-heading dups no longer flood extraction_failures with qualified_name_collision noise** (#1207, v0.66 DOGFOOD). Sibling headings with identical text are a COMMON shape in real documentation — reference docs with repeated subsection structure, auto-generated index pages, tutorial scaffolds. The goldmark walker correctly emits one Section per heading, and `disambiguateDuplicates` adds `~<line>` suffix so all sections survive in the DB. But the `qualified_name_collision` diagnostic — designed to surface regex-scope blindness in code — fired on every Markdown file with repeated headings, drowning legitimate failures with low-signal Markdown rows. Now skipped for Markdown specifically: every Section symbol still survives via disambiguation, and the diagnostic stays focused on its real audience (regex code extractors with genuine scope issues). Three tests pin: positive Markdown sibling-heading dup emits no failure, negative Python QN collision still emits, cross-check Markdown with no collisions emits no row. Discovery context: v0.66 DOGFOOD round 1 — doctor extraction_failures listed `axolotl_dataset_formats.dataset_formats` ×3 and `integrations.sonarqube` ×2 dominating the recent-failures view.
- TypeScript extractor drops function-overload signatures (`function name(...): T;` declared without body), keeping only the implementation. Pre-fix a typical TS library with 3 overload signatures + 1 implementation produced 4 Function symbols all sharing the same qualified_name → `qualified_name_collision` extraction failure per file. Walks parens + generic args + comments to detect multi-line signatures correctly. Heuristically scoped to declarations using the `function` keyword so arrow consts (`const x = (...) => ...;`) aren't false-dropped by their terminating `;`. As a follow-up, the `qualified_name_collision` diagnostic is now suppressed for TypeScript wholesale — residual collisions (top-level const + same-name object-property arrow, JSX polymorphic variants, re-exports vs locals) are legitimate real-world shapes the regex extractor can't scope-resolve without an AST; disambiguateDuplicates still `~<line>`-suffixes the second occurrence so every symbol remains addressable. Mirrors the #1207 Markdown carve-out. Reconsider when the TS AST extractor (#1177-area) lands (#1208).
- `query` filters `testdata/` / `__fixtures__/` / `test-fixtures/` paths by default — pinchQL audit-shape queries no longer surface pincher's own pinned test corpora (#33) as if they were real source. Pass `include_fixtures=true` to include them. Mirrors the existing `isTestFixturePath` filter that `dead_code` and `architecture` already apply. Filter only triggers when the query projects a `file_path` column; `RETURN n.name`-only queries are pass-through (#1212).
- **isTestFile now recognizes bash test conventions** (#1213, v0.66 DOGFOOD). Pre-fix the test-file detection only matched `_test.go` / `_test.py` / `*.spec.ts` / `*.test.ts` etc. — bash test scripts using the same `_test.sh` suffix convention slipped past, surfacing as production functions in dead_code and architecture hotspots. Pincher-repo itself shipped `scripts/release-channel_test.sh` and `scripts/pr-issue-consistency_test.sh` — both were leaking into pinchQL audit queries until the v0.66 DOGFOOD probe caught it. Added `_test.sh` suffix and `test_*.sh` prefix to the convention list, mirroring Go (`_test.go`) and Python (`_test.py` / `test_*.py`). Two tests pin: positive both bash conventions match; cross-check non-bash conventions still match after the addition.
- **pinchQL: column-vs-column comparison predicate is dropped, not substituted with false** (#1217, v0.66 DOGFOOD round 2). Pre-fix the warning text said "predicate ignored (returns false)" but the runtime actually substituted `false` for the unsupported predicate — turning `WHERE x AND y` into `WHERE x AND false → 0 rows`, silently dropping every row that `x` would have matched. Same silent-confidently-wrong family as v0.59's drain. Surfaced during v0.66 DOGFOOD round 2: a query `WHERE a.language="TS" AND a.file_path <> b.file_path` returned 0 rows; without the second predicate it returned 10 — the substitute-with-false killed the conjunction. Now the unsupported predicate is treated as always-true (no-op), so the surrounding WHERE clauses still apply. Warning text updated: "predicate dropped (treated as always-true)". One new test pins the AND-with-supported case; the original test updated to match the new contract.
- `guide` now routes "dead function" / "dead method" / "dead symbol" tasks to `shapeDeadCode`. Pre-fix the classifier had `"dead code"` (two words) but missed the natural-language `"dead functions"` plural — the most common phrasing for the canonical `dead_code` use case. `guide task="find all dead functions in this project"` recommended `search query="dead"` (BM25 of the literal phrase) instead of pointing at the purpose-built tool. Same family as #768 / #1107 (natural-phrasing gaps in the classifier). The fix uses noun-suffixed substrings (`"dead function"`, `"dead method"`, `"dead symbol"`) so bare `"dead"` in unrelated contexts (`deadline`, `dead-letter`, `dead links`) doesn't false-route (#1230).
- Corpus snapshot CI gate unblocked: `testdata/corpus/python-web.snapshot.json` was missing its trailing newline. `pincher index --json-summary | jq` emits one, so every Linux/macOS CI run regenerated the file with a trailing LF and the diff failed. Drift came in with #1190/#1191 (the python-web fixture commits) and slipped review because Windows runners normalise on read. Single-byte fix (#1239).
## [0.65.0] — 2026-05-16 — Description-honesty audit sweep

v0.65 is a focused description-vs-reality drain across seven tools. Each PR follows the same pattern: identify a place where the tool description undersells, overstates, or misnames its actual response shape; align the description with the runtime; pin both sides with table-from-the-start tests (positive + negative + cross-check). The agent's mental model formed from these descriptions drifted in subtle ways from real behavior — most consequentially, agents using the `fetch` tool followed advice to use `search kind:Document` (FTS5 operator syntax) when the search tool's `kind` argument actually wants `kind="Document"` (JSON arg style). Seven contract tests now block regression across the audited tools. No behavioral changes apart from the `query` cypher-alias warning text + the v1.0 removal anchor (#638 tool-schema-freeze). #1177 (TS receiver-type resolver) + #635 (dashboard panel rendering on the v0.64 substrate) carry forward to v0.66+ as multi-day work.

### Fixed
- **search tool: min_confidence description names the corpus='docs' default branch** (v0.65 description audit). Pre-fix the per-field schema text claimed the default was driven by query shape only (exact-identifier 0.0 vs phrase/wildcard 0.71). `defaultMinConfidenceFor` has had a third branch since at least v0.5: `corpus='docs'` returns 0.0 regardless of query shape — Markdown sections are the intentional docs-corpus target, not noise to filter out. Agents calling `search corpus='docs' query='authentication overview'` saw section symbols and might have read it as "0.71 floor not enforced" when actually it's by design. Three contract+behavior tests pin description-vs-reality across all three branches (positive corpus='docs' acknowledgment, negative query-shape-only framing, control runtime parity across exact/phrase/docs branches).
- **architecture tool description names every response field + every hotspot kind** (v0.65 description audit). Pre-fix the description said hotspots were "functions" — the response actually surfaces Function/Method/Class/Interface/Type/Module via `isHotspotKind`. And the description named four response fields ("language breakdown, entry points, hotspot functions, graph statistics") but the response includes six top-level keys (`project`, `languages`, `entry_points`, `hotspots`, `node_kinds`, `edge_kinds`). Description now names every field by JSON key and every hotspot kind explicitly. Four contract+behavior tests pin: positive every-field naming, negative no-stale "hotspot functions" framing, control description-vs-runtime parity for isHotspotKind, cross-check real response contains every promised key.
- **query tool: cypher alias deprecation framing matches the actual removal plan** (v0.65 description audit). Pre-fix the `cypher` parameter description said "kept for one release" — a commitment introduced in #712 that did not survive the next ten+ releases. Both the per-field description and the per-call deprecation warning now name the actual plan: alias is honored with a deprecation warning, slated for removal in v1.0 per #638's tool-schema-freeze. Three tests pin description-vs-reality: positive `v1.0` mentioned in both pinchql + cypher descriptions, negative no `kept for one release` regression, cross-check the runtime warning emitted matches the description's v1.0 plan.
- **fetch tool: description recommends correct `kind="Document"` arg syntax** (v0.65 description audit). Pre-fix the description told agents to use `search kind:Document` — but that colon syntax is FTS5-operator-style and is NOT supported on the search tool's `kind` argument, which is a plain JSON string parameter (`{"query":"...","kind":"Document"}`). Agents following the description literally would pass `query="kind:Document"` as the FTS5 search string and get bizarre BM25 results with no error path. Three contract tests pin description-vs-reality: positive correct arg syntax recommendation, negative no FTS5-operator-style regression, cross-check Document appears in the search.kind schema vocabulary so the recommendation matches.
- **stats tool description names all three response sections + uptime/latency surfaces** (v0.65 description audit). Pre-fix the description listed four returned items ("tokens used, tokens saved, call count, per-project index size") but the response is actually a text-rendered three-section box (SESSION / ALL-TIME / PROJECT) with seven distinct counters in SESSION alone — including process uptime (#420) and avg latency, plus the bounded-percentage form of tokens saved. Most importantly, agents didn't learn from the description that ALL-TIME (cumulative across reconnects) exists — the headline number for "is pincher actually saving me tokens over the long haul?" Two tests pin: positive description names every section + uptime/latency, cross-check rendered output contains every label the description promises.
- **symbol tool description names rename-resilience capability** (v0.65 description audit). Pre-fix the description named the ID format and recommended context/symbols alternatives, but didn't surface a load-bearing capability — the handler auto-resolves stale IDs via the `symbol_moves` table on file renames. Agents who'd cached an ID across a file rename might have re-issued a `search` by short name, when retrying the original `symbol` call would have worked unchanged. Two tests pin: positive description mentions rename + symbol_moves, cross-check handleSymbol still resolves stale IDs end-to-end via symbol_moves on a recorded move.
- **doctor tool description names every response field + the top=50 ceiling** (v0.65 description audit). Pre-fix the description named five returned items but omitted `binary_version` (top-level, the running pincher binary's version) and `advisories` (array surfacing large-DB sizing hints + ghost-project warnings). It also missed naming the per-project `schema_version_at_index` + `binary_version` drift columns — the actual mechanism behind "per-project staleness". And the `top` field schema said "Default 10" without naming the 50-row hard ceiling, so agents passing top=100 got silently clamped with no advance signal. Three tests pin: positive every field named, negative `top` schema acknowledges the ceiling, cross-check real response contains every promised key.
## [0.64.0] — 2026-05-16 — Dashboard data plumbing + description-honesty audit

v0.64 splits cleanly into two themes. (1) **Data plumbing** for the v0.64+ dashboard triangulating panels (#635): schema v27 lands `session_tool_calls`, a per-call event log persisted on every tool response. Substrate for tool-call entropy, response-payload distribution, and per-tier saved-percentage medians — none of which were computable before because `session_stats` holds only per-session aggregates. The panel rendering itself rolls forward to v0.65. (2) **Description-honesty audit** drains stale claims accumulated across v0.57→v0.63: `dead_code` no longer claims `language=Go` is a default (Python AST shipped in #862); `health` distinguishes three parser tiers AST/Regex/Stub (post-v0.63 the bucketing collapsed real-regex with empty-stub coverage); python-web pinned corpus extends the AST-extraction gate with decorators + inheritance + async. CI repair bundled: v0.63's stub promotions left two test fixtures stale on master (node-monorepo snapshot, profile_test); both are now green. `#1177` (TS receiver-type resolver) rolls forward to v0.65 — multi-day TS AST work.

### Added
- **python-web pinned corpus** added (#1184, v0.64) — exercises FastAPI/Django-shaped Python patterns: decorators on sync AND async `def`, multi-class inheritance (`User(BaseModel)`, `Item(BaseModel)`), dependency-injection-shaped cross-file CALLS chains spanning three files. The existing `python-app` corpus pinned basic Class/Method/Function/IMPORTS extraction; this corpus extends the gate with the shapes that ship in 80% of real Python deployments — decorators and inheritance — both previously unverified end-to-end. `Makefile` `CORPORA` list and `cmd/pinch/snapshot_test.go` `corpora` slice mirror this. Snapshot pins 3 Classes / 7 Functions / 5 Methods / 5 IMPORTS / 6 CALLS across 6 files.
- **Schema v27: session_tool_calls** (#635 v0.64) — per-call event log persisted on every tool response. Substrate for the v0.64 dashboard triangulating panels (entropy, payload distribution, per-tier saved-percentage medians). Columns: session_id / tool / complexity_tier / response_bytes / tokens_used / tokens_saved / tokens_saved_pct / ts / request_id. Three indexes on (session_id), (ts), (tool, ts) — supports the three canonical query shapes the dashboard runs. New `Store.RecordToolCalls` bulk-INSERTs from server's 10s flush; `Store.RecentToolCallsForSession` reads back per-session. Capability tag bumped `schema_v26`→`schema_v27`. Pre-fix dashboards couldn't compute per-call medians at all (session_stats holds only per-session aggregates). Eight new tests (4 db-level, 4 server-level) follow table-from-the-start: positive round-trip / negative empty-noop / control session-isolation / cross-check tier-from-registry + handler-driven append + cap-overflow re-arm + buffer-drain integration + index-existence guard.

### Fixed
- **dead_code tool description** corrected — pre-fix claimed `language=Go` was a default and that Python AST extraction was pending. Both stale: server passes `language=""` (no filter), and Python AST shipped in #862 (v0.57). Description + per-field schema text now name `min_confidence=0.95` as the actual precision lever and acknowledge Python (and JSON/YAML/HCL/TOML parser-backed) at AST-tier 1.0. Four contract tests pin description-vs-reality so the next floor-change fails loudly (#1184-adjacent v0.64 tool-description audit).
- **Master CI snapshot + profile-test legacy from v0.63 stub promotions** repaired. `node-monorepo` snapshot Method count 6→8 (v0.61 TS Method extraction in #1158 added 2 more) plus `edge_count_by_kind` gained `CALLS: 1`. `internal/init/profile_test.go` `TestProfileDir_StubMajority` / `TestProfileDir_Mixed` swapped Scala→Haskell — Scala was promoted to regex-tier in #1187 so the "stub-majority" assertion no longer held; Haskell is the only remaining stub-tier language (indentation-sensitive layout deferred per #1161). Restores CI green on master (was failing Corpus snapshot / Coverage / macos test).
- **health tool: parser identity now distinguishes three tiers (AST / Regex / Stub)** (#635-adjacent v0.64). Pre-fix the per-language coverage label collapsed everything below the AST confidence threshold (0.99) into "Regex", silently bucketing stub-tier extractors (confidence=0.0, empty FileResult on every call) with real regex coverage. Post v0.63 promotions (#1186/#1187) only Haskell remains stub-tier, so the bug surface narrowed but the mislabeling persisted. The switch now branches on the registered confidence into three tiers: ≥0.99 = AST, <0.5 = Stub, else Regex. Description text updated to name all three tiers and call out the v0.63 stub-promotion outcome. Three contract+behavior tests (positive AST/Regex/Stub round-trip, negative no-stale-two-tier-framing, control Python AST-upgrade still fires).
## [0.63.0] — 2026-05-16 — Stub-tier language audit: 6 of 7 stubs promoted to regex-tier

v0.63 theme: audit the 7 stub-tier languages (Scala, Lua, Zig, Elixir, Haskell, Dart, R) and decide promote-or-defer. Outcome: 6 of 7 promoted to regex-tier this release. **Lua / Elixir / Zig** (round 1) and **Scala / Dart / R** (round 2). Haskell is the only stub-tier language remaining — its indentation-sensitive layout with no `{`/`def`/`function` anchor makes regex-tier representation significantly harder; proper extractor deferred to v0.64+ with rationale documented inline. All 6 promoted extractors opt into the v0.62 regex-tier CALLS pass; confidence 0.70. With v0.63 shipped, every detected language emits symbols + same-file CALLS edges except Haskell. AST extractors for Rust/Java + Python real-corpus validation + TS receiver-type resolver carried from v0.62 are tracked under #1177/#1182/#1183/#1184 and roll forward to v0.64+ as multi-day work.

### Added
- ast: Scala / Dart / R promoted from stub-tier to regex-tier (#1161, v0.63 round 2). Following the v0.63 round 1 batch (Lua/Elixir/Zig). With this, Haskell is the only stub-tier language remaining. Decisions per the v0.63 audit:
- **Scala** — `def name`, `class`/`object`/`trait` for scope. Modifier-rich keyword stack handled in funcRE: `private`/`protected`/`override`/`final`/`abstract`/`implicit`/`sealed`. `object` and `case class` both surface as Class scope.
- **Dart** — C-family function shape (`returnType name(args)`). Optional modifiers: `static`/`external`/`abstract`/`async`. Reuses TS's `dropTSKeywordFalsePositives` since Dart inherits JS/TS control-flow vocabulary.
- **R** — `name <- function(...)` and `name = function(...)` assignment-style definition. Accepts dotted names (`helper.fn`) since R conventionally uses `.` in identifiers.
All three opt into the v0.62 regex-tier CALLS pass. Confidence 0.70. Haskell remains stub — its indentation-sensitive layout with no `{`/`def`/`function` anchor makes regex-tier representation significantly harder; a proper extractor is tracked as v0.64+ work.
Tests pin the contract: positive function-extraction for each of the three, Haskell-stays-stub regression guard, and the round-1 Lua/Elixir/Zig tests continue to pass.
- ast: Lua / Elixir / Zig promoted from stub-tier to regex-tier (#1161, v0.63). Pre-fix, all 7 stub-tier languages emitted zero symbols — `IsSourceFile` returned true but the `FileResult` was always empty, leaving the corpus invisible to `search`/`query`/`trace`. v0.63 promotes the three easiest (well-defined function-definition syntax, common in real repos):
- **Lua** — `function name(...)`, `local function name(...)`, `function obj:method(...)`, `function ns.name(...)` patterns. Uses `end`-keyword block closing like Ruby.
- **Zig** — `pub fn name(...)`, `export fn name(...)`, `extern fn name(...)`, plus top-level `const Name = struct` as Class.
- **Elixir** — `def`/`defp`/`defmacro` for functions, `defmodule` for module-as-class scope. Uses `end`-keyword block closing.
All three opt into the v0.62 regex-tier CALLS pass via `extractCalls=true`. Confidence 0.70 (regex-tier). Scala / Haskell / Dart / R remain stub-tier this release — Haskell's indentation-sensitive layout, Scala's mixed-paradigm syntax, and Dart/R requiring more nuanced detection make regex-tier representation significantly harder. Decide-or-defer for those tracked under v0.63 follow-ups.
8 tests pin the contract: function-extraction + per-file-CALLS for each of the three promoted languages (6 tests), a defmodule-emits-Class cross-check, and a stub-tier-stays-stub regression guard for the four still-deferred languages.
## [0.62.0] — 2026-05-16 — Regex-tier CALLS sweep: every regex-tier language emits CALLS edges

Closes the #858 edge-graph-empty warning surface for the regex-tier language set. Pre-v0.62, only Go (AST), Python (AST, #856), C (#858), and TS (#1158 v0.61) emitted CALLS edges — every other regex-tier project's `trace`/`dead_code`/`neighborhood` returned the edge-graph-empty warning. v0.62 lights up Rust, Java, PHP, C#, Kotlin, Swift, and Ruby; each opts into the shared `extractCalls=true` path via `regexCallScan`. Required generalizing `regexCallScan`'s signature-skip from `{`-only to `{`-or-first-newline so Ruby's end-keyword block bodies parse the same as C-family. Same-file calls resolve immediately; cross-file resolution is v0.63+ resolver work — deferred under #1177/#1182/#1183/#1184. Per-language AST extractors for Rust/Java + Python real-corpus validation deferred under v0.63 milestone for the same reason.

### Added
- ast/php+csharp+kotlin+swift: per-file CALLS pass enabled (#1159, v0.62). Extends the v0.62 regex-tier CALLS sweep to four more languages — same shape as TS #1158, Rust+Java #1159 piece 1, and C #858. All four use `{` block bodies so the shared `regexCallScan` path works as-is; same-file calls resolve, cross-file resolution is the v0.62+ per-language resolver task. Confidence pinned at 0.6 (regex-tier). With this round, every regex-tier language except Ruby (which uses `end`-keyword block closing instead of `{`) now emits CALLS edges — closing the #858 edge-graph-empty warning surface for the seven highest-coverage regex-tier languages. 4 positive tests confirming each language emits expected CALLS edges from method bodies.
- ast/ruby: per-file CALLS pass enabled (#1159, v0.62). Closes the regex-tier-CALLS sweep — every regex-tier language now emits CALLS edges, ending the #858 edge-graph-empty warning surface for the regex-tier set. Required generalizing `regexCallScan`'s signature-skip from "find `{`" to "find `{` or first newline" so Ruby's `def ... end` block bodies work the same as the C-family brace bodies. Confidence pinned at 0.6 (regex-tier). Known regex-tier limitation: Ruby idiom often elides parens on method calls (`render` vs `render()`), and regexCallRE requires `(` after the name, so paren-less call sites are invisible. Documented in the test comments. Tests: positive (paren-bearing call resolves), control (paren-less body emits zero CALLS — pins documented limitation), cross-check (regexCallScan newline-fallback doesn't emit the function's own name as a CALLS target).
- ast/rust+java: per-file CALLS pass enabled (#1159, v0.62). Parallel to TS #1158 and C #858. Pre-fix, neither Rust nor Java emitted CALLS edges — every project's edge graph was empty (`trace`/`dead_code`/`neighborhood` returned the #858 edge-graph-empty warning). Both extractors now opt into `extractCalls=true` via the shared `regexCallScan`. Same-file calls resolve immediately; cross-file resolution is the v0.62+ resolver task. Control-flow keywords filtered by the existing `regexCallKeywords` blocklist. Confidence pinned at 0.6 (regex-tier). 6 tests covering positive (3-target body emits 3 edges), control (empty body emits zero CALLS), cross-check on confidence, and Java control-flow keyword filtering.
## [0.61.0] — 2026-05-16 — Phase 2 entry: TypeScript foundation (Method symbols, per-file CALLS, polymorphic blocklist)

First dev release of Phase 2. Lays the TypeScript foundation for the v0.62 regex → AST promotion + receiver-type resolver work. TS class methods now extract as Method symbols with `Parent` = enclosing class (foundational for `X.method` resolution). Every TS Function/Method body emits CALLS edges via the shared `regexCallScan`, so the edge graph is no longer empty for TS projects. The polymorphic-method-name blocklist (#465 Go fix) is generalized into a per-language map with TS + Python entries pre-populated for the v0.62+ resolvers that will consume it. Closes #1158 (v0.61 entry scope); the receiver-type resolver itself rolls to #1177 in v0.62 where the TS AST extractor work lands.

### Added
- resolver: polymorphic-method-name blocklist generalized to per-language map (#1158, v0.61). Pre-v0.61, `isPolymorphicInterfaceMethodName` was a hardcoded Go-only switch covering `String`/`Error`/`Read`/`Lock`/`ServeHTTP`/etc. — the names that overwhelmingly resolve to stdlib interfaces and produced the #465 dogfood false-positives (every `.String()` binding to a single project-local Method). v0.61 generalizes to `polymorphicMethodNamesByLanguage` map plus a new `isPolymorphicMethodName(name, language)` dispatch. Adds TS entries (Object.prototype universals like `toString`/`valueOf`/`hasOwnProperty`, Promise `then`/`catch`/`finally`, Iterator `next`/`return`/`throw`, Map/Set/Array methods, EventTarget, lifecycle hooks) and Python entries (dunders like `__init__`/`__str__`/`__getitem__`/`__call__`/`__enter__`, common protocol methods). The TS/Python entries are foundational — when v0.62+ TS/Python receiver-type resolvers land, they consume this same list to avoid the equivalent of #465's dogfood false-positives. Legacy `isPolymorphicInterfaceMethodName` preserved as a thin Go-dispatching wrapper for the existing Go resolver call sites (indexer.go:2278, :2687) — no behavioral change for Go. Six tests pin the contract: Go set preserved, TS entries present, Python entries present, unknown language doesn't filter (fail open), languages isolated (TS-only names don't leak into Go), legacy wrapper byte-parity with the dispatch.
- ast/typescript: per-file CALLS pass enabled, parallel to C #858 (#1158, v0.61). TS extractor now opts into the shared `extractCalls=true` path via `extractOpts`, emitting CALLS edges from every Function/Method body's call sites. Pre-fix the TS edge graph was always empty — `trace`/`dead_code`/`neighborhood` returned the #858 edge-graph-empty warning on every TS project. Same-file calls resolve immediately; cross-file calls drop until the v0.61 receiver-type resolver piece lands. Control-flow keywords are filtered by the shared `regexCallKeywords` blocklist (`if`/`for`/`while`/`switch`/`return`/`throw`/`new`/`delete`/`typeof`/...). Confidence pinned at 0.6 (regex-tier). Tests: positive (3-target body emits 3 edges), negative (control-flow keywords excluded), control (empty body emits zero CALLS), cross-check on confidence + class-method-body extraction (foundational for the resolver piece).
- ast/typescript: class methods now extract as Method symbols with `Parent` = enclosing class (#1158, v0.61 entry). Pre-fix, the TS regex extractor produced Class symbols but their methods were invisible — `class Cart { add(item): void { ... } }` emitted only `Class:Cart`, never `Method:Cart.add`. Adds `methodRE` to `tsRE` matching `name(` after optional `public`/`private`/`protected`/`readonly`/`static`/`async` modifiers + `*` generators, scoped by the existing `currentClass` tracker in `regexExtractor.extract`. New `dropTSKeywordFalsePositives` filter strips false-positive captures of `if`/`for`/`while`/`return`/`throw`/`try`/`catch`/`switch`/`case`/`default`/`break`/`continue`/`do`/`else`/`finally`/`typeof`/`instanceof`/`new`/`delete`/`void`/`yield`/`await` that the regex shape accidentally matches on control-flow statements inside class bodies. Foundational for the rest of the v0.61 TS receiver-type stack — without Method symbols there is nothing for the resolver to bind `X.method` calls to in later releases. Tests: positive (all 5 methods of a representative `Cart` class extract), parent-points-at-class-QN, keyword-filter drops control flow, free functions still emit as Function, and a direct unit test for `dropTSKeywordFalsePositives`.
## [0.60.0] — 2026-05-16 — Phase 1 stable promotion: silent-confidently-wrong family drained, drift correctness, encoder consistency

The Phase 1 closeout release. Bundles the v0.59 hardening drain (17 PRs landing pinchQL warning emitters, edge-graph-empty surfacing across trace/dead_code/neighborhood/context, CI gates) with the v0.60 correctness blockers (atomic drift-reindex stamping, binary-version downgrade race, C keyword extractor false positives, vacuum WAL-reader advisory, encoder consistency across all `_meta`-bearing surfaces). Per the channel/stability discipline (#638), v0.60 is the stable-promotion target — every named correctness regression on master is now shipped, the 41-shape known-good pinchQL safety net guards future warning emitters, and capability advertisement is runtime-probed in CI.

### Added
- ci: gate PR-title (#N) ↔ body `Closes #M` consistency (#1103). PR titles use the conventional-commit suffix `(#N)` which GitHub does NOT auto-interpret as a close reference; bodies use `Closes #M`. When N ≠ M the wrong issue gets closed — observed twice in 24h (#1075 + #1094 closed by unrelated PRs #1092 / #1095). New `pr-issue-consistency` job runs on every pull_request event, extracts the title's trailing `(#N)` and every body Close/Fix/Resolve `#M`, and fails CI when they disagree. Skipped when title has no suffix or body has no close-ref (nothing to verify). Mid-issue numeric references (e.g. "Companion to #1093" in body without the close keyword) are ignored — only the close-keyword anchors are validated. Self-test in `scripts/pr-issue-consistency_test.sh` covers 11 cases including the two real-world #1103 bug shapes.
- test(cypher): known-good query suite as a safety net for future warning emitters (#1121). New `TestExecute_KnownGoodSuite_NoWarnings` table-driven test runs 30 real-world pinchQL shapes (node scans, single-hop joins, BFS variable hops, all supported WHERE operators including IS NULL / IS NOT NULL / NOT prefix / AND/OR / parens, DISTINCT, ORDER BY incl. COUNT(*), LIMIT, COUNT/SUM/AVG/MIN/MAX, GROUP BY, inline pattern props, all supported edge directions) against a seeded in-memory DB and asserts each produces zero warnings. v0.59 added eight new warning emitters in rapid succession (#1108/#1109/#1115/#1116/#1117/#1118/#1119/#1120); this gate makes false-positive regressions in any future warning addition fail loudly instead of silently leaking into user output. Deliberately-rejected shapes (`IN` operator, inbound `<-` arrow, undirected `-[r]-` edges) are excluded — those have curated error messages and aren't "known good."
- test(cypher): expand known-good safety net suite with v0.59 fix territories (#1141). 11 new shapes added to `TestExecute_KnownGoodSuite_NoWarnings` covering the canonical real-world queries exercised by v0.59's silent-wrong fixes (#1123/#1127/#1129/#1133/#1136/#1139/#1140): aliased RETURN + aliased ORDER BY, aliased RETURN + source-prop ORDER BY, grouped aggregate with ORDER BY COUNT(*), DISTINCT + grouped aggregate, MIN/MAX integer aggregates, edge-confidence predicates, valid-kind aggregate, bare bound-variable return, IS NULL / IS NOT NULL with aggregates, nested NOT(OR) in WHERE. Suite now 41 shapes; new emitters must keep all 41 silent or the gate fails.
- neighborhood: surfaces the #858 "edge graph is empty for this language" warning when the project's dominant language has no cross-file edge resolution (#1145). trace and dead_code already flag this via edgeCoverageGap; neighborhood didn't, so a TypeScript / C / Rust user calling neighborhood saw same-file siblings only and had no signal that's because the graph layer is missing for the language, not because the symbol is a leaf. The warning names the dominant language and clarifies that the file-scope list is the complete structural view available — there is no graph-traversal layer to expand into. Refactor: split `edgeCoverageGap` into the existing trace/dead_code-specific message + a new `edgeGraphEmptyForLanguage` probe that returns the language + boolean, letting neighborhood (and future tools) format their own honesty message.
- context: surfaces the #858 edge-graph-empty warning when callees + imports are both empty AND the dominant language has no cross-file edge resolution (#1146). Pre-fix, a TypeScript / C / Rust user calling context on a function got `callees=[]` + `imports=[]` and read it as "this function calls nothing / imports nothing" — when the resolver never ran for the language. Same shape as #1145 (neighborhood); the warning fires only when both arrays are empty so a Go function with real callees gets the genuinely-informative empty case (no callees here, but the resolver ran) without false-positive warnings.
- http: GET /v1/projects gained MCP-parity filter query params — `?active`, `?active_within_days`, `?include_dead`, `?min_edges` (#707). Pre-fix, the HTTP endpoint dumped every row in the store (including dead-on-disk paths and worktree fan-out from concurrent agent runs), while the MCP `list` tool filtered the same data to a clean orientation view. New `filterProjects` helper in internal/server/project_filter.go is the single source of truth for the "drop dead-path / drop inactive / drop low-edges" logic and is now used by both the MCP and HTTP paths. Defaults intentionally stay unfiltered so the dashboard's loadProjects / populateSearchProjects / ADR dropdown calls (which rely on every row being present) keep working; the filters are opt-in via query params. Response shape gains `filtered_out` and `filtered_breakdown` (always present, even on unfiltered requests, for shape stability) so dashboard or agent code can show what got hidden and which knob would recover it. Pagination (#530) preserved verbatim — filter runs before windowing.
- test(index): binary-drift parity regression guard (#986). Adds `TestIndex_BinaryDriftParity_MultiFile` — indexes a 60-file × 10-symbol Go corpus (past the 500-symbol flushBuffers threshold), then runs a drift-triggered reindex (binary version bumped, force=true, mirroring `maybeReindexOnDrift`) followed by an immediate explicit `force=true` reindex. Asserts both produce identical symbol + edge counts. The user-reported 30% symbol gap on a 501-file corpus does NOT reproduce at this scale — confirming the gap is either v0.57.x-era latent (since closed by intervening work) or scale/concurrency-dependent beyond unit-test reach. The test pins the parity contract so any future regression at the drift-reindex code path fails CI rather than reaching the dogfood loop silently. Investigation findings on #986 (disproved resolver-gate hypothesis, remaining suspects) are documented in the issue comment.

### Changed
- mcp: tool responses default to compact JSON (#1089). Pre-fix, `jsonResultWithMeta` used `json.MarshalIndent(data, "", "  ")` — two-space indented. The withRequestID middleware (#657) already re-encoded responses compact after injecting `_meta.request_id`, so the production wire format was effectively compact; this PR aligns the underlying handler so unit tests and the wire format match. `PINCHER_DEBUG_META=1` preserves pretty-printing for human-eyeballing raw MCP traffic both in the handler and in the middleware re-encode path. ~10-15% byte reduction on representative shapes when the env-flag isn't set (per the issue body's measurements).
- init tool description now documents the codex skipped_always_global behavior (#1100). Followup to #1075 — the description still said "Returns per-target {target, path, action, diff_preview, bytes_in, bytes_out}" with no mention of the codex skip-entry shape. Agents reading the description didn't know codex would return a different envelope.

### Fixed
- doctor ghost-project advisory now flags low edges/symbols ratio (<0.001), not just zero edges (#1010). The pre-existing strict gate missed real ghosts like warp_rc (1.4M syms / 247 edges, ratio 0.000175) that leak a few edges before the resolver phase dies. Two orders of magnitude below the worst observed healthy ratio.
- Go reads/writes extractor now descends into function-literal bodies (#1062). Pre-fix, references inside `go func(){...}()`, `defer func(){...}()`, callback args, and assigned closures produced no READS or WRITES edges — `dead_code` false-flagged consts like `sessionFlushFast` that are only used inside spawned goroutines. The walker now hands `FuncLit.Body` off to the statement walker so AssignStmt/IncDecStmt fire correctly inside the closure.
- search project-not-found now uses the rich error envelope (#1063). Pre-fix, handleSearch was the only per-project tool that returned a bare `{"error":"..."}` with no `_meta.next_steps` — every other per-project tool (architecture, query, symbol, dead_code, changes, etc.) routes through mustProject and gets the canonical `list` + `index` recovery affordance. Agents consuming _meta.next_steps silently lost the structured recovery only on search.
- changes project-not-found now uses the rich error envelope (#1064). Companion to #1063 search fix — these were the two per-project tools that returned a bare `{"error":"..."}` with no `_meta.next_steps`, while every other per-project tool (architecture / query / symbol / dead_code / trace / schema / neighborhood) returned the canonical `list` + `index` recovery affordance via mustProject. Family closed.
- search now rejects unknown corpus values upfront with a rich envelope (#1065). Pre-fix, `corpus="cdoe"` (typo) fell through to the DB layer's `corpusVtab`, which returned a bare `unknown corpus "cdoe" (valid: ...)` error wrapped in handleSearch's catch-all `search error: ...` string. No `_meta.next_steps`. Same silent-confidently-wrong family as #1063/#1064. Now: explicit validation surfaces the three valid corpora (code/config/docs) as candidate calls naming the user's query.
- symbols batch now surfaces a top-level not-found summary (#1066). Pre-fix, missing IDs returned `{"id": "...", "error": "not found"}` stubs buried inside the `symbols` array, while top-level `count` lumped found+not-found together. Agents reading top-level fields concluded all IDs resolved. Now: `not_found_ids` array, `count_found` / `count_not_found` split, and a `_meta.warnings` entry naming the misses with a `search`-by-name recovery hint. Same failure-as-pedagogy shape as the bare-error sweep (#1063/#1064/#1065).
- architecture now surfaces a ratio-class ghost-extraction warning (#1067). Companion to #1010 (doctor ratio extension). The strict #1040 ghost diagnosis fired only when both hotspots and entry-points were empty AND edges == 0 — fools-gold-pirate at 11181 symbols / 9 edges (ratio 0.0008) produced 6 hotspots from one Python file, so the strict gate skipped it. Now: a ratio-class warning fires regardless of hotspots existing, threshold matches the doctor advisory (0.001 — two orders of magnitude below the lowest observed healthy ratio).
- schema now surfaces a ratio-class ghost-extraction warning (#1068). Completes the ratio-class extension trio (with #1010 doctor and #1067 architecture). Pre-fix, schema's strict diagnosis fired only on edgeCount == 0 — projects that leak a handful of edges through a failing resolver slipped past. Same 0.001 threshold so the ghost signature is consistent across the three orient-first tools.
- dead_code now surfaces a ratio-class ghost-extraction warning (#1071). Fourth in the ratio-class series (with #1010 doctor, #1067 architecture, #1068 schema). When the project has a handful of edges but most symbols have no inbound edge, dead_code's candidates are disproportionately FPs from the un-resolved bulk of the corpus. Same 0.001 floor so the ghost signature is consistent across the full graph-tool surface.
- query now surfaces a ratio-class ghost-extraction warning (#1073). Fifth in the ratio-class series (#1010 doctor, #1067 architecture, #1068 schema, #1071 dead_code) — closes the ghost signature across the full graph-tool surface. When query returns rows on a ratio-ghost project, those rows reflect only the small resolved subset; agents need to know the bulk of the corpus has no edges so they don't generalize the partial result to the whole project.
- `pincher web` --timeout flag now references the `webBackgroundReadyTimeout` constant instead of hardcoding 8 seconds (#1074). The constant existed with a clear doc comment but was never wired up — pincher's own dead_code detector flagged it (the FuncLit READS fix in #1062 surfaced enough graph signal to make this visible). Dogfood loop closes: pincher found its own dead constant.
- init now surfaces always-global targets as `action: skipped_always_global` instead of silently dropping them (#1075). Pre-fix, `target=codex` returned `results: []` with no signal; `target=all` quietly omitted codex (and continue) — the user got no refusal, no explanation, no recovery affordance. Now each filtered target appears with a reason pointing at the `pincher init --target=<name>` CLI fallback. Same silent-confidently-wrong family as #1063/#1064/#1065.
- UpsertProjectMeta now re-stamps legacy pre-v18 projects on re-index (closes #1086). Pre-fix, projects with `schema_version_at_index = NULL` (last indexed before the v14→v15 migration) couldn't get the column populated even after `index force=true`. SQLite NULL propagation through `MAX(NULL, 26)` returned NULL, and `excluded.X >= NULL` evaluated to NULL (false in CASE WHEN) — both arms of the monotonic guard failed silently, leaving `binary_version` empty and the drift warning firing permanently. Fix: `COALESCE(schema_version_at_index, 0)` in both expressions, treating legacy NULL rows as below any current schema version.
- dead_code with a bad language filter now diagnoses the filter, not min_confidence (#1093). Pre-fix, `language="BogusLang"` (typo or not-indexed-here) fell into the generic `no dead code at min_confidence ≥ N — lower min_confidence` branch — confidently-wrong advice that wouldn't help. Now: a filter-specific diagnosis names the bad language, lists the available ones, and points at the recovery (drop the filter / rerun on most-symbol-rich language).
- dead_code with an all-unknown kinds filter now returns 0 results with a kind-filter diagnosis (#1094). Pre-fix, `kinds="BogusKind"` had ALL unknown values dropped during validation (per the #851 "drop unknown, keep going" policy), so SQL ran with no kind filter and returned dead symbols from every kind — contradicting the caller's intent. The warning in `_meta.warnings` named the bad value, but the primary response shape said "here are 5 dead symbols across all kinds." Now: the all-unknown case rides the bad value through to SQL (0 rows) and emits a filter-specific diagnosis. Mixed lists (one valid, one bogus) keep the existing drop-bad-keep-good behavior.
- trace with an all-unknown kinds filter now rejects with a rich envelope (#1096). Companion to #1094 dead_code fix. Pre-fix, `trace kinds="BogusKind"` warned about the unknown value then proceeded with the kinds list empty — SQL ran without an edge-kind filter and traced ALL edge kinds. The warning surfaced but the result contradicted it. Mixed lists (one valid + one bogus) keep the existing drop-bad-keep-good behavior.
- docs/REFERENCE.md drained 15 schema versions + 6 missing tools of drift (closes #1101). The schema version was stuck on v11 (binary on v26); the "22 MCP tools" section enumerated only 16 (missing dead_code, neighborhood, init, doctor, rebuild_fts, self_test — all restored to MCP in v0.52 #624 reversal); the search corpus enum still listed the `all` value removed in v0.5; the extraction-confidence table had Python in the 0.85 regex tier (moved to 1.0 AST in #856) and missed HTML / Ruby tuning. Migration history table extended to v26 with one-line summaries per migration. Same drift class that prompted #698 / #999 ("12 schema versions stale" → release-prep checklist).
- query now warns when min_confidence is a no-op because the RETURN clause doesn't project extraction_confidence (#1103). Pre-fix, `query pinchql='MATCH (n) RETURN n.name' min_confidence=0.9` silently passed every row through unfiltered — the caller thought they were filtering by confidence and got an unfiltered result. The doc-documented contract is preserved (rows still pass through), but the warning now names the no-op and points at the fix (add `n.extraction_confidence` to RETURN). Same silent-confidently-wrong family as #1094 / #1096. Suppressed on empty-result queries to avoid redundant noise with the empty-result advisory.
- fetch now returns a rich error envelope on non-2xx HTTP responses (#1106). Pre-fix, a 404 / 401 / 429 / 5xx response was reported as bare text (`server returned HTTP 404 for ...`) with no `_meta`, no next_steps, no diagnosis — the agent had to guess whether to retry, switch URL, or pivot. Now each status class carries a tailored hint: 401/403 routes the agent to `search corpus=docs` (fetch is unauthenticated, doc may already be indexed), 404 prompts a corrected URL, 429 carries a retry-after hint, 5xx flags transience. Same fix shape as #1063 / #1064 (project-not-found rich envelopes).
- guide now routes idiomatic "no one calls" / "nobody calls" tasks to dead_code (#1107). Pre-fix, `guide task="find functions in this repo that no one calls"` routed to `shapeUnknown` → search+context with a single-word discriminator ("one") plucked from the task, totally unrelated to dead_code. Same family as #768 (gap between "no" and "callers"). Now: dead_code is the first recommendation for both forms.
- query now warns when MIN/MAX/SUM/AVG targets a text or bool column (#1108). Pre-fix, `RETURN MAX(n.name)` silently returned null — `computeAgg` parses each row's value as float64 and skips non-numeric ones, so an all-text column yields `nums=[]` → nil. SQLite's MAX/MIN actually work lexicographically on text, so pincher's behavior diverges silently; the agent reads `MAX(n.name): null` as "no rows match" when the real cause is the aggregator/column-type mismatch. Same silent-confidently-wrong family as #889 (WHERE type mismatch). The warning names the aggregator + property + suggests COUNT or a numeric column.
- query now warns when a variable-length hop range with min=0 is silently coerced to min=1 (#1109). Pre-fix, `MATCH (a)-[:CALLS*0..3]->(b)` silently dropped the length-0 path (the seed itself, per Cypher semantics) — pincher's BFS only emits length≥1 hops, so `min=0` got clamped without signal. Agent read the result as seed-inclusive when it was actually seed-exclusive. The new warning surfaces the coercion and points at the workaround (separate MATCH for the seed, or `symbol`/`context` direct). Same silent-confidently-wrong family as #869 (inverted hop range).
- search now rejects leading `^` and trailing `$` regex anchors with a friendly error (#1110). Pre-fix, `search query="^handle"` returned zero results with the generic "lower min_confidence" diagnosis — the FTS5 sanitizer silently stripped the anchor and ran a partial-match search that returned BM25-low matches, so the empty-result branch fired the wrong remediation. The new pre-flight names the anchor and redirects to the `query` tool's `=~` operator. Same regex-leak family as #509 (`.*`, `.+`, `.?`) and #788 (slash-delimited regex literal).
- changes clean-scope diagnosis no longer doubles "what what's" in its next_steps (#1113). Pre-fix, the caller composed `fmt.Sprintf("try scope=%q to see what %s captures", other, scopeDescription(other))` and `scopeDescription("staged")` returned `"what's already added via git add ..."` — the composition read "try scope="staged" to see what what's already added ... captures", with a doubled "what what's" that was jarring on the eye. New format reads as a clean compound: `try scope="staged" — what's already added via git add (pre-commit blast radius)`.
- guide audit-shape classifier now matches natural-language phrasings with up to 8 intervening words between the noun and the absence verb (#1114). Pre-fix, `auditShapePattern`'s lazy `w+( w+){0,3}?` quantifier bottomed out at 4 trailing words, too tight for natural phrasings like "find handlers in this codebase that don't have a test" (5 intervening words) — these fell through to shapeUnknown / shapeFind and the agent got search+context recommendations instead of pinchQL audit-query recommendations. Bumped to {0,8}: covers natural prose without over-catching, because the absence-word alternation is the load-bearing audit signal — without it the regex can't match regardless of intervening-word slack.
- cypher: undirected-edge error now points at a workaround that actually works (#1115). Pre-fix, the error text suggested `<-[r:KIND]-` as the "inbound" remediation — but the parser rejected that exact form with the same error, so agents trying the suggested form got the same error in a loop. New text describes the only supported form (`-[r:KIND]->`) and the variable-swap workaround for inbound traversal: write `MATCH (caller)-[r:KIND]->(target) WHERE target.name=...` instead of attempting an inbound arrow.
- query warns when WHERE/RETURN reference a variable not bound in any MATCH pattern (#1116). Pre-fix, `MATCH (n:Function) WHERE m.name="x" RETURN n.name` silently coerced `m.name` to NULL/always-false — the predicate was effectively ignored and rows passed through unfiltered. Agents reading the result thought their filter applied; the query was actually unfiltered. New warning names the unknown variable + the bound-variable list ("Bound variables in this query: a, b, n. (typo? did you mean one of those?)"). Same silent-confidently-wrong family as #473 (unknown property), but at the variable scope.
- query now rejects Cypher write keywords (CREATE/DELETE/SET/MERGE/REMOVE/DETACH/DROP/FOREACH) with a "pinchQL is read-only" message (#1117). Pre-fix these got the generic "unexpected token — expected a clause keyword (WHERE, RETURN, ORDER BY, LIMIT) at this position" error, which reads as a syntax bug the agent can fix — the real story is pinchQL is read-only by design and the only way to mutate the graph is to re-extract via `index force=true`. Naming the contract up-front short-circuits the wrong fix attempt.
- query rejects comma-separated patterns in MATCH with a coverage-gap message (#1118). Pre-fix, `MATCH (a:Function), (b:Function) WHERE ...` (Cypher's syntax for joining independent patterns) hit the generic "unexpected token ','" error pointing at WHERE/RETURN — which reads as a syntax bug to fix. The real story is pinchQL supports one pattern per MATCH (and one MATCH per query, #871). New error names the gap + points at workarounds: two separate query calls for independent matches, or the edge form `MATCH (a)-[:CALLS]->(b)` for joined matches.
- query: unknown inline-brace props now evaluate to always-false per the warning contract (#1119). Pre-fix, `MATCH (n:Function {nme: "main"}) RETURN n.name` silently dropped the inline filter — the query returned all-Function rows, contradicting the warning text that promises "treated as undefined (always false in comparisons)". The two `cypherPropToCol` skip-branches in `appendInlinePropFilters` (SQL path) and `matchesInlineProps` (BFS path) silently swallowed unknown predicates. Now both inject an always-false guard so the result matches the contract: 0 rows. Warning still surfaces so the typo is teachable.
- query ORDER BY COUNT(*) now actually sorts (#1120). Pre-fix, the ORDER BY parser dropped the asterisk inside COUNT(*) because the tokenizer reads `*` as an empty HOPS token (same `*`-as-HOPS shape as #946 in parseReturn). `q.orderBy` got set to `"COUNT()"` while the projection column was `"COUNT(*)"`, so the grouped-row sort looked up `grouped[i]["COUNT()"]`, found nothing, and returned rows in scan order — agents reading a `... GROUP BY language ORDER BY COUNT(*) DESC` result thought the data was sorted by frequency but it wasn't. Now the parser preserves `*` literally so the ORDER-BY key matches the projection column name.
- query: ORDER BY aggregate without a matching projection aggregate now warns (#1122). Pre-fix, `MATCH (n:Function) RETURN n.language ORDER BY COUNT(*) DESC` returned ungrouped rows in scan order with zero signal — the user reads "languages by frequency" while pincher returns "every Function row's language in scan order." Without a projection aggregate there is no grouping context, so `COUNT(*)` collapses to one value across the whole match set, the sort has nothing to order, and the result is silently scan-ordered. Companion to #1120 (asterisk-as-HOPS projection-key mismatch); collectUnknownOrderByWarnings explicitly skipped aggregate targets, so this structurally-adjacent case had no detector before now.
- query: repeated pattern variable now enforces self-loop semantics (#1124). Pre-fix, `MATCH (a:Function)-[:CALLS]->(a) RETURN a.name` returned every CALLS edge in the graph because runJoinQuery aliased the from-end as `a` and the to-end as `b` and joined both independently — repeating the variable name added no binding constraint. Standard Cypher binds the variable once; pinchQL now matches by injecting `AND a.id = b.id` when `fromVar == toVar` on a single-hop pattern, plus a warning so the row-count change is teachable. The canonical "find functions that call themselves" idiom now actually returns self-loops. Variable-length self-loops (`*1..N`) for cycle detection are out of scope — BFS does not natively support cycles.
- query: ORDER BY <alias> now returns the global top-N, not the top-N of a pre-truncated scan window (#1126). Pre-fix, `MATCH (n:Function) RETURN n.name, n.complexity AS cx ORDER BY cx DESC LIMIT 5` returned five small-complexity rows (27/24/20/18/16) while the source-named equivalent `ORDER BY n.complexity` returned the actual top-5 (98/79/59/49/45). orderByCol / joinOrderByCol stripped the var prefix, looked up "cx" in the property whitelist, got "", skipped the SQL ORDER BY pushdown. The safety scan-LIMIT then clamped to an arbitrary natural-order window; buildResult sorted that window. Same #847 family that bit the non-aliased path before that fix. New `resolveOrderByAlias` maps the alias back to its source `var.prop` form before pushdown so the SQL ORDER BY uses the underlying column.
- trace: directional synonyms (`incoming`/`outgoing`, `in`/`out`, `up`/`down`, `reverse`/`forward`, singular `caller`/`callee`) now map to their canonical direction instead of silently falling back to `both` (#1128). #839 fixed this for `callers`/`callees` but missed the rest of the obvious AI-agent reaches — semantically identical synonyms behaved differently, with `direction="incoming"` returning a mixed inbound + outbound set under a warning that didn't teach "you meant inbound." All synonyms now share one switch with a single interpolated warning that names the canonical direction.
- query: LIMIT now rejects non-integer values with a LIMIT-aware error instead of silently returning zero rows (#1130). Pre-fix, `MATCH (n:Function) RETURN n.name LIMIT 1.5` returned `{rows: [], total: 0}` with no warning — the LIMIT parser ran `strconv.Atoi("1.5")`, swallowed the error via `_`, defaulted `q.limit` to 0, and the engine then interpreted that as "explicit zero rows." Same silent-confidently-wrong family as #1120 / #1124. `LIMIT -1` produced a generic "unexpected token '-'" error with no LIMIT-aware guidance (relevant because some SQL dialects use `LIMIT -1` for unlimited). The parser now explicitly rejects float literals, negative LIMIT, and non-NUMBER junk with an error that names LIMIT, echoes the bad value, and points to the integer alternative (or `LIMIT 0` to suppress rows).
- query: unknown-enum-value warning now lists edge-kind vocabulary for edge-bound WHERE conditions (#1132). Pre-fix, `MATCH (a:Function)-[r:CALLS]->(b) WHERE r.kind = "READS"` returned 0 rows under a warning that said "matched no symbols. Known kind values in this project: Block, Class, Function, Method…" — symbol kinds listed under an edge query, with the remediation "did you mean name = READS?" pointing at a symbol-name fix that has no relevance to an edge-kind value. The probe walked the WHERE conditions without checking whether the variable was bound to an edge in the MATCH pattern. New scope-aware probe checks `pat.edgeVar` and routes edge-bound conditions to a `SELECT DISTINCT kind FROM edges` query so the warning lists edge kinds (CALLS/READS/WRITES/IMPORTS/REFERENCES) instead of symbol kinds. Node-side #501 path preserved.
- query: `RETURN <bare-property>` now surfaces a missing-prefix warning instead of silently projecting `{name: {}}` (#1135). Pre-fix, `MATCH (n:Function) RETURN name LIMIT 1` returned `{name: {}}` — an empty object under a column named `name`, which looks like data with all values empty. The parser stored `variable="name", property=""`, treated it as a bare variable reference (legitimate for `RETURN n` whole-node form), and buildResult projected the unbound variable as an empty map. Same silent-confidently-wrong family as #1116 (unbound WHERE variable), worse because the column header itself looks like a real attribute. New `collectReturnBarePropertyWarnings` detects bare RETURN names that match a known property name + aren't bound, and surfaces the missing prefix with a direct remediation (`RETURN n.name` when one bound var; the bound-var list when multiple).
- query: function calls in WHERE now get a pinchQL-aware hint (#1137). Pre-fix, `WHERE size(n.name) > 5` (and its cousins `length(...)` / `toUpper(...)` / `COUNT(DISTINCT ...)`) returned the generic "cypher parse: unsupported operator: (" with no signal that "function calls in WHERE" was the structural reason. Same dead-end-without-pedagogy shape that #928 fixed for arithmetic operators. The `(` operator hint now points users to the supported scope (aggregators live in RETURN only) and the canonical workaround (project the underlying property and post-process client-side).
- query: typo'd MATCH kind now warns even in aggregate queries (#1139). Pre-fix, `MATCH (n:Funtion) RETURN COUNT(*)` returned `{COUNT(*): 0}` silently while the corresponding non-aggregate `MATCH (n:Funtion) RETURN n.name` did warn — same typo, same data, different behavior depending on whether RETURN aggregated. The Total==0 outer gate skipped the pattern-label probe for aggregate queries because COUNT/SUM/AVG always produce one row regardless of match count. Now: pattern-label typos run via a dedicated unconditional helper (`collectKindLabelTypoWarnings`), and the WHERE-value probe gates on `isEffectivelyZero` (recognises aggregate-zero outcomes too). Aggregate-zero queries with a typo'd label or filter value get the same teach-the-typo warning as the non-aggregate equivalent.
- ast/c: drop reserved-keyword false-positive symbols before the collision pass (#1148). Pre-fix, the C regex `(?:w+s+)+names*(` occasionally captured C reserved words like `sizeof`, `struct`, `typeof`, `offsetof` as Function names when they appeared inside expressions like `size_t n = sizeof(struct foo)`. Multiple occurrences in one file collided on the qualified name and `pincher doctor` reported `qualified_name_collision`. The Lenovo APQ8053 kernel corpus repro shows 25+ such rows. Adds `dropCKeywordFalsePositives` between `dropCForwardDecls` and `extractCBareMacros` — exact-name match against a tight blocklist of 18 reserved keywords that can never legally be C function names. Lookalikes (`sizeof_thing`, `struct_init`) and non-Function symbols (Class with `struct` name) pass through unchanged. Re-file of #1067 — the original closing PR (#1069) was an unrelated ratio-class warning that didn't touch C extraction.
- cli: `pincher vacuum` surfaces a targeted advisory when an open WAL reader pins freelist pages (#1149). Pre-fix, the most common time to run `pincher vacuum` is right after a `project rm` of a large project — i.e. while an MCP child holds an open reader, which silently makes VACUUM reclaim 0 B. Users read the "0 B reclaimed" output as "vacuum is a no-op" and miss that a `/mcp` reconnect would actually free the pages. Adds a probing `wal_checkpoint(TRUNCATE)` that captures `busy=1` and routes it through a new `VacuumResult.WalReaderBusy` return value. When set and zero bytes reclaimed, the CLI prints "another pincher process holds an open reader — WAL freelist pages are pinned and VACUUM cannot reclaim them. Restart the MCP server (or run after active sessions disconnect), then retry." JSON output gains `wal_reader_busy` + `advisory` fields. Re-file of #1068 — the original closing PR (#1070) was an unrelated ratio-class warning that never touched `cmd/pinch/vacuum.go`.
- docs+cli: `make install` no longer exits 1 on a fresh clone, and CLAUDE.md documents the prerequisites (#1151). Pre-fix, `scripts/swap-active-binary.sh` defaulted `--target` to the first `pincher` resolved via PATH; with no on-PATH binary (the in-repo dogfood case), the script exited 1 and `make install` failed. CLAUDE.md called this the "autonomous dogfood path — no manual /mcp" without documenting that (a) an on-PATH binary must exist for the swap, or (b) `pincher supervised` / `PINCHER_AUTO_RESTART_ON_DRIFT=1` is required for the auto-pickup claim. Two changes: (1) script now treats the no-PATH case as a graceful no-op with an advisory pointing to the one-time bootstrap, (2) CLAUDE.md's `make install` snippet now spells out both prerequisites inline.
- mcp: `PINCHER_DEBUG_META=1` now applies consistently across success bodies, error bodies, and the `withRequestID` middleware re-encode (#1152). Pre-fix `errResultRich` always emitted compact JSON regardless of the env flag, and three separate sites duplicated the same `os.Getenv` check. Centralized through a single `marshalMetaJSON` helper; new four-case table-driven test exercises every encode site plus an env-unset control.
- db: binary_version downgrade rejected across concurrent same-schema pincher processes (#1154). Pre-fix, the schema-version monotonic guard (#724) only fired when schema versions differed; same-schema dev builds (`0.58.0-44-g91e9c0f` vs `0.58.0-10-gdeb797d`) raced and the older orphan's watcher kept stamping its `binary_version` back over the newer process's write, locking `health.index_drift` permanently true. Adds a Go-side semver+commit-count comparator (`compareBinaryVersion`) that reads the existing row before write and skips the version field when the incoming value is older. Path/name/indexed_at still update — only the version-specific field is clamped. `dev` sentinel never displaces a release stamp. Table-driven comparator test covers MAJOR/MINOR/PATCH ordering, commit-count tiebreak, leading-v stripping, dev sentinel, and unparseable fallback.
- pinchQL: surface implicit GROUP BY when RETURN mixes an aliased aggregate with a bare column (#1155). `count(a) AS total, a.name` aliases "total" as the answer, but pinchQL applies implicit GROUP BY on the bare column (#348/#432) so each row's count is per-group (typically 1), not the overall total the alias names. Same silent-confidently-wrong family as #1122 / #1135. Narrowly scoped: bare aggregate without alias (`RETURN n.language, COUNT(*)`) plus DISTINCT-grouped and ORDER-BY-on-aggregate shapes stay silent — those are the canonical group-by idioms. Caught a false-positive in the 41-shape known-good suite (`count_group_by`) on first iteration; final design only warns when the alias signals "I expected one number."
- stats: empty-result pinchQL queries credit a small baseline for the avoided grep+empty-read cycle (#1157). Pre-fix, a query that legitimately returned zero rows reported `tokens_saved: 0` even though the agent had run a tool call that genuinely answered "this doesn't exist anywhere" — the alternative was `grep -r <name>` plus reading the empty result (~200 tokens of stdout / shell exit + interpretation overhead). Adds a named constant `emptyResultBaselineTokens=200`; applied only when row count is 0 and no file_path column was harvested. Audit-shape queries that legitimately answer zero rows now show non-zero savings instead of looking like wasted calls in stats. Final v0.59 follow-up from #1090 close.
- pincher web now warns when the running HTTP server's version differs from the on-disk binary (closes #706). Pre-fix, the common dev loop "rebuild → run pincher web → dogfood" silently returned the URL of a stale prior-session server; the dashboard reflected the running (older) code, not the binary just built. Live example from the filed issue: on-disk `0.54.0-10-g63103dc-dirty`, running server `0.22.0-2-g04b9715` — 23 releases behind, no signal. Fix probes `/v1/health` for the running server's `version`, compares to the on-disk binary's stamped version, and emits a stderr banner naming both versions + the PID to kill if the mismatch is unintentional. Best-effort: probe failures suppress the warning rather than failing the flow.
- index: drift-reindex now atomic — interrupted passes leave the project re-startable instead of locking partial state under the new binary stamp (#986). Pre-fix, the start-of-pass `UpsertProjectMeta` wrote the running `idx.binaryVersion` BEFORE walking files. Any interruption (process kill, MCP child restart, supervisor respawn, crash mid-pass) left the project row claiming the new binary version while the symbols table was partial. The next startup then saw `prev.BinaryVersion == idx.binaryVersion`, detected no drift, never retried — leaving ~30% symbol coverage stuck on the new version stamp. Fix: start-of-pass stamp now writes the PRIOR `binary_version` (or `""` for a new project); the end-of-pass `UpsertProject` flips it to the running version only on successful completion. An interrupted pass therefore preserves drift detection for the retry. Three new tests pin the contract: success-stamps-new-version, mid-pass-stamps-prior, interrupted-pass-leaves-prior. Existing `TestIndex_BinaryDrift_ForcesReindex` (#936) gate confirms no regression to the successful-completion path.
- index: files inside fixture paths (`testdata/`, `__fixtures__/`, `fixtures/`, `test-fixtures/`, `test_fixtures/`) are now skipped at index time instead of being extracted (#996). Pre-fix, external projects with large JSON test fixtures (warp-fork: 4360 files, 1.45M symbols, 247 edges; symbol-to-edge ratio of 5,886:1) inflated the symbols table because the indexer extracted every JSON object as a symbol. `isFixturePath` already existed (#750) but was only used by resolution (`preferNonFixtureSyms`) to avoid binding edges INTO fixtures — it was never wired into the per-file gate at index time, so the bytes still got read, hashed, and extracted. The fix adds the check in the indexer's per-file loop, after `ast.ShouldSkip` and before `IsSourceFile`, using the project-relative path (the same form `isFixturePath` already expects). Pinned-corpus snapshots are unaffected: when a corpus is indexed as its OWN project root, the relative path does not contain `testdata/` and the check returns false (the heuristic checks the relative path, not the absolute one — comment at line 1782 confirms).
## [0.58.0] — 2026-05-15

**Theme: failure-as-pedagogy at the project boundary.** Every retrieval-shape tool (`symbol` / `symbols` / `context` / `neighborhood` / `trace` / `search`) now surfaces when an unscoped lookup crosses into a different project than the session — closing the silent-cross-project-leak class where mirror projects (sniffer mirrors, MCP_Combine staging, `.pincher-supported` snapshots) silently returned source bytes from the wrong tree. The `project="*"` sentinel is now handled consistently across all 13 project-arg tools: silent fallback on retrieval + aggregate tools, clear-rejection error on per-project tools. `doctor` gets a ghost-project advisory + a lower `top` ceiling (50, vs the prior 500 that still blew the MCP token cap). `dead_code` / `architecture` / `schema` / `query` close the ghost-extraction diagnosis family; `changes` gets an empty-state diagnosis. Python AST extraction picks up its first pinned end-to-end snapshot corpus; YAML + Markdown gain edge-case pin-down tests (code-block isolation, setext headings, merge keys).

### Added
- `doctor` now emits a `ghost-project` advisory when any project has ≥1000 symbols and zero edges — the zelosMCP-#815 ghost-extraction signature (symbols extracted but resolver phase silently produced no graph). Pre-fix the same totals appeared as healthy numbers in `doctor`'s project list; ghost projects answered `search` happily then returned silent zero-row `trace`/`query`. Caps at the worst 3 by symbol count so the advisory stays scannable.
- Python AST extraction + YAML/Markdown edge cases now have end-to-end snapshot coverage. Added: pinned `python-app` corpus (5 files / 16 symbols / 6 edges) exercising Class + Method + Function + Module + AsyncFunctionDef + cross-file IMPORTS + CALLS; goldmark heading-in-code-block isolation test (pin against regex-tier regression if the parser ever swaps); setext-heading (`===` / `---`) extraction test; YAML merge-key (`<<: *anchor`) static-extraction test. Closes the test-depth gap for the three AST-extracted non-Go corpora before v0.58 release.
- New `pincher init --target=gemini` writes the pincher usage policy to `GEMINI.md` at the project root (or `~/.gemini/GEMINI.md` with `--global`). Same convention as Claude Code's CLAUDE.md — Gemini CLI loads project rules from GEMINI.md by the same pattern. `--target=detect` picks Gemini when `GEMINI.md` file or `.gemini/` directory is present at the project root. Second leg of the #658 wave-1 cross-platform parity work (zed shipped separately; warp follows).
- `pincher init --target=vscode` writes `.github/copilot-instructions.md` at the repo root — GitHub Copilot's documented project-rules file, picked up by VS Code Copilot Chat, JetBrains Copilot, Codespaces, and GitHub.com. Same plain-markdown shape as the other rules-file targets, so the shared `MergePolicyBlock` writer handles marker-block idempotency. Detection fires on the rules file, `.vscode/`, or `.github/instructions/`. Promoted from wave 2 to wave 1.5 because VS Code's user share makes it a first-class citizen, not a deferral. (#658)
- `pincher init --target=warp` writes a project-scoped Warp Agent rules file at `./WARP.md` (or `~/.warp/WARP.md` with `--global`), same plain-markdown shape as `--target=claude` and `--target=gemini`. Detection fires on either `WARP.md` or `.warp/` at the project root. Last leg of #658 wave-1 init parity. (#658)
- New `pincher init --target=zed` writes the pincher usage policy to `.rules` at the project root (or `~/.config/zed/.rules` with `--global`). Uses the shared marker-block convention so re-runs replace in place and unmanaged content is preserved. `--target=detect` auto-picks Zed when either `.zed/` directory or `.rules` file is present at the project root. First leg of the #658 wave-1 cross-platform parity work; warp + gemini follow in separate PRs.
- Every tool OpenAPI endpoint now stamps `x-pincher-idempotent: true|false`. Router-shape consumers (zelos, bifrost, detour-shape gateways) retry failed calls; without an explicit declaration they had to assume "not idempotent" conservatively and skip retries. Stamped at the tool level, classified statically — read-only tools (search/symbol/symbols/context/trace/query/guide/changes/fetch/architecture/dead_code/neighborhood/list/health/stats/schema/doctor/self_test) declare true; writers (index/rebuild_fts/init/adr) declare false. New capability tag `idempotency_declared` in `_meta.capabilities` so consumers know the declaration is machine-readable. Gate test `TestToolIdempotency_EveryToolClassified` blocks any future tool that lands without a classification — same enforcement shape as the existing `toolComplexityTiers` gate (#650).
- New `GET /v1/ready` HTTP endpoint — k8s-style readiness probe distinct from `/v1/health` (liveness). Returns 200 when the server can serve traffic; 503 with structured `reasons[]` when an essential dependency (store, indexer, schema migration) isn't ready. Same public-probe treatment as `/v1/health` and `/v1/openapi.json` (no `--http-key` bearer required so orchestrators can probe without config gymnastics, #588). Documented in OpenAPI spec with both 200/503 response schemas; manifests/Helm charts can wire `livenessProbe → /v1/health` + `readinessProbe → /v1/ready` for proper restart-vs-traffic-gating semantics.
- Helm chart prototype under `packaging/helm/pincher` for single-tenant Kubernetes deployment — Deployment + Service + PVC + ServiceAccount, liveness probe to `/v1/health` and readiness to `/v1/ready` (#660), `strategy: Recreate` to honor the single-writer SQLite invariant (#51), optional bearer-token auth via Secret reference, optional streamable-HTTP MCP transport (#651), optional supervised auto-restart-on-drift (#352). See `docs/deployment/helm.md` for the install guide. (#661)
- `pincher init --target=vscode-mcp` writes `.vscode/mcp.json` at the project root, registering pincher as an MCP server inside VS Code Copilot Chat's tool surface. Distinct from `--target=vscode` (#970), which writes the project-rules file at `.github/copilot-instructions.md`: that target tells Copilot HOW to behave; this one tells it WHERE to find pincher's tools. Running both gives Copilot the rules + the runtime — closing the loop for the VS Code user-base push. Uses the supervised wrapper (auto-restart on disconnect) and a per-target `PINCHER_DATA_DIR` so VS Code's pincher DB stays isolated from Codex/Claude Code instances. Preserves user-added entries in `mcp.json.servers.*`; refuses to write on malformed-JSON existing files rather than guess.
- README hero placement and tutorial for VS Code Copilot Chat. The supported-editors list, quickstart `pincher init` examples, Client configuration `<details>` blocks, and tutorials index now all surface VS Code alongside Claude Code / Cursor / Codex / Zed. Pairs with #970 (vscode rules target) and #989 (vscode-mcp MCP-registration target) to make VS Code a first-class citizen on the README — the largest editor user-base shouldn't be discovered four screens down under "Continue, Windsurf, Aider follow the same pattern."

### Changed
- `pinchQL` arithmetic-operator error now teaches the workaround instead of dead-ending. Pre-fix `MATCH (n:Function) WHERE n.end_line - n.start_line > 100 ...` errored with `unsupported operator: -` — no path forward, no link to the open issue. The line-count audit template (#921) emits exactly this shape, so the failure had a high-traffic blast radius. Now: `arithmetic operators (+, -, *, /) are not yet supported in pinchQL (tracked in #928). For line-count audits use RETURN n.start_line, n.end_line and compute the diff client-side; for fan-in ratios, run two queries and divide outside the engine.` Engine support for actual arithmetic still lives behind #928. Note: `*` and `/` hit different tokenizer paths (HOPS variable-length match, regex delimiter respectively) and don't reach operatorHint cleanly — the hint covers the high-traffic `-` and `+` cases that #921's template uses.

### Fixed
- `resolveProjectID` falls back to canonicalizing absolute-path inputs via `db.ProjectIDFromPath` when the exact-string match against stored IDs misses. Closes #997. On case-insensitive filesystems (Windows NTFS, macOS APFS) a user passing the same project path with different casing (`D:ClaudeCodeX` vs `d:claudecodeX`) previously got `project not found` despite the underlying directory being byte-identical — `GetProject` does string-exact match and the stored ID and the input were case-different. Now the resolver canonicalizes any absolute-path input and retries; non-path inputs (project names like `pincher-repo`) still flow through the existing name-match path unchanged.
- `taskHintFromString` drops auxiliary-verb and negation tokens (`have`, `has`, `had`, `having`, `no`, `not`, `without`) when building the hint phrase. Pre-fix the task "find symbols that have no test coverage" extracted **"have no"** as the discriminator (longest non-stopword run between breaks), and the templated search recommendation searched for the literal phrase — which never resolves to anything code-shaped. Same family as the #933 call-family-verb strip and the #615 visibility-noun strip: stopword shape is "this word is never the subject of a software-engineering task." Failure-as-pedagogy throughline.
- `docs/REFERENCE.md` Roadmap section pruned of multi-month-stale chronology (v0.2 ✅ / v0.3 ✅ / v0.4 🚧 / v0.5 listed without status — when v0.57 was the active release). The README is now the single source of truth for release themes + status; REFERENCE.md keeps only the v1.0 ship criteria and the out-of-scope refusals. Same staleness shape as #998/#999 (drift between documented surface and active reality) — every parallel chronology rots, so collapse to one source.
- PR template's `Release alignment` section no longer lists stale milestones (`v0.4.0` / `v0.5.0`). The actual current milestone is v0.58; contributors opening PRs saw a checklist that hadn't moved with the project. Replaced the hardcoded version list with a "current minor (default)" / "patch / v1.0" framing plus a pointer to the live milestones page.
- HTML extractor no longer produces `byte_range_negative` Sections on documents with duplicate `(level, title)` headings. Pre-fix `bytesFindHeading` scanned from offset 0 every call, so two h2 "Phase 2: Configuration" sections under different parents both resolved to the first occurrence's byte offset. The section-emit loop then computed `end_byte = start_byte = N` for the first duplicate, and `recordExtractionHeuristics` caught these as extraction failures on real HTML docs (atrium/website, hermes/docs were both surfacing the failures). Caught during a `doctor lookback_hours=1` probe — the failure reason was already reported but the root cause wasn't filed. `bytesFindHeadingAfter` advances the search bound past each previously-located heading so each one resolves to its own offset.
- XML extractor no longer emits zero-byte-range `Setting` symbols for attributes whose byte range can't be located in the source. Pre-fix, when `xmlFindAttrRange` returned `(0, 0)` (typical for namespaced 1-char attrs like `xmlns:s` whose stripped local name `s` fails the preceding-char-is-separator check), the caller fell back to `(n.startByte, n.startByte)` — an empty byte range that `recordExtractionHeuristics` correctly caught as `byte_range_negative` on real XSD files (Microsoft Office Open XML schemas like `pml.xsd`, `wml-2010.xsd` showed this in `doctor`). Now the attribute symbol is skipped when its range can't be located; the element itself still emits a Setting. Same family as #1004 (HTML duplicate-headings byte_range_negative).
- `_meta.index_in_progress` warning now says "starting (walking files)" when `files_total` is 0 instead of the misleading "mid-pass (0/0 files)". Caught probing my own #994 fix this session: when `GetProgress` reports `active=true` before the gocodewalker has seeded `FilesTotal`, the warning text quoted `0/0` files — agents reading the numbers would conclude the index is hung. Completes the phase coverage: walk → extract → resolve all have distinct wording. Same fix applied to both `jsonResultWithMeta` (success path) and `errResultRich` (error path).
- `mustProject` now returns an `errResultRich` envelope on "project not found" with `list` + `index` next_steps. Pre-fix, every per-project tool (architecture, schema, query, trace, dead_code, neighborhood, changes, search) shared a bare `errResult("project ... not found")` — agents had no inline recovery path. Same failure-as-pedagogy throughline as #982 (changes git-diff) / #984 (schema empty-state) / #987 (cypher errors). One shared-helper upgrade fans out across ~10 tools.
- `handleContext` now returns an `errResultRich` envelope on "symbol not found" with `search` (by short name) + `list` next_steps. Pre-fix, `context` shared `handleSymbol`'s pre-#704 bare envelope — `handleSymbol` got the rich-envelope upgrade, `context` was the sibling that didn't. Same failure-as-pedagogy gap as #984 (schema empty) / #987 (cypher errors) / #1007 (mustProject not found): agents on a stale ID had no inline recovery path.
- `docs/REFERENCE.md` listed `--max-file-size-mb` default as 512; actual default has been 4 (4 MB) since #111. The 128× discrepancy matters: a user reading the doc decides "files under 512 MB are indexed" and is surprised when an 18 MB JSON gets a `file_too_large` failure. Source of truth (`internal/index/indexer.go` `DefaultMaxFileSize`) wins; doc gets corrected.
- `guide` now classifies "find every X that doesn't <verb>" / "don't <verb>" as `shapeAudit` (was `shapeTest`/`shapeFix`/`shapeFind`). Pre-fix, `auditShapePattern` only matched the literal `doesn't have` / `does not have` absence form. Tasks like "find every test that doesn't run in parallel" or "find every handler that doesn't return an error" are structurally identical audits but used a different verb after the negation; they fell through to BM25 search of the literal phrase. Same family as #992 (article quantifier optional) and #924 (untested adjective).
- `handleNeighborhood` now clamps `limit` to 500 (matching `search`'s #532 ceiling) with a `_meta.warnings` clamp message. Pre-fix the handler clamped only the low end (`limit <= 0 → 50`); a caller passing `limit=99999` got "every symbol in the file" — on big files (200+ symbols) the response blew the MCP per-call token cap and the agent saw a truncation error with no recovery path. Same shape as the other tools' clamp warnings.
- `doctor` now surfaces clamp warnings when `lookback_hours` or `top` are explicitly passed as <=0 (silently coerced to 168/10 pre-fix). Same silent-clamp pattern as #879 (search) and #1013 (neighborhood). A caller who passes `lookback_hours=0` (perhaps thinking "include all history") used to see `lookback_hours: 168` in the response and not know the value was rewritten. Omitted-param calls still pass through cleanly without a clamp warning — the caller accepted the default explicitly.
- `doctor` now clamps `top` to 500 (matching search's #532 ceiling) with a `_meta.warnings` clamp message. Pre-fix, `top` had no upper bound: a caller passing `top=99999` on a multi-project install produced a 506 KB response that blew the MCP per-call token cap — agent saw a truncation error with no recovery path. Same shape as search (#879), neighborhood (#1013), and doctor's own lookback_hours clamp (#1015).
- Stale tool-count claims swept to 22: `.claude-plugin/marketplace.json` (the description Claude Code's plugin marketplace surfaces to users still said "15 MCP tools"), `RELEASING.md` tool-contract section (named only 15 tools when listing the pinned-contract set, missed seven), and `cmd/pinch/main.go`'s "all 15 tools" comment in main(). Same staleness-sweep theme as #998 / #999 — every user-visible tool-count claim should track the actual surface. The comment in main.go rewritten to point at `registerTools` as the authoritative source so it stops drifting.
- `handleInit` previously hard-rejected `target=continue` (always-global, escapes project_path from MCP context) but its `unknown --target` error still listed `continue` in the "one of: ..." valid-targets enumeration — pulled from `pinit.ResolveTargets`'s CLI-perspective list. An agent reading the error, then trying `target=continue`, immediately hit the hard-reject. Contract drift inside one tool. Now: `continue` is stripped from the enumeration in MCP context so the error is truthful — every named target actually works.
- `adr action=delete` now returns a `key not found` rich envelope when no row was deleted, instead of confidently reporting `deleted: true` for a no-op. Pre-fix, `DeleteADR` discarded the SQL `RowsAffected` count and the handler emitted success on a typo'd key or wrong-project-scope call. Same silent-confidently-wrong family as #984 / #987 / #1008. Success path now includes the `rows` count so callers can verify what was actually deleted.
- `list` no longer drops input-clamp warnings when the result is paginated or empty. Pre-fix, the next-page hint and empty-state diagnosis blocks each assigned `data["_meta"] = map[string]any{...}` — overwriting the `listClampWarnings` block that ran just above. Probe `list active_within_days=-5 limit=2 include_dead=true`: the "active_within_days=-5 ignored — using default 14" clamp warning disappeared from the response. Now: both paths merge into existing `_meta` so warnings survive next-step and empty-state augmentation.
- `neighborhood`'s pagination next-page hint no longer clobbers earlier `_meta` warnings (staleness + clamp). Same shape of bug as #1020 in `list` — `data["_meta"] = map[string]any{"next_steps": ...}` replaced rather than merged. `attachStalenessWarning` runs at line 246 of `neighborhood.go`, then line 252 wiped its output before the response left the handler. Fix: merge `next_steps` into existing `_meta`, never overwrite. Audited the rest of the codebase for the same pattern — every other `data["_meta"] = map[string]any{...}` site is a first-time setter where no prior warnings can be lost.
- `search` now wraps raw FTS5 syntax errors in a rich envelope with three recovery next_steps: retry with the trailing boolean stripped, wrap the whole query in quotes, or hand off to `guide`. Pre-fix, a malformed boolean query (`foo AND`, `OR bar`) leaked SQLite's `fts5: syntax error near` directly — opaque to the caller, no recovery path. The sanitizer catches dotted identifiers (#289) and unmatched quotes (#489) but can't repair an incomplete boolean; this fix closes the remaining gap. Adds `stripTrailingBoolean` helper for the programmatic-recover next_step.
- `health` now surfaces a `_meta.warnings` entry when the caller passes a `project` name that doesn't resolve, instead of silently degrading to the minimal `{schema_version, db_path}` global-view envelope. Pre-fix the caller had no signal — typo, not-yet-indexed, and "name=ID instead of name=Name" all looked identical. Also fixes an instance of the #1020/#1021 `_meta` clobber pattern in the same handler: the next-step block reassigned `data["_meta"] = map[string]any{...}` and wiped the warnings the new block had attached. Both bugs land together because surfacing the project-resolve warning is what made the clobber observable.
- `stats` now honors the documented `project` parameter. Pre-fix the InputSchema declared `project` as a valid arg ("Project to include in index size breakdown. Defaults to session project.") but the handler never read it — passing `project=foo` silently returned the session project's stats. Contract drift inside one tool: the schema promised a feature the code didn't implement. Now: caller-supplied `project` is resolved via `resolveProjectID`; an unresolvable value falls back to the session project AND inlines a warning so the override failure is visible. Same family as #1018 (init `continue` enumeration drift) and #1023 (health silent fallback).
- `neighborhood` now warns when the `project` arg doesn't resolve, instead of silently falling back to a global symbol lookup. Pre-fix, a typo'd project name with a valid id returned siblings from whatever project happened to own the id — the caller had no signal the scope override failed. Same silent-fallback shape as #1023 (health) and #1024 (stats). `handleSymbol` has the same pattern but ships separately to keep the PR atomic; filed for follow-up.
- `symbol` now warns when the `project` arg doesn't resolve, instead of silently falling back to unscoped symbol lookup. Pre-fix, a typo'd project name with a valid id returned the symbol from whatever project owned it — caller passed a scope hint and got data outside their requested scope. Same silent-fallback shape as #1023 (health) / #1024 (stats) / #1025 (neighborhood). Filed as the follow-up #1025 promised.
- `symbols` (batch) now warns when the `project` arg doesn't resolve, instead of silently falling back to unscoped batch lookup. Closes the silent-fallback family: #1023 (health) / #1024 (stats) / #1025 (neighborhood) / #1026 (symbol) / #1027 (this PR — symbols batch). Every per-project tool that took an explicit project arg now surfaces resolution failures.
- `guide` now warns when the `project` arg doesn't resolve, instead of silently ignoring it. The schema declared the param ("Project name or ID. Defaults to session project.") but the handler ignored it entirely — a typo'd project name returned recommendations as if the caller hadn't scoped at all. Same contract-drift shape as #1024 (stats). Closes the silent-fallback family across every per-project tool that takes a documented `project` arg: #1023 (health) / #1024 (stats) / #1025 (neighborhood) / #1026 (symbol) / #1027 (symbols batch) / #1028 (this PR — guide).
- `clampMinConfidence` now clamps negative values to 0.0 with a warning. Pre-fix only the upper bound (`v > 1.0`) was clamped; negative inputs silently passed through. Downstream `> 0` gates in search/query/trace/dead_code mean a negative value behaves like the 0.0 default — but the caller never learned they passed an invalid value. Same "documented [0.0, 1.0] contract violated, no signal" shape as #875 (the upper-bound case the original clamp closed).
- `search`'s `fields=` projection now strips empty entries and warns on unknown field names. Pre-fix `fields=",,"` produced per-row `{"": null}` (confidently-wrong artifact, no signal), and `fields=id,bogus_field` silently emitted `{"bogus_field": null}` with no warning. Now: parseFieldsArg drops empty entries, unknown fields are removed from the projection with a warning, and an all-bogus projection falls back to the full response with a louder warning. Same pattern `symbol`/`context` already use via projectAndCheckFields.
- `context` with `lite=true` now warns on unknown `fields` entries instead of silently dropping them. Pre-fix the lite branch used `projectFields` (silent drop) rather than `projectAndCheckFields` (drop + warn). `context lite=true fields=bogus_field` returned an empty body with no signal the projection was bogus — same silent-confidently-wrong shape as #1030 (search fields). Now: typo'd fields trip a warning naming the unknown keys, an all-bogus projection falls back to the full lite body so the call stays useful.
- `trace name=` resolution now fetches up to 50 candidates and lets sortTraceCandidates pick the best. Pre-fix `GetSymbolsByName` was called with `LIMIT 5` and no SQL `ORDER BY` — for names where the project has >5 matches AND the Module/Setting rows happened to land first in SQL row order (Go projects with `package main` declare a Module per file), all 5 returned rows were Module-kind. sortTraceCandidates had no Function to pick, the trace resolved to a Module which has no CALLS edges, and the result looked like "this symbol is a leaf." The alternatives list surfaced in `_meta.ambiguous_match` is still capped at 5 — only the candidate fetch grew.
- `search` with `offset` past the end of the result set now surfaces a pagination-overshoot diagnosis instead of the generic "no exact-term matches" advice. Pre-fix a query with real matches but `offset >= total` returned `count: 0` with a diagnosis that contradicted the `total > 0` field in the same response. Agents reading the diagnosis concluded the symbol didn't exist; the confidently-wrong text won. Now: when `total > 0` and the offset overshoots, the diagnosis names the actual cause and the next_step suggests retrying at offset=0.
- `list` with `offset` past the end of the filtered result set now surfaces a pagination-overshoot diagnosis. Pre-fix the response carried `count: 18, page.returned: 0` with no signal that the empty rows were caused by an offset overshoot — agents couldn't tell whether the filter matched nothing or they'd paged past the end. Same shape as #1033 for search. The empty-store path (`total==0`) is unchanged.
- `neighborhood` with `offset` past the end of the neighbor list now surfaces a pagination-overshoot diagnosis. Pre-fix the offset was silently clamped to `totalNeighbors` and the response carried `count: 224, page.returned: 0` with no signal the offset overshot — agents couldn't distinguish "file has neighbors but I paged past them" from a degenerate seed. Same shape as #1033 (search) / #1034 (list). The pagination-overshoot diagnosis family is now closed across all three list-shaped tools.
- `trace` now warns when both `id` and `name` are passed instead of silently honoring `id` per the documented precedence. Pre-fix an agent that included both (e.g. by templating from a search result with the id AND passing the short name in parallel) couldn't tell which one was honored — the trace returned what felt like name-resolution but was actually id-resolution. Same silent-precedence shape as the silent-fallback / silent-fields families closed earlier in v0.58.
- `symbol` "not found" errors now surface a stacked project-resolve failure when the caller's `project` arg ALSO didn't resolve. Pre-fix the error only mentioned the symbol miss; an agent who'd typo'd both args (e.g. wrong project + bogus id) only learned about the id mistake, then hit the same project-resolve failure on their next call. Now both failures are surfaced in one error message so the caller can fix both in one round-trip.
- `neighborhood` "not found" errors now stack the project-resolve failure when the caller's `project` arg also didn't resolve. Pre-fix the error only mentioned the symbol miss; an agent who'd typo'd both args only learned about the id mistake. Companion to #1037 (symbol) — same stacked-failure shape across both handlers.
- `context` now warns when the `project` arg doesn't resolve. The schema declared the param ("Project name or ID. Defaults to session project.") but the handler ignored it entirely — a typo'd project name silently fell through to the unscoped symbol lookup. Same contract-drift family as #1024 (stats) / #1028 (guide). When the symbol is also missing, both failures stack into the error message per the #1037/#1038 pattern.
- `architecture` now diagnoses ghost-extraction projects correctly instead of confidently claiming "config/docs-only (no Functions)" for a project with hundreds of Functions. Pre-fix the "truly empty" branch (no hotspots + no entry points + symCount>0) defaulted to the docs/config diagnosis even when the langs histogram in the same response showed Go/TypeScript/etc. — a direct self-contradiction. Now: code-corpus language present + `edge_count==0` routes to a ghost-extraction diagnosis (#815 family) with `index force=true` + `doctor` as next steps.
- `list` now clamps negative `min_edges` to 0 with a warning. Pre-fix negative values were accepted silently — the downstream `if minEdges > 0` made them behave like 0, but the documented contract is non-negative and limit/offset/active_within_days already clamp with warnings. Last list-arg clamp to bring up to parity.
- `schema` now surfaces a ghost-extraction diagnosis when a project has 100+ symbols including callable kinds (Function/Method/Class/Interface) but ZERO edges. Pre-fix schema silently returned bare counts — agents seeing `{node_kinds:{Function: 327, ...}, edges: 0}` could mistake the project for config/docs-only or miss the resolver failure entirely. Same family as #1040 (architecture) / #815 (doctor advisory). Threshold of 100 symbols avoids tripping on small projects that legitimately have no edges.
- `query` (pinchQL) now surfaces a ghost-extraction diagnosis when an empty result lands on a project with 100+ symbols but ZERO edges. Pre-fix the empty result was indistinguishable from a true empty match — every edge-traversal query against a ghost project will look the same, and the agent had no way to tell. Companion to #1040 (architecture) and #1042 (schema) — closes the ghost-extraction diagnosis family across all three read-shaped tools that can return empty on a ghost project.
- `dead_code` now surfaces a ghost-extraction diagnosis when the scoped project has 100+ symbols and ZERO edges, instead of confidently returning a list of false-positive "dead" symbols. Pre-fix, every function looked unreferenced on a ghost project (no inbound edges anywhere) and the populated `dead_symbols` list guided the caller toward deletion of categorically-false-positive results. Closes the ghost-extraction diagnosis family across `architecture` (#1040) / `schema` (#1042) / `query` (#1043) / `dead_code` (#1044, this PR), alongside the existing #1009 `doctor` advisory.
- `dead_code` now warns when `language=` filters to a language with zero symbols in the project, surfacing the actual cause of the empty result instead of the misleading "lower min_confidence" hint. Mirrors the existing `kinds=` validation (#851) and `search`'s language-mismatch diagnosis — every empty `dead_code` response now tells the caller what's wrong, not just that the result is empty.
- `resolveProjectID`'s name-match fallback is now case-insensitive. On case-insensitive filesystems (Windows NTFS, macOS APFS) the canonical-path fallback (#997) already accepted mixed-case PATHS, but the *name* fallback didn't — `Pincher-repo` passed by an agent failed to resolve against the stored `pincher-repo` name and silently fell back. Exact-case match is still preferred when both an exact-case and casefold-only match exist; only when no exact-case row is found does the casefold fallback fire.
- Cross-project `search` (`project="*"`) now emits `project_id` on each result row. Pre-fix the response was a flat list of N hits with no signal which project each came from. Symbol IDs are scoped (`file::qn#kind`) and don't embed `project_id`, so on monorepo mounts with mirrored source trees (`pincher-repo` + sniffer mirrors that both contain `internal/server/server.go`) the only disambiguator was the file path — itself ambiguous. Single-project search omits the field to keep responses lean; `fields=project_id` is recognised in both modes.
- `symbol` / `symbols` (batch) / `context` / `neighborhood` now accept `project="*"` silently as the documented cross-project sentinel. Pre-fix `*` was treated as an unknown project name and produced a misleading "did not resolve — falling back to unscoped lookup" warning even though the unscoped lookup returned the right answer. `search` and `query` already accept `*`; the consistency gap meant a workflow like `search project=* → context project=*` got useful results but contradictory warning text. Unknown project names (anything other than `*`) still warn as before.
- `symbol` now warns when an unscoped lookup (no `project` arg) resolves the ID in a project other than the session's. Pre-fix the unscoped GetSymbol fallback found the ID in WHATEVER indexed project happened to carry it — mirror projects (sniffer mirrors, MCP_Combine staging, `.pincher-supported` snapshots) routinely carry identical symbol IDs to their primary repo, so the agent got source bytes from a stale fork with no signal the lookup crossed project boundaries. Self-discovered probing `cmd/pinch/main.go::main.main#Function` — returned source from the sniffer mirror, not pincher-repo. Backward compatible: the unscoped lookup still succeeds; the warning surfaces the cross-project resolution so the caller can re-issue with `project=<session>` to pin the scope.
- `context` + `symbols` (batch) now warn when an unscoped lookup resolves the ID(s) in a project other than the session's, extending #1049 from `symbol`. Same risk class: mirror projects (sniffer mirrors, MCP_Combine staging, `.pincher-supported` snapshots) carry identical symbol IDs to their primary repo, so the unscoped GetSymbol fallback can return source bytes from a stale fork with no signal. `context` is more dangerous than `symbol` because its EdgesFrom calls walk the leaked project's graph — callees + imports in the response also come from the wrong tree. The batch warning aggregates: one entry per request listing the offending project_ids + count, not N per-row warnings. Closes the silent-cross-project-leak family across symbol (#1049), context (#1050), symbols (#1050).
- `neighborhood` now warns when an unscoped lookup resolves the seed id in a project other than the session's, completing the cross-project leak diagnosis family across symbol (#1049), context (#1050), symbols (#1050), neighborhood (#1051). Pre-fix `neighborhood id=cmd/pinch/main.go::main.main#Function` with no project arg returned 13 neighbors from the sniffer mirror project — bytes pulled from the wrong on-disk file. Agents using the neighbor list to plan an in-file refactor were planning against the wrong file with no signal. Backward compatible: the unscoped lookup still succeeds; the warning surfaces the cross-project resolution alongside the response.
- `trace` (id-mode) now warns when the seed resolves into a project other than the BFS traversal target. Pre-fix `trace id=cmd/pinch/main.go::main.main#Function` on a workspace with mirror projects landed the seed in whichever indexed project carried the id, but the BFS scoped to the session project — edges live in the seed's project not the BFS's, so hops silently came back empty with no signal whether the result was real ("no callers") or a scope mismatch. Completes the cross-project leak diagnosis family across symbol (#1049) / context (#1050) / symbols (#1050) / neighborhood (#1051) / trace (#1052). Skipped when `project="*"` or an explicit `project=<x>` is passed (deliberate scope choices).
- `changes` now surfaces an empty-state diagnosis when the requested scope returned 0 changed files. Pre-fix `changes scope=staged` on a clean working tree (or with only unstaged edits) returned `{changed_files: [], changed_symbols: [], impacted: [], summary: {...:0}}` with NO `_meta` — the caller couldn't tell "nothing changed" from "I asked the wrong scope." Now probes the other scopes (`staged`/`unstaged`/`all`) on empty and reports which DO have content, plus offers next_steps pointing at each. When every scope is clean, the diagnosis suggests `scope="base:<branch>"` for a committed-baseline comparison.
- `doctor` lowers the `top` ceiling from 500 (#1016) to 50. Doctor returns three sections at `top` each (`projects` / `extraction_failures` / `slow_queries`) plus per-row detail (file paths, multi-line stack traces) — at top=500 the response still ran ~218 KB and exceeded the MCP per-call token cap. Dogfood-discovered: `doctor top=99999` (which #1016 clamped to 500) still returned "result exceeds maximum allowed tokens" with no recovery affordance. 50 × 3 sections × ~400-byte rows lands well inside the per-call budget. For deeper enumeration use `list` (paginated) or pinchQL queries against the underlying tables.
- `health` + `stats` now accept `project="*"` silently as the documented cross-project sentinel. Pre-fix passing `*` to either tool produced a misleading "did not resolve — falling back" warning even though the caller deliberately passed it (likely thinking they support cross-project the way `search`/`query` do). Extends the #1048 fix from the per-ID retrieval tools (symbol/symbols/context/neighborhood) to the aggregate tools. Unknown project names still warn as before.
- Per-project tools (`architecture` / `schema` / `trace` / `dead_code` / `changes` / `adr` / `index` / etc.) now reject `project="*"` with a clear-rejection error naming the cross-project sentinel and pointing at `search` + `query` as the two tools that accept it. Pre-fix the bare "project '*' not found" error treated the deliberate sentinel as a typo and pointed at `list` — which never contains `*`, deepening the confusion. Built into `resolveProjectID` so every tool that scopes to a single project gets the better error automatically. Genuine typos still surface as "not found"; only the literal `*` arg trips the new path.
- Fixes master breakage from #1057: `python-app` was added to `cmd/pinch/snapshot_test.go`'s `corpora` slice without a matching entry in `searchRelevanceQueries`, which tripped `TestSearchRelevance_QueriesRegistered`. Adds three curated queries (`open_session` / `Session` / `user_name`) targeting Function / Class / Method respectively, and regenerates the snapshot to include the `search_relevance` block. Also commits a portable `scripts/strip-snapshot.go` helper so future corpus regen can drop the `jq` dependency on Windows. PR #1057 should have caught this in CI — investigating why the gate was skipped is filed separately.
- `pinchQL` now returns the full node object for bare-variable RETURN. Pre-fix `MATCH (n:Function) RETURN n` returned `{"n": "Open"}` — just the name string — even though the comment in `buildResult` claimed "return all properties for the variable" and Cypher spec requires the entire node. Comment-implementation drift in the canonical silent-confidently-wrong shape. The new shape: `{"n": {"name": "Open", "kind": "Function", "language": "Go", ...}}`. Property-specific projection (`RETURN n.name`) unchanged.
- Every tool response now stamps `_meta.index_in_progress` (with files_done/files_total) and a warning when the session project's indexer is mid-pass. Pre-fix, the 30-60s window after a binary-swap respawn (PINCHER_AUTO_RESTART_ON_DRIFT, #352) returned silently-incomplete search/query/trace results — no flag, no diagnosis, and the standard empty-result advisory misdiagnosed the cause as low min_confidence. Agents had no way to know to retry. Cheap probe (atomic counters, no DB hit); only stamped when genuinely active so quiet calls don't pay the field weight. Auto-restart-on-drift workflow now self-signals when its window closes. Same silent-confidently-wrong family as #836/#944.
- pinchQL `ORDER BY` on a RETURN-clause alias (`RETURN n.complexity AS c`, `RETURN COUNT(*) AS cnt`) no longer emits a misleading "sort was silently dropped" warning. The post-projection sort in `buildResult` (property-alias path) and the aggregate path already resolved both alias shapes correctly — but `collectUnknownOrderByWarnings` only consulted the property whitelist via `cypherPropToCol`, so the user saw a warning telling them their sort hadn't applied even though it had. The warning now skips when the orderBy target matches any `returnVar.alias`, while still firing for genuinely unknown columns (the #881 behavior is preserved).
- `guide` no longer leaks call-family and trace-family verbs into the hint extraction. Pre-fix, `task="trace what calls processPayment"` yielded `hint="calls processPayment"` and the trace recommendation templated `name="calls processPayment"` — which doesn't resolve. The shape detector already owns these tokens (`shapeTraceIn`'s keyword list includes "calls"); the hint extractor was treating them as discriminators alongside legitimate identifiers. `taskHintFromString` now also drops `call`/`calls`/`called`/`caller`/`callers`/`calling`, `trace`/`traces`/`traced`/`tracing`, and `uses`/`used` as stopwords — same treatment the audit/refactor/fix verbs already get.
- `search` now surfaces the `corpus="all"` deprecation as a `_meta.warnings` entry, not just a stdout log line. Pre-fix, callers passing the legacy `corpus="all"` value got a soft-redirect to `corpus="code"` and a `slog.Warn` log line — invisible to the agent calling the tool. The agent thought it was searching every corpus, got code-only results, and had no signal that the override happened. Same failure-as-pedagogy shape as the case-fix (#902/#910) and unknown-property (#473) families. `corpus=""` (default) still emits no warning; only the explicit deprecated value triggers it.
- Index `Index()` now treats a binary-version drift as `force=true`, forcing re-extraction of files whose content hasn't changed but whose extractor has. Pre-fix `python_extract.py` (and similar files indexed pre-#856 Python AST) stayed stuck on the regex path forever — file content unchanged → hash matched → indexer skipped → new extractor never ran. The Module symbol for nested-package Python files was the visible symptom (#936), but the gap applied to any extractor/resolver upgrade that's behaviorally significant without changing file content. Empty `binary_version` on either side (legacy projects pre-v18, dev builds without `--version` stamp) opts out so one-shot CLI runs don't nuke the hash cache every call. Explicit `force=true` still works for legacy projects that need to opt in. Same dogfood-recovery principle as the auto-restart-on-drift workflow (#352).
- `guide` classifier no longer misroutes meta questions about pincher's own tool surface to `shapeReview`. Pre-fix, `task="what's the difference between symbol and context"` matched the substring `"diff"` in `"difference"` and routed to `shapeReview` (which then recommended the `changes` tool — meaningless for a comparison question). New `reviewDiffWord` regex word-bounds the "diff" keyword the same way `refactorExtractWord` (#784) word-bounded `extract` to avoid catching `extraction` / `extractor`. Bare "diff" still routes to shapeReview; "difference"/"different"/"differentiate" don't.
- `search project="*"` (cross-repo search) now populates the `snippet` field on every result. Pre-fix the snippet path resolved the project root ONCE from the session/explicit project — but `project="*"` leaves the resolved projectID empty, so root="" and the disk read short-circuited for every result. Agents running cross-repo discovery (the canonical "which repo has this symbol" workflow, #395) lost the BM25-snippet discriminator and had to round-trip a `symbol` call per result. Fix resolves root per-symbol via `r.Symbol.ProjectID` with a small per-call cache so single-project performance doesn't regress.
- `guide`'s `taskHintFromString` strips apostrophes before tokenizing so contractions don't leave stray single-letter tokens. Pre-fix "show me the indexer's worker pool" hinted "indexer s worker pool" — the `'` split "indexer's" into ["indexer", "s"], and the bare "s" survived stopword filtering. Curly Unicode apostrophe (`’`) handled too. Same #921/#933/#937 hint-extraction refinement family.
- `guide` audit templates for docstring + untested coverage now match exported Methods alongside Functions. Pre-fix `MATCH (n:Function) WHERE n.docstring IS NULL ...` skipped Go's method-heavy idioms (the MCP server's many `handleX` methods, all repository-style stores). On a method-heavy codebase this hid the majority of the coverage gap. Templates now use `MATCH (n) WHERE (n.kind="Function" OR n.kind="Method") ...` and project `n.kind` so callers can distinguish the two in results. Complexity + line-count templates intentionally keep Function-only — those metrics' phrasings read as function-shaped questions. Same #921/#923/#924 audit-template refinement family.
- Python AST parser identity is now visible in both `health` and per-symbol confidence. Pre-fix the Python langAdapter registered `confidence=0.85` (the regex fallback's honest floor) and the dispatcher returned identical FileResults whether the AST path or the regex path ran. Result: `health` labeled Python `parser="Regex"` even when CPython 3 was available and AST extraction succeeded, and per-symbol `extraction_confidence` stamped ~0.975 either way — no signal in the response or in the database to tell the two apart. Two changes: (1) `health` upgrades the Python label to `"AST"` when `PythonAvailable()` returns true (the same gate the dispatcher uses), and (2) `FileResult.ConfidenceOverride` lets a dispatcher declare the actual extractor tier; `extractPythonAST` sets it to 1.0 so AST-extracted Python symbols stamp at ~0.99+ instead of ~0.975, making `min_confidence` filtering distinguish AST from regex. Same silent-confidently-wrong family as #836/#945/#946.
- `fetch` now extracts body content from HTML5 pages that use `<header>` elements (Wikipedia, MDN, most modern docs sites). Pre-fix the wholesale-block stripper matched tag-name prefixes literally — `<head` matched both `<head>` (document head) and `<header>`. The closing-tag search for `</head>` did not match `</header>`, so the "no closing tag" branch fired and truncated the document from the first `<header>` onwards. Wikipedia (100+ `<p>` tags, multiple `<header>` elements, fully static) reduced to its pre-`<header>` skip-link — 15 chars from 400+ KB. Fix adds a tag-boundary check (next char must be `>`, `/`, or whitespace) and changes the missing-closing-tag branch to skip past the open rather than truncate. The companion misdiagnosis warning ("page is likely JS-rendered") now sanity-checks by counting `<p>` tags in the raw HTML: if 5+ paragraphs exist but extraction came up near-empty, the warning blames the extractor instead of sending users in the wrong direction.
- `pinchQL` now renders `COUNT(*)` as `COUNT(*)` in the result column header instead of `COUNT()`. Pre-fix, the tokenizer read `*` as an empty HOPS token (the variable-length path scanner), so the parser set `rv.variable=""` and `aggColName` rendered `"COUNT" + "(" + "" + ")"`. The count value was always correct; only the column name lost the asterisk. Round-trip breaks when a caller uses the header as a property reference. Same silent-confidently-wrong shape as the case-fix (#902/#910), unknown-property (#473), and dashboard symbol_count key mismatch (#836) families. Aliased form (`COUNT(*) AS total`) was already correct and is unchanged.
- `search` now returns `end_line`, `start_byte`, and `end_byte` on every result. Pre-fix only `start_line` was surfaced — agents reading a result couldn't compute the function's line range, byte span, or size-aware decisions ("is this a 5-line helper or a 200-line monster?") without a follow-up `symbol` call. `neighborhood` / `symbol` / `symbols` all surfaced these fields; search was the outlier. Explicit `fields=end_line` projection used to silently substitute `null` because the response map had no such key — same silent-confidently-wrong shape as #836/#945/#946. OpenAPI search response schema (`/v1/openapi.json`) updated to match.
- `guide` now routes "find all functions over 100 lines" (and the rest of the bare-preposition threshold phrasings) to shapeAudit + the line-count pinchQL template. Pre-fix the existing `auditThresholdPattern` required "with/having/whose" scaffolding and `auditLooseThresholdPattern` required a comparative adjective ("longer than"); "over N units" / "under N units" / "more than N" / "at least N" / "<=N" / "≥N" all fell through to BM25 search on the literal words — silently failing on the most common audit phrasing in code review. New `auditBareThresholdPattern` anchors on a trailing digit so prose ("look over there") doesn't false-trigger. Same #473-family silent-quality-loss as the previous audit-classifier gaps (#921/#923/#924).
- `search` now surfaces a `_meta.warnings` entry when `kind` or `language` is set to an unknown enum value (e.g. `kind="FunctionTypoKind"` or `language="PythonTypo"`). Pre-fix the typo'd filter silently returned 0 rows and the diagnosis recommended "drop the kind filter" — implying the value was valid but selective. The warning names the unknown value and lists the canonical set. Existing case-mismatch path (#902/#910) still handles `kind="function"` (lowercase) separately. Same #473-family silent-quality-loss as the audit-classifier and corpus="all" deprecation (#935) fixes.
- `eventBus.subscriberCount()` was flagged as dead code in v0.58 dogfooding even though its doc-comment claimed "used by tests" — the tests didn't exist. Adds `eventbus_test.go` exercising subscribe/publish/unsubscribe lifecycle (3 cases including idempotent double-unsubscribe and non-blocking backpressure) so the method is now actually load-bearing and the comment is no longer a lie. Caught by `mcp__pincher__dead_code`.
- `mcp__pincher__health` latency could spike to 30+ seconds during a full binary-drift re-extract (#960) because `HealthCheck` ran its three SELECTs against the single-writer connection. The single writer was held by the indexer goroutine bulk-upserting symbols, so health probes queued behind every file. Route the three SELECTs through the reader pool (`s.ro`) and reclassify `HealthCheck` as reader-routed in `db_test.go` — the original "mixed read+write for transactional consistency" justification didn't match the implementation (no writes inside). Caught dogfooding the v0.58 supervisor auto-restart loop: a single `mcp__pincher__health` call after a binary swap took 38560ms.
- `Watch()` now triggers a reindex when the project's stored `BinaryVersion` differs from the running indexer's version, not only when source file mtimes are newer than `IndexedAt`. Closes the gap in #960: after a supervisor auto-restart swapped pincher onto a new binary, `Watch` wouldn't call `Index()` until the next source-file save, so the `binaryDriftForce` branch inside `Index()` never fired and the new binary served old-binary extraction quality. Observed during dogfooding: a `0.56.0` → `0.57.0` supervisor swap left `pincher-repo` showing 1169 symbols / 4660 edges for 22 minutes; `force=true` recovered the expected 5776 / 11773. (#972)
- `errResultRich` now stamps `_meta.index_in_progress` (and a retry-after-pass nudge prepended to `next_steps`) when the session indexer is mid-pass, matching the existing success-path `jsonResultWithMeta` behavior. Pre-fix, an agent that got `"symbol not found"` during a binary-drift re-extract had no signal the result was transient and could conclude the symbol genuinely didn't exist. Closes the silent-confidently-wrong gap that surfaced this session when `trace name=disambiguateDuplicates` returned `not found` despite the function being defined two files away. (#974)
- `pinchql` query with `BETWEEN x AND y` now returns a teaching error pointing at the two-ANDed-comparisons workaround (`n.start_line >= 100 AND n.start_line <= 200`) instead of the bare `"unsupported operator: BETWEEN"`. SQL/Neo4j range-membership muscle memory is high-traffic, and the previous error gave no path forward. Joins the existing operator-hint family (`!=`, `LIKE`, `REGEXP`, `IN`, arithmetic). Caught during the v0.58 dogfood EXPLORE pass.
- `mcp__pincher__query` now surfaces a deprecation warning in `_meta.warnings` when called with the legacy `cypher` parameter alias instead of `pinchql`. Pre-fix the alias was honored silently, so agents using `cypher` had no signal the migration window was closing — the day the alias is removed every cached call site would break at once. Matches the corpus="all" soft-redirect pattern (#935): observable redirect beats silent rewrite.
- `mcp__pincher__context lite=true` now emits the same staleness warning the non-lite path does when the underlying file has been edited since indexing. Pre-fix the lite short-circuit ran before `attachStalenessWarning`, so an agent redirected from `Read` via the PreToolUse hook received stale source bytes silently — same silent-confidently-wrong family as #317 (stale-bytes warning) and #960 (binary drift). The minimum-envelope contract means "skip imports/callees/next_steps," not "swallow correctness signals." Discovered dogfooding the post-merge state: `context lite=true` on `errResultRich` returned the pre-#975 body even though the file on disk had the new version.
- `mcp__pincher__neighborhood include_source=true` now emits a single `_meta.warnings` entry when the underlying file has been edited since indexing — every neighbor shares the same file, so one warning per call is the right shape vs N per-symbol warnings. Pre-fix the response shipped stale source bytes for every sibling silently. Same silent-confidently-wrong family as #317 / #960 / #978. `include_source=false` (default) is unaffected — signatures stay correct even when offsets drift.
- `mcp__pincher__context` now emits `_meta.warnings` entries for every IMPORT and CALLEE file that has been edited since indexing, not just the seed file. Pre-fix, editing only a callee file (no edit to the seed) returned stale callee source bytes silently — the seed's hash matched, no warning fired, the agent acted on the wrong dependency body. Closes the last byte-offset read path missing staleness coverage; matches the audit shipped in #978 / #979. One re-index next_step is appended whether one or many files are stale (force=true covers all at once). (#980)
- `mcp__pincher__changes` now returns a rich error envelope when `git diff` fails (typo'd base branch, non-existent ref, etc.) instead of a bare `errResult`. The `_meta.next_steps` carries the four supported scopes (`unstaged`, `staged`, `all`, `base:<branch>`) so the agent learns the valid shapes without round-tripping through docs. Failure-as-pedagogy applied to the most common typo path.
- `mcp__pincher__list` empty-state diagnosis now names every active filter cause (inactive, dead-on-disk, low-edges) and only emits the recovery `next_steps` for filters that actually fired. Pre-fix the diagnosis hardcoded "stale or dead-on-disk" and the `next_steps` listed only `active=false` + `include_dead=true` — so a caller whose `min_edges=N` was the dominant cause (e.g. 73 of 128 dropped during a high-edge audit) saw a misleading explanation and recovery hints that wouldn't have helped. Now: `min_edges=0` is surfaced when low-edges drops fired, and each cause is named in the diagnosis.
- `mcp__pincher__schema` now surfaces a `_meta.diagnosis` and recovery `next_steps` when the resolved project has 0 indexed symbols. Pre-fix the response was just `{symbols:0, edges:0, node_kinds:{}, edge_kinds:{}}` with no signal — the caller couldn't tell whether the project really was empty or whether the project arg matched a stale name (e.g. `project="pincher"` resolving to a dead-on-disk `D:��pincher` row instead of the intended `pincher-repo`). Recovery hints surface `list include_dead=true` (to see the collision) and `index force=true` (to re-extract from the right path).
- `resolveProjectID` now prefers a project whose path exists on disk when a name-collision matches both a live and a dead-on-disk row. Pre-fix the first match in `ListProjects` order won, so a stale `D:��pincher` (0 symbols, dir gone) routinely out-resolved the intended live `pincher-repo` and downstream tools (`search`, `query`, `trace`, `schema`, `context`) returned silently empty results. The root-cause fix complements #984 (schema empty-state diagnosis): #984 made the symptom visible; this PR keeps the right project resolving in the first place. Dead-only matches still resolve (with a slog warning + remediation hint) so single-stale callers don't break — only collisions flip.
- `mcp__pincher__query` cypher parse/syntax errors now return a rich error envelope with `schema` + working-example + `guide` next_steps. Pre-fix the bare `errResult("cypher error: ...")` left the agent staring at a wall when the operator-hint family (BETWEEN, !=, arithmetic, etc.) didn't already teach the workaround — malformed regex, unknown predicates, type mismatches all dead-ended. Now every cypher failure surfaces the three recovery paths inline. Matches the failure-as-pedagogy throughline of #976/#977/#982/#983/#984.
- `mcp__pincher__init` argument-validation errors (missing `project_path`, `target=continue` rejection, unknown target) now return rich envelopes with explicit recovery `next_steps` examples. Pre-fix the bare `errResult` left the agent with a wall of text and no copy-paste-able recovery shape. The unknown-target path surfaces both `target=detect` (let pincher auto-pick) and `target=all` (write to every per-project target). Matches the failure-as-pedagogy throughline of #976/#977/#982/#983/#984/#987.
- CLAUDE.md and docs/REFERENCE.md Project-layout sections corrected to point at `internal/index/bloat_trap.go` instead of the long-stale `cmd/pinch/bloat_trap.go`. The file moved to `internal/index` in #790 so both CLI and MCP `index` handlers share the same `IsBloatTrap` guard; both docs were still telling readers to look in `cmd/pinch/` for it. Caught during a dogfood probe of dead_code / list — agents trying to reason about the bloat-trap guard from the README would have looked in the wrong place.
- `guide` audit-shape detection now recognizes audit phrasings that lack the `every|all|any` quantifier — "find symbols that have no test coverage", "list functions without docstrings", "find handlers missing error returns". Pre-fix the regex required the quantifier, so these slipped through to `shapeFind` → BM25 search of the literal phrase ("have no") which matches nothing. The absence-phrase alternation (`without|missing|has no|...`) is the load-bearing audit signal; dropping the optional quantifier preserves the overcatch-protection (a phrase like "find the auth middleware" lacks an absence word and still falls through to shapeFind). Same failure-as-pedagogy throughline as #467/#608 — the third sibling that completes the audit-classifier family.
- `_meta.index_in_progress` warnings now phase the message — when `files_done == files_total` the per-file walk has finished but cross-file resolvers are still running, so the warning reads "indexer is finalizing (cross-file resolver running after N/N files extracted)" instead of the silent-confidently-wrong "indexer is mid-pass (55/55 files)". Caught during a dogfood probe of #993: agents saw 100% file completion and an alarming "retry" suggestion in the same response. Both the success-path (`jsonResultWithMeta`) and error-path (`errResultRich`) wordings are updated for symmetry.
- `init` MCP tool description now lists every supported target. Pre-fix the description claimed targets were "claude / cursor / cursor-legacy / windsurf / aider / detect / all" — missing **codex, zed, gemini, warp, vscode, vscode-mcp** which have all shipped in the last several releases (#658, #732, #970, #989). Agents reading the description before calling would conclude these targets didn't exist; one of the failure-as-pedagogy modes pincher is most prone to. Same staleness shape as #698 (README leading paragraph) and #688 (REFERENCE.md leading metadata) — every drift between code and self-description is silent-confidently-wrong territory.
- Plugin README, plugin.json manifest, and the GitHub Pages landing page (`docs/index.html`) all referenced the long-stale "16 MCP tools" count. Actual count has been 22 for several releases (closure tables + supervised + operator tools etc.). The 6-tool drift is exactly the kind of stale-leading-paragraph staleness #698 caught for README. Anyone discovering pincher via the Claude Code plugin store or the GitHub Pages site was reading a copy that materially under-stated the surface — and the anchor link `#the-16-mcp-tools` 404'd against the now-`#the-22-mcp-tools` anchor in `docs/REFERENCE.md`.
- Follow-up to #998 picking up the staleness drifts that raced past the auto-merge window. Three more tutorial anchor links (`#the-16-mcp-tools` → `#the-22-mcp-tools` in claude-code.md / cursor.md / vscode-copilot.md), the internal/server/server.go package doc + `New()` constructor doc (still claimed "all 16" and "all 14 MCP tools" respectively), and the CLAUDE.md Package-responsibilities line that was 7 schema versions behind (v19 → v26). Same staleness-sweep theme as #998.

### Removed
- Removed dead `bytesFindHeading` wrapper in `internal/ast/html.go`. #1004 added `bytesFindHeadingAfter` and refactored the only call site to use it directly with the start offset — the original `bytesFindHeading(source, level, title)` became a thin shim delegating to `bytesFindHeadingAfter(..., 0)`. Verified zero callers (in-package, tests, and full-repo grep). Folded the doc comment into the surviving function. Surfaced by `dead_code` on the new binary; precision-fix loops back into precision.
## [v0.57.0] - 2026-05-15

**Python AST + C CALLS + type-info resolver + silent-confidently-wrong sweep.** Phase 1 release 6 of 9. Schema **v26** (`pending_edges.base_type` for type-info resolver). Three structural extractor wins — full Python AST extractor with cross-file IMPORTS+CALLS resolution (#856), per-file CALLS pass for C (#858), and the type-info resolver that closes the dead_code FP triangle's last leg (#760). The bulk of the release is a 30-issue dogfood pass closing the silent-confidently-wrong family — queries that return plausible-looking but wrong results with no signal: guide shapeAudit, FTS5 sanitizer, pinchQL DISTINCT/ORDER BY/COUNT/CONTAINS, max_rows enforcement, plus pedagogy across the failure surface.

### Added
- Full AST-based Python extraction with IMPORTS and CALLS edge resolution (#856). Python was regex-extracted at 0.85 confidence with no edge graph — `trace`, `dead_code`, `neighborhood`, and the closure-table fast-path were all no-ops on Python repos. The new extractor shells out to a CPython 3 interpreter running an embedded stdlib-`ast` helper script: correct nested-class QNs, `async def`, decorators in signatures, full type annotations, `__all__`-driven `IsExported`, a per-file Module symbol, and — crucially — resolved IMPORTS and CALLS edges. Python's dotted module-path imports are bridged to pincher's file-path-derived QNs via source-root autodetection (pyproject.toml setuptools/poetry/hatch, setup.py, setup.cfg, and an `__init__.py` filesystem walk). Default-on when a working CPython 3 is on PATH (the interpreter is probe-verified, not just `LookPath`-checked, so a non-functional Windows `python3` shim doesn't mask the real `python`); opt-out via `PINCHER_DISABLE_PY_AST=1`; transparent fallback to the regex extractor on parse failure or no interpreter. Measured on the `rich` codebase: 0 edges with the regex fallback → 3,973 resolved edges (3,099 CALLS + 874 IMPORTS) with the AST extractor.
- C source files now produce a CALLS edge graph. The regex extractor gained an opt-in per-file CALLS pass (`extractOpts.extractCalls`), enabled for C: each function body is scanned for C-family `name(` call sites and CALLS edges are emitted from the enclosing function. Same-file targets resolve; keywords (`if`/`for`/`while`/`sizeof`/…), undefined names, and cross-file targets drop at the per-file resolver, so `trace` and `dead_code` now return real results on C corpora instead of a confidently-empty graph (#858). Cross-file C resolution is still out of scope — that needs the AST-extractor track (#268). The pass is off for every other regex-tier language until its call syntax is validated against `regexCallScan`.

### Changed
- Milestone celebrations (#494) are now opt-in. The `_meta.celebration` line ("Pincher has saved you X tokens…") is only emitted when `PINCHER_CELEBRATIONS=1` is set — default off. Celebrations are one-shot per threshold per installation by design, but any workflow that spins up fresh DBs (throwaway indexes, CI, per-task temp dirs) re-fires every tier from zero, so the line became recurring noise rather than a rare signal. The `celebrations` table and machinery are unchanged; only the default emission is gated.

### Fixed
- Go struct-field reads no longer false-bind to same-named project Methods. The `resolveReads` binding pass (#565) converts a READS edge that resolves to a Function/Method into a confidence-0.4 CALLS edge so function-value bindings (`w.doFn = w.defaultDo`) stay reachable — but on name alone it couldn't tell `e.Confidence` (a struct-field read) from `w.defaultDo` (a method value), so a field read whose name collided with a project Method false-bound to it (`e.Confidence` → `*hclExtractor.Confidence`, the residual of #758). The extractor now tracks a per-function local-variable → declared-type map (receiver, params, `var x T`, `x := T{}` composite literals, slice-range variables) and stamps the base expression's type onto selector READS edges via the new `ExtractedEdge.BaseType` field, persisted through `pending_edges.base_type` (schema v26). The binding pass drops the edge when that type names a project struct with a field of the READS edge's target name — a positive confirmation that the AST node was a field access, not a function-value reference. Genuine method-value bindings are unaffected. Closes the last leg of the dead_code false-positive family.
- `trace` and `dead_code` no longer return silently-misleading empty results on non-Go/non-Python projects. Cross-file edge resolution (`resolveImports`/`resolveCalls`/`resolveReads`) covers Go and Python; C / TypeScript / Rust / etc. extract symbols fine but produce a zero-edge graph, so an empty `trace` read like "no callers" and an empty `dead_code` like "no dead code" when both actually meant "this language has no edge graph." Both tools now emit a `_meta.diagnosis` naming the coverage gap (#858) when the project's dominant language has no edge resolution and the result is empty for that reason — and `dead_code` no longer suggests lowering `min_confidence` in that case (there are no edges at any confidence). Genuine empty results on Go/Python projects are unaffected. The underlying gap — non-Go edge resolution itself — remains open (#266/#268).
- Python `import` statements no longer false-bind to same-named config-file keys. `import os` (a stdlib import with no in-project target) resolved to a `Setting` symbol whenever a JSON/YAML/TOML file in the project had a top-level `os` key — those extract as a `Setting` whose qualified name is literally the key string, and `resolveImports`' canonical-pick had no kind filter on the Python path. An IMPORTS-edge target is always a code symbol (Module, Class, Function, Method); the Python to-side lookup now drops config/docs kinds via the same `excludeNonCodeSyms` guard `resolveCalls` already applies (#762/#790). Found hammer-testing the #856 Python AST extractor against the `rich` codebase.
- pinchQL no longer silently returns zero rows for an unknown edge kind. `MATCH (a)-[:CALLZ]->(b)` (a typo) compiled to `e.kind IN ('CALLZ')`, matched nothing, and returned a confidently-empty result with no signal — the edge-side twin of #473's unknown-property silent zero. `query` now emits a `_meta.warnings` entry naming the unrecognized kind and listing the valid taxonomy (CALLS, HTTP_CALLS, ASYNC_CALLS, READS, WRITES, IMPORTS, REFERENCES), matching the guard `trace` already applies. Relatedly, edge kinds are now upper-cased at parse time, so a lower-case `-[:calls]->` resolves instead of silently matching nothing against the upper-case-stored `kind` column.
- pinchQL no longer silently mishandles an inverted variable-length hop range. `MATCH (a)-[:CALLS*3..1]->(b)` — bounds written backwards — collapsed to `*3..3` via `parseHops`' `max = min` clamp, so a transposed-bounds typo returned depth-N-only results that matched neither the written range nor the likely intent (`*1..3`), with no signal. `parseHops` now swaps inverted bounds to the intended range and the engine emits a `_meta.warnings` entry naming the swap — the same failure-as-pedagogy treatment as the unknown-property (#473) and unknown-edge-kind (#867) warnings.
- pinchQL no longer silently truncates multi-clause MATCH queries. `MATCH (a)-[:CALLS]->(b) MATCH (a)-[:READS]->(c) RETURN a.name, c.name` parsed cleanly — additional patterns appended to `q.patterns` — but the executor only ran `q.patterns[0]`, so variables introduced by the second/third MATCH never got bound and RETURN columns referencing them silently projected NULL on every row. Same silent-confidently-wrong family as #433's chained-edge case. `Execute` now rejects multi-MATCH explicitly with a remediation pointer (combine into a single MATCH, or run separate queries and join client-side); proper multi-MATCH joins remain a separate design exercise.
- pinchQL `query` now reports a meaningful `_meta.confidence_distribution`, and its `min_confidence` parameter actually filters. Both rode on `rowConfidence`, which looked up the bare key `"extraction_confidence"` — but pinchQL projects with the variable prefix (`RETURN n.extraction_confidence` yields row key `"n.extraction_confidence"`), so the lookup never matched. `confs` stayed empty, `confidenceDistribution([])` returned `{"0.0-0.5":0,"0.5-0.7":0,"0.7-0.9":0,"0.9-1.0":0}` on every result set, and `min_confidence` filtered nothing. `rowConfidence` now also scans for any row key suffixed `.extraction_confidence` / `.confidence`, and `handleQuery` omits the histogram entirely when the query didn't project confidence (rather than emitting the misleading all-zero shape).
- `min_confidence > 1.0` on `search` / `query` / `trace` / `dead_code` no longer silently filters every result. `extraction_confidence` is always in `[0.0, 1.0]`, so a `min_confidence=2` filter is universally unsatisfiable — but `floatArg` returned the raw value with no validation, and every handler then produced an empty result with no signal. The new `clampMinConfidence` helper clamps values `> 1.0` to `1.0` and emits a `_meta.warnings` entry naming the clamp, alongside the existing depth / kind / direction clamps. Same silent-confidently-wrong family as the trace `depth` clamp (#703). Negative values stay as-is — the existing filter loops treat them as "no filter," which is harmless.
- `changes depth` now clamps to the documented `[1, 5]` range with a warning instead of silently coercing through `TraceByID`. Pre-fix `intArg(args, "depth", 3)` passed any value to `TraceByID`, which silently rewrote `≤0` and `>5` to `3` — so `changes depth=99` quietly ran at depth 3 while the caller believed they got a depth-99 blast radius. The `trace` handler already had this clamp (#703/#712); `changes` now matches.
- `query max_rows` and `dead_code limit` now surface a `_meta.warnings` entry when an out-of-range value is clamped, instead of silently swallowing the request. Pre-fix `cypher.Executor.maxRows()` rewrote `<= 0` to 200 and `> 10000` to 10000, and `handleDeadCode` clamped `limit > 500` to 500 — `max_rows=99999` and `limit=9999` quietly ran with reduced budgets while the caller believed they got what they asked for. Same silent-confidently-wrong family as `changes depth` (#877) and `min_confidence > 1.0` (#875); `search` and `neighborhood` already had the limit/offset clamp warnings.
- pinchQL `ORDER BY` on an unknown column no longer silently returns unsorted results. `orderByCol` / `joinOrderByCol` returned `""` for any property outside the whitelist — the SQL never emitted an `ORDER BY` clause, results came back in scan order, and the caller had no signal that their sort was discarded. `collectUnknownOrderByWarnings` now surfaces a `_meta.warnings` entry naming the dropped column and listing the valid properties — same failure-as-pedagogy shape as the WHERE-side unknown-property warning (#473), which already covered WHERE / RETURN / inline match braces but skipped ORDER BY.
- pinchQL now rejects multi-column ORDER BY (`ORDER BY a, b`) with an honest error message. Pre-fix the trailing comma fell through to the parser's generic clause-keyword catch-all and surfaced as "unexpected token ',' — expected WHERE, RETURN, ORDER BY, LIMIT" — actively misleading, since ORDER BY *was* the previous clause and the actual constraint is "pinchQL doesn't support multi-column sort." The parser now errors with the real constraint and a single-column / client-side remediation pointer, matching the #871 / #433 pattern for explicitly-rejected pinchQL shapes.
- pinchQL `CONTAINS` / `STARTS WITH` / `ENDS WITH` now do literal substring match against `%` and `_`, matching Cypher semantics. The SQL pushdown path wrapped the user literal with `%` for substring match but left the literal's own `%` / `_` unescaped, so `CONTAINS "%"` compiled to `LIKE '%%%'` and matched every row — silent semantic divergence. New `escapeLikePattern` escapes `%`, `_`, and ``, and the LIKE clauses now carry `ESCAPE ''`. The Go-side `eval` path used `strings.Contains` and was already correct; only the SQL pushdown leaked.
- FTS5 `search` queries with multiple CamelCase identifiers around an `OR` operator now return rows. Pre-fix, `handleSearch OR handleQuery` resolved to 0 results against a vtab that returned matches for each term individually — the bare CamelCase tokens flowed through FTS5's operator parser into a shape that quietly matched nothing. The sanitizer now phrase-wraps mixed-case identifiers (e.g. `handleSearch` → `"handleSearch"`), making multi-token OR queries actually surface their hits. Semantically a no-op for single-word queries — a one-token phrase is the same search — but unlocks the common case of `Foo OR Bar`-style code lookups.
- pinchQL WHERE comparisons that cross literal type — e.g. `n.start_line = "twenty"` (int column, string literal) or `n.name = 12345` (text column, number literal) — now surface a warning instead of silently returning 0 rows. SQLite's type affinity coerces the literal under the hood and typically yields no matches, which used to read as a confidently-wrong "nothing matches your filter" for what is actually a malformed query. Same warning-surface family as #473 (typo'd property), #867 (unknown edge kind), #881 (unknown ORDER BY column). The classifier `cypherPropType` is the single source of truth: text / int / real / bool, derived from the same column whitelist as the SQL pushdown. Boolean columns still accept the canonical forms (`true`/`false`, `1`/`0`); a clear typo (`is_exported = 42` or `is_exported = "yes"`) warns.
- pinchQL: a NULL property value no longer matches BOTH `col = ""` AND `col <> ""`. Pre-fix the SQL emitter wrapped inequality in `(col IS NULL OR col<>?)` and the in-Go evaluator returned TRUE for NULL-vs-anything, so combined with #606's NULL-match-on-`=""` rule the same row satisfied both predicates — logically impossible and breaks every "find missing field" audit since the two predicates no longer partitioned the corpus. Now `<>` with a zero-value RHS (`""` or bool-false) is the dual of `=`: NULL excluded. For a non-zero RHS the inequality keeps the pre-existing "NULL surfaces" behaviour — `WHERE col <> "x"` naturally reads as "anything but x" and users expect NULL/missing rows.
- `health` no longer reports a project's file / symbol / edge counts as 0 during the brief window between start-of-index and the first counts-flush. Pre-fix the indexer's start-of-Index() `UpsertProject` call zeroed the cached counts (the Go struct's zero values overwrote the previous run's accurate totals via the UPSERT's `file_count=excluded.file_count` clause). `UpdateProjectCounts` caught up within seconds, but anything calling `health` during the gap saw an empty-project shape for a project that was otherwise intact — `list` and `architecture` correctly reported the same counts at the same moment, and `extraction_coverage` inside the same `health` response named all the symbols. New `UpsertProjectMeta` updates path / name / indexed_at / binary_version / schema_version_at_index without touching the count columns; the indexer's start-of-run call now uses it. The end-of-run `UpsertProject` continues to write authoritative totals.
- `dead_code` empty-result advisory no longer recommends `min_confidence=0.7` when the caller is already at or below 0.7. Pre-fix the suggested floor was hard-coded — at `min_confidence=0.7` the suggestion was a no-op, and at `0.0` it was a logical inversion (0.7 is HIGHER, would NARROW the candidate pool, the opposite of "find more dead code"). The advisory now steps down through the meaningful tiers (0.95→0.7→0.0) and drops the min_confidence hint entirely when the caller is already at the widest floor, falling back to a "broaden kinds" recommendation instead.
- `trace` no longer hides the `include_tests=true` escape hatch when every inbound/outbound hop was a test or fixture file. Pre-fix the default `include_tests=false` filter silently dropped all hops, and the empty result advised "no call edges found at this depth — read the symbol's own source instead." For a heavily-tested utility (e.g. a test-injection variable whose only writers are `*_test.go`) that reads as "this symbol is unused" — confidently wrong. The advisory now counts filtered hops and, when the trace would have surfaced rows but all were tests, surfaces a retry hint that preserves `direction` + `kinds` while passing `include_tests=true`. Genuine-leaf traces (no edges of any kind) keep the legacy "read source" recommendation.
- pinchQL `max_rows` is now a hard upper bound on the result set. Pre-fix it was only used as a scan-headroom hint (`scanLimitFor` returned `max_rows × 2`) and was never applied as a final-result cap, so a query like `RETURN n.name LIMIT 99999999` against `max_rows=5` returned 10 rows. The MCP arg's name and schema description (`Max rows`) implied a strict upper bound and callers using it to budget response size or paginate were silently getting 2× the count. Final result is now clamped to `max_rows` with a warning naming the trimmed-from count; aggregating queries (`RETURN COUNT(n)` / `RETURN n.kind, COUNT(n)`) skip the trim because their row count is the cardinality, not a sample.
- `search`'s empty-result advisory now teaches the case fix when the user's `language=` filter only fails because of case. Pre-fix `language="JaVaScRiPt"` (stored canonical: `JavaScript`) returned 0 and the advisory said "drop the language filter" — over-broadening the user's actual intent (filter by JavaScript, not by all languages). When the relax probe finds matches AND the user's input case-normalises to a known language, the advisory now names the canonical form (e.g. `JavaScript`) and the next_step retries with the corrected case, preserving the filter. Unknown-language values (e.g. `BogusLang`) still fall back to the drop-the-filter advisory.
- pinchQL `ORDER BY n.name` now sorts correctly when the projection renames the column (e.g. `RETURN n.name AS funcname`). Pre-fix the post-scan Go sort looked up `q.orderBy` directly in the projected row map, but the projection had rewritten the key to the alias (`funcname`) — so the lookup returned nil and the sort silently no-op'd, leaving rows in scan order. `ORDER BY funcname` (the alias) already worked. Now both spellings find the same column via an alias-resolution map built from `q.returnVars`. Same family as #881 / #883 — ORDER BY drops keep recurring because the projection/sort plumbing is split.
- pinchQL `COUNT(n.property)` now honors SQL/Cypher non-null semantics. Pre-fix every COUNT shape (`COUNT(n)`, `COUNT(*)`, `COUNT(n.docstring)`) returned the row count via `len(rows)`, making the property-keyed form indistinguishable from the row-keyed form. So `COUNT(n.docstring)` on a corpus where most functions have NULL docstring returned the total function count — silently wrong on the canonical "how many functions are documented" query. `COUNT(n)` / `COUNT(*)` keep their row-count semantics; `COUNT(n.prop)` now counts only rows where the property is non-null. Empty-string TEXT values still count as non-null per SQL semantics.
- `symbol` and `symbols` now warn on unknown `fields=` values, matching `context`'s existing behavior. Pre-fix three handlers had three different responses to the same typo: `context` warned, `symbols` silently dropped the unknown field, and `symbol` silently INCLUDED it with a `null` value — making the response look like the field existed in the schema. A downstream consumer iterating response keys would think `nonexistent_field: null` was a legitimate field, the deepest of the three failure modes. All three now route through `projectFieldsChecked` (or the equivalent batch check), drop unknown fields, and surface a `_meta.warnings` entry naming each unknown field plus the valid key set. Behavior on valid `fields=` is unchanged.
- `search`'s empty-result advisory now teaches the case fix when the user's `kind=` filter is only wrong by case. Parallel to #902 for the `language=` filter. Pre-fix `kind=FuNcTiOn` recommended "drop the kind filter" — which over-broadens to all kinds. The advisory now probes the canonical form (Function / Method / Class / etc.) via the new `canonicalKindCase` helper; when the canonical case matches, the next_step preserves the filter with the corrected case. Genuinely unknown kind values (e.g. `BogusKind`) still fall back to the drop-the-filter advisory.
- `guide` now routes threshold/comparison audit tasks (e.g. "find every function with complexity above 50") to `shapeAudit` — the pinchQL workflow. Pre-fix the audit detector only matched the absence pattern (`without`/`missing`/`lacks`/`with no`); threshold phrasings fell through to `shapeFind` and the recommendation was a BM25 search of the literal phrase, which can't find the answer (BM25 doesn't read numeric columns). New `auditThresholdPattern` covers `above`/`over`/`exceeds`/`greater than`/`more than`/`below`/`less than`/`at least`/`at most`/`>`/`>=`/`<`/`<=` with 0-3 optional metric words between `with`/`having`/`whose` and the comparison, so both "with more than 100 lines" (zero metric words before the multi-word comparison) and "with complexity above 50" (one metric word) route correctly. Same advisory-recognises-intent shape as the case-fix family (#902/#910).
- `fields=` projections on `trace` and `changes` (and any future handler routing through the shared helper) now warn when the caller passes a key that doesn't exist on the response — matching the behavior `symbol`/`symbols`/`context` got in #908. Pre-fix both handlers used the plain `projectFields` which silently dropped unknown fields; a typo'd field name (`fields=summary,bogus_field,tests_to_run`) trimmed the response without telling the caller their projection was malformed. New shared `projectAndCheckFields` helper encapsulates the runs-check-attaches-warning pattern; handleSymbol switches to it as well, eliminating the duplicated inline branching introduced by #908.
- pinchQL rejects undirected edge patterns `(a)-[r:KIND]-(b)` with a clear remediation. Pre-fix the syntax parsed cleanly but the executor only consulted outbound edges, so a query that should have returned matches in both directions silently returned only outbound. For a symbol with N outbound and M inbound CALLS, the user got N rows where they expected N+M. pinchQL's documented stance on partially-supported syntax (#871 multi-MATCH) is to reject early with a remediation rather than half-implement; the same shape now applies here. The error names the directed forms (`-[r:KIND]->` outbound, `<-[r:KIND]-` inbound) and the union-client-side workaround.
- `search` correctly applies operator-preserving phrase-wrap to camelCase identifiers in multi-token OR/AND queries. Follow-up to #887 — that fix wrapped camelCase tokens at the wrap-individual-tokens layer but the upstream `looksLikeCodeIdent` gate only recognized PascalCase, so an expression like `handleSearch OR handleQuery` got the whole string phrase-wrapped (`"handleSearch OR handleQuery"`) instead of `"handleSearch" OR "handleQuery"`. End-to-end probing against the merged binary surfaced it; `looksLikeCodeIdent` now delegates to the same `hasMixedCase` predicate the wrap layer uses, so both camelCase and PascalCase trigger operator-preserving wrap behavior.
- `guide` shapeAudit no longer hardcodes the docstring/is_exported pinchQL template for every audit task. Pre-fix, `guide task="find every function with cyclomatic complexity above 20"` correctly classified as `shapeAudit` (#912 routes threshold phrasings here) but then recommended a fixed `MATCH (n:Function) WHERE n.docstring IS NULL AND n.is_exported=true ...` template that ignored the user's actual complexity-above-20 intent — silent-confidently-wrong. New `inferAuditPinchQL` helper routes on task keywords (`complexity`/`cyclomatic`, `long`/`lines above`/`exceed`, `untested`/`test coverage`) to emit the right structural query: complexity-threshold tasks get `n.complexity > N ORDER BY n.complexity DESC`, line-count tasks get `(n.end_line - n.start_line) > N`, untested-coverage tasks get `is_exported=true AND is_test=false`. Docstring-coverage remains the canonical #467 fallback for unclassified audits.
- `guide` shapeAudit docstring + untested templates now scope to `n.language='Go' AND n.is_test=false`. Pre-fix the canonical `MATCH (n:Function) WHERE n.docstring IS NULL AND n.is_exported=true` query returned mostly noise: regex-tier languages like JavaScript and Bash don't populate `docstring`, so 100% of their symbols matched, plus Go test functions don't follow the docstring convention. Top 10 results from pincher-repo were 6 `TestDashboardJS_*` functions, a JS `handler`, and 3 Bash helpers. After scoping to Go non-test, the result actually answers "which Go APIs need docstrings." Same fix applied to the untested-coverage template.
- `guide` now classifies natural threshold/audit phrasings that #912 missed. Two new patterns: `auditLooseThresholdPattern` catches adjective-form comparisons that drop the "every|all|any" article and "with|having|whose" clause ("find functions longer than 100 lines", "list methods bigger than 200 lines"); `auditAdjectivePattern` catches standalone audit adjectives like `untested`, `undocumented`, `uncovered`, `untyped`, `unowned`, `unauthenticated`, `unvalidated`, `unhandled`. The adjective pattern runs BEFORE the bare-`test` shapeTest check so "find untested exported functions" doesn't get recommended as a test-writing task. "unused" stays with shapeDeadCode (more specific). Pair with #923's tighter template defaults.
- `guide` line-count audit template no longer emits an arithmetic-in-WHERE query the cypher engine can't parse (#928). Pre-fix, `inferAuditPinchQL` for "find functions longer than 100 lines" emitted `MATCH (n:Function) WHERE (n.end_line - n.start_line) > 100 ...` which crashed with `cypher parse: unsupported operator: -` — silently-confidently-wrong on a query the agent confidently recommended. Until pinchQL supports arithmetic (tracked at #928), the template returns `start_line` + `end_line` for every Go non-test function (capped at 200) with a why-line that explains the limitation and instructs the agent to compute the diff client-side. New `TestInferAuditPinchQL_AllTemplatesParseable` regression guard scans every emitted template for forbidden arithmetic patterns so a future change can't re-introduce engine-incompatible templates.
- pinchQL DISTINCT no longer returns silently-incomplete results when the row scan exceeds `max_rows*2`. Pre-fix the SQL scan applied its safety LIMIT BEFORE the in-Go DISTINCT projection, so a query like `MATCH (n) RETURN DISTINCT n.kind ORDER BY n.kind LIMIT 100` on the pincher index returned 4 of 15 kinds (Block, Class, DataSource, Function) — the alphabetically-first 200 rows of the SQL `ORDER BY kind ASC LIMIT 200` only covered those 4 kinds, and DISTINCT in Go collapsed that subset rather than the full match set. The fix skips the safety scan LIMIT when `q.distinct` is set; in-Go DISTINCT + the post-DISTINCT LIMIT in `buildResult` bound the response size correctly. Same `!q.distinct` guard applied to both `runNodeScan` and `runJoinQuery`. Trade-off: when DISTINCT is paired with a large unfiltered scan, we now scan up to "all project symbols" instead of capping at 200 — but the scan is project_id-scoped and the DISTINCT projection collapses rows anyway, so the typical worst case is `~5K rows in memory` not `the whole symbols table`.
## [v0.56.0] — 2026-05-14 — Bedrock-layer observability + a deep dogfood haul: SSE events, request-ID correlation, stale-project reclamation, diff-encoded context, and a sweeping extractor/resolver/pinchQL bug pass

Phase 1 — release 5 of 9. The scoped half closed the v0.56 milestone: a `GET /v1/events` SSE stream so dashboards and CI bots stop polling ([#654](https://github.com/kwad77/pincher/issues/654)); `X-Request-ID` correlation threaded through HTTP, streamable-HTTP, stdio `_meta`, and logs so bedrock-layer routers can trace a request end-to-end ([#657](https://github.com/kwad77/pincher/issues/657)); `pincher project prune-stale` + `pincher vacuum` to reclaim DB space from stale-but-present projects ([#732](https://github.com/kwad77/pincher/issues/732)); diff-encoded `context` for repeat reads behind `PINCHER_DIFF_CONTEXT=1` ([#655](https://github.com/kwad77/pincher/issues/655)); a streamable-HTTP concurrent-session loadtest that caught a real gzip-vs-SSE deadlock ([#687](https://github.com/kwad77/pincher/issues/687)); auto-reindex on binary-drift respawn so a swapped binary self-heals its index ([#719](https://github.com/kwad77/pincher/issues/719)); and a closure-table at-scale measurement validating the depth-3 default ([#686](https://github.com/kwad77/pincher/issues/686)).

The other half was an exhaustive dogfood pass — roughly two dozen extractor, resolver, and pinchQL bugs of the "silent confidently wrong" class: regex/JS-AST extractors mis-spanning declaration bodies, the call/read resolvers false-binding bare names across language and package scope, and pinchQL silently dropping typo'd clauses or decimal-literal predicates. All fixed here.

No schema change — still **v25**.

### Added
- Every tool response now carries a correlation ID for end-to-end request tracing. Pincher reads an `X-Request-ID` header on HTTP/streamable-HTTP requests (minting a UUID v7 when absent or junk), echoes it on the `X-Request-ID` response header, stamps it into `_meta.request_id` on every tool response over both stdio and HTTP, and includes it in structured logs. Bedrock-layer routers (zelos, bifrost, ingress) can now map a single request from their own span through pincher and back without scraping latency or guessing. Inbound IDs are length-bounded and printable-ASCII-only so a caller-supplied value can't inject a response header or poison logs (#657).
- The MCP server now auto-heals binary-version drift: when a session opens against a project whose index was built by a different binary (the typical trigger is the supervisor's auto-restart-on-drift respawning onto a swapped binary), the server kicks off a background `index force=true` so the graph converges to the running binary's extraction rules. Previously the swap + respawn was automated but the re-index was not — `search`/`query`/`trace` silently served a stale graph (a v0.55 dogfood run saw a 3× symbol gap) until a manual `index force=true`. The existing `_meta.binary_version_warning` already signals that results may shift while the re-index converges (#719).
- `doctor` now emits an `advisories` array — failure-as-pedagogy applied to the diagnostic itself. Previously `doctor` reported `db_size_bytes` as a bare number, so a 4.7 GB store looked no different from a 4.7 MB one. When the DB crosses 1 GiB, `advisories` carries an actionable warning that names the heaviest project and spells out the remediation (`list prune_dead=true` for path-gone projects; manual re-index/removal for stale-but-present ones; a `VACUUM` to actually reclaim the freed space, since SQLite doesn't shrink the file on row deletion). The field is always present — `[]` on a healthy store — so consumers can iterate without a null check. This covers the MCP `doctor` tool; CLI `pincher doctor` parity is a follow-up.
- `pincher project prune-stale` and `pincher vacuum` close the #732 reclamation gap for stale-but-present projects. `prune-stale [--days N]` drops every project that is both schema-stale and not re-indexed in N days (default 30) — `list prune_dead=true` only handled projects whose path was gone, so a project indexed by an old binary and never touched since had no reclamation path and permanently bloated the shared DB. `vacuum` runs SQLite VACUUM so the file on disk actually shrinks afterward; it's a deliberate, explicit CLI step (VACUUM holds an exclusive lock) kept out of the hot MCP path. The `doctor` large-DB advisory now points at both commands.
- Diff-encoded `context` for repeat reads, behind `PINCHER_DIFF_CONTEXT=1` (#655). A repeat `context(id=X)` call short-circuits on the backing file's content hash: unchanged since the last fetch → `{unchanged:true, since_hash}` and the imports/callees rebuild is skipped entirely; changed → the symbol body ships as a compact line diff under `symbol.diff` instead of the full source. Default-off in v0.56 until perf validates; flag-off behaviour is unchanged.
- `GET /v1/events` Server-Sent Events endpoint (#654). Dashboards and CI bots can subscribe to `index_started`, `index_complete`, and `binary_drift` events instead of polling `/v1/health` or `/v1/index-progress`. On connect the stream sends a `binary_drift` snapshot for every project the running binary hasn't re-indexed yet, then streams live events; an optional `?project=` query filters to one project. Honors the `--http-key` bearer, advertised via the `sse` capability, and declared in the OpenAPI spec as a streaming endpoint.

### Changed
- CLI `pincher doctor` now emits the same `advisories` as the MCP `doctor` tool (#772 follow-up) — both the `--json` output (an `advisories` array, always present) and the human-readable Markdown (a `⚠ Advisories:` block under the storage summary) flag a pathologically large DB with concrete remediation. Closes the MCP/CLI inconsistency the MCP-only #772 left behind. `largeDBAdvisory` is duplicated CLI-side per the established `internal/server/admin.go` bounded-duplication convention (the CLI is package `main` and can't import the server package).
- **`doctor`'s `top` parameter description now documents that it also caps the projects list.** #575 added a `top`-bounded cap to `doctor`'s `projects` list (response-size bound on multi-project installs) but never updated the parameter description, which still claimed `top` only controlled "failures + slow queries returned per section". On a 125-project install `doctor` silently returned 10 projects with `projects_truncated: 115` and no documented knob explaining why. The description now states `top` caps all three sections (extraction failures, slow queries, projects), notes projects are sorted by symbol count desc so the largest are kept, and points at the `list` tool for full project enumeration. Found during v0.56 dogfooding.
- CI: bumped the Windows `go test` per-package timeout 300s → 600s. The process-spawn-heavy packages (`cmd/pinch`, `internal/supervisor/cmd/probe`) run real `pincher` child processes serially; under `-p 2` they contend with a concurrent package for the slow Windows runner's CPU headroom, and `cmd/pinch` tripped the 300s per-package deadline (`panic: test timed out after 5m0s`) three times in one session — each cleared by a rerun. 600s absorbs the loaded-runner tail without masking a genuine hang (a healthy run is well under a minute).

### Fixed
- **pinchQL `ORDER BY` on an aggregate column was silently ignored.** `MATCH (n:Function) RETURN n.file_path, COUNT(n) ORDER BY COUNT(n) DESC` returned an arbitrary (smallest-count-first) window instead of the top-N — the ORDER BY clause had zero effect, and `ORDER BY COUNT(n) ASC` / `DESC` / no-clause all produced byte-identical results. Root cause: the ORDER BY parser only read the bare `COUNT` token, so `q.orderBy` was `"COUNT"` while `buildResult` keys grouped rows by `aggColName` (`"COUNT(n)"`) — the sort key never matched, the comparator saw `nil` on both sides and no-op'd, and the trailing `DESC` was left unconsumed so the direction defaulted to ASC too. The parser now recognizes an aggregate-function ORDER BY target (`COUNT`/`AVG`/`MIN`/`MAX`/`SUM`), consumes the full `FN(var[.prop])` form, and builds `q.orderBy` to match `aggColName`'s output. Found during v0.55-shipped dogfooding while probing pinchQL aggregations.
- **`changes` could exceed the MCP token limit and fail by default on a change to a hot file.** A change to a central symbol (e.g. `cypher/engine.go`'s `parseQuery`) blast-radiuses into 100+ impacted symbols and 100+ `tests_to_run` entries — the full response then crossed the MCP token budget and `changes` returned an *error* instead of a result, exactly when blast-radius analysis matters most. The `impacted` and `tests_to_run` lists are now capped at 50 each (matching `neighborhood`'s #293 pagination treatment): `impacted` is sorted by risk severity first so the cap keeps the CRITICAL/HIGH entries, `tests_to_run` was already sorted by overlap descending. The `summary` block keeps the true `total_impacted` / `tests_to_run` counts, and a `_meta.warnings` entry names the trim and points at `fields=summary,tests_to_run` for the list-free view. Found while dogfooding v0.56 — a `changes scope=base:master` on a branch touching `engine.go` returned a 62 KB body that the MCP transport rejected.
- **READS edges silently dropped when a bare Go identifier collided with a config-file key name.** `main.version` (the ldflags-stamped `var version`) had zero inbound READS edges despite being read several times in `main()`. Root cause: `resolveReads`'s `lookupQN("version")` matched the *cross-language* JSON `version#Setting` symbols — `package.json`/`plugin.json` have a top-level `version` key whose qualified name is literally `"version"`, while the Go package var's QN is `main.version`. A non-empty (but wrong-language) QN match suppressed the same-language `lookupNameInLang` fallback that would have found the Go Variable, and the #436 language-mismatch guard then dropped the edge entirely. This silently affected every Go identifier sharing a name with a universal config key (`version`, `name`, `description`). Fix: `resolveReads` now falls through to the same-language name lookup whenever the QN match is empty *or* a language mismatch, keeping the cross-language QN result only when no same-language symbol exists. Found during v0.56 dogfooding.
- **`fetch` stored duplicate Document symbols for the same resource.** The Document symbol's ID was keyed on the raw input URL with no normalization, so `https://example.com`, `https://example.com/`, `HTTPS://example.com:443/`, and `https://example.com/#top` each created a distinct symbol — `search kind=Document` returned the same page two, three, or more times. `fetch` now normalizes the URL (lowercase scheme/host, strip default :80/:443 ports, supply a root path, drop the fragment) before building the symbol ID and the stored `db.Symbol` fields, collapsing equivalent spellings onto one symbol. The raw URL is still used for the actual HTTP request. Found during v0.56 dogfooding.
- **`index` MCP response disagreed with `health` after an incremental re-index.** `index` reported `IndexResult.Symbols/.Edges/.Files` raw, but those are per-run accumulators — `.Symbols`/`.Files` count only files reprocessed this run, and `.Edges` is further inflated because `resolveImports`/`resolveCalls`/`resolveReads` rebuild the entire cross-file edge set every run and add their whole-project count to the running total. Observed on a 13-reprocessed / 350-skipped incremental run: `index` returned `symbols: 0, edges: 12275` while `health` reported the true graph (`symbols: 5126, edges: 15032`). The CLI json/text path already pulled totals from `GraphStats` (the `cmd/pinch/main.go` comment even calls out "IndexResult only has delta counts for this run") — only the MCP `handleIndex` handler still leaked the raw per-run fields. `index` now reports DB graph totals for `symbols`/`edges`/`files` and adds a `reprocessed` field for the per-run file count; the zero-symbol diagnosis still keys on the per-run struct fields. Found during v0.56 dogfooding.
- **`search query="*"` leaked a raw SQLite error.** A stem-less prefix wildcard (`*`, `**`) — the natural "search for everything" instinct — fell through `sanitizeFTS5Query` unchanged (the sanitizer strips the trailing `*`, sees an empty core, and returns the token as-is), and FTS5 rejected the bare `*` with `SQL logic error: unknown special query`. `handleSearch` already pre-flights empty queries, unbalanced quotes, and regex meta-patterns; it now also catches a query that is nothing but `*` characters and returns a friendly error explaining that prefix wildcards need a stem (`auth*`), redirecting to the `query` tool (`MATCH (n) RETURN n.name LIMIT 50`) for an actual list-all. Stemmed wildcards are unaffected. Found during v0.56 dogfooding.
- **`dead_code` echoed `filters.kinds` as JSON `null` when no `kinds` arg was passed.** The handler built `kinds` as `var kinds []string` (nil) and put it straight into the `filters` echo block — a nil slice marshals to `null`, so a consumer iterating `filters.kinds` without a null-check breaks. `filters.language` already defaults to `""` (not null), so the echo block was internally inconsistent too. `kinds` is now allocated as `[]string{}`; `GetDeadCode` keys on `len(kinds) == 0` so the default-kinds behaviour is unchanged. This is the recurring nil-slice-in-response class the repo's JSON invariants call out. Found during v0.56 dogfooding.
- **`changes` `changed_symbols` list was uncapped.** #730 capped `impacted` and `tests_to_run` at 50 to keep the response inside MCP token budgets, but missed `changed_symbols` — on a wide rename, or `scope=all` over a tree with many untracked multi-symbol files, that list grew unbounded and reopened the same response-bloat problem (a `scope=all` run over 8 untracked planning docs returned 83 `changed_symbols` entries, ~5.8K tokens). `changed_symbols` now gets the same treatment: sorted by `(file_path, id)` so the trim is deterministic, capped at `changesMaxList`, with the true count preserved in `summary.changed_symbols` and the trim surfaced in `_meta.warnings`. Found during v0.56 dogfooding.
- **pinchQL: a node label that's a valid kind but the wrong one for the named symbol no longer returns a silent zero.** `MATCH (n:Function) WHERE n.name = "handleSearch" RETURN n.name` returned 0 rows with no diagnostic — `handleSearch` is a `Method`, not a `Function`, and `:Function` is an exact `kind='Function'` match. `:Function` is a valid kind value (so the #501 enum-value check stayed quiet) and `name` is a valid property (so the #473 property check stayed quiet), leaving the agent to read the empty result as "no such symbol." The engine now emits a warning when a labelled node pattern with a `name = "literal"` WHERE predicate yields zero rows and a symbol with that name exists under a different kind: `node label "Function" matched 0 nodes named "handleSearch" — a symbol with that name exists with kind Method. Use the matching label, or drop the label to match any kind.` Same failure-as-pedagogy shape as #473/#501. Found during v0.56 dogfooding.
- **pinchQL `project=*` cross-project queries returned rows with no project attribution.** A cross-project query (`MATCH (n:Function) WHERE n.name = "main" RETURN n.file_path` with `project=*`) returned rows from every indexed repo, but `file_path` is project-relative and collides across repos — and pinchQL had no `project` property to RETURN, so the results were fundamentally un-attributable. `project_id` (alias `project`) is now an addressable node property: `RETURN n.project_id` disambiguates which repo each row came from, and `WHERE n.project = "repoA"` filters by it. The column was already scanned into `symRow` on all three query paths (node-scan / join / BFS) — it just wasn't exposed in `cypherPropToCol` / `symRowToMap` / the known-property list. Found during v0.56 dogfooding.
- **pinchQL parser silently skipped unknown tokens — a typo'd clause keyword dropped the whole clause.** `MATCH (n:Function) WERE n.name = "Open" RETURN n.name` (typo: `WERE` for `WHERE`) returned every function in the project instead of erroring: `parseQuery`'s top-level loop ended with `default: p.next() // skip unknown tokens`, so the malformed `WERE n.name = "Open"` was consumed token-by-token and discarded, degrading the query to `MATCH (n:Function) RETURN n.name`. The agent typo'd one keyword and got a confidently-wrong full-table result that reads as real data — worse than #473, where the typo'd property at least kept the WHERE clause (just falsy). The `default` case now returns a parse error naming the unexpected token, with a Levenshtein-≤1 "did you mean WHERE?" hint when the token is a keyword near-miss. Every top-level token in pinchQL's grammar is a clause keyword, so anything else at clause position is unambiguously malformed. Found during v0.56 dogfooding.
- **Call/binding resolution bound false-positive edges from real project code into isolated `testdata/` fixture corpora.** A `query` probe found `cmd/pinch/doctor.go::runDoctorCLI` carrying a confidence-0.4 CALLS edge to `testdata/corpus/go-project/internal/auth/auth.go::auth.Open` — `runDoctorCLI` only calls the real `db.Open`. The name-fallback resolution paths (`resolveCalls`'s `lookupName`, `resolveReads`'s `lookupNameInLang`/binding-pass, and `resolveMethodByName`) looked up symbols by bare name across the whole project and could pick a `testdata/`-corpus symbol — those are isolated mini-projects, never real call targets from outside their tree, exactly as `dead_code` already post-filters them. All three name-fallback paths now drop fixture-corpus candidates (`testdata/`, `__fixtures__/`, `test-fixtures/`, etc.) before choosing a target, keeping them only when *every* candidate is a fixture so intra-fixture resolution still works. Pinned-corpus snapshots are unaffected — when a corpus is indexed as its own project root its paths are corpus-relative, so the fixture-path check is a no-op there. Found during v0.56 dogfooding.
- **pinchQL silently ignored WHERE predicates with a decimal literal.** `MATCH (a)-[r:CALLS]->(b) WHERE r.confidence < 0.5 RETURN ...` returned zero rows with the misleading warning *"column-vs-column comparison `r.confidence < 0.5` is not supported"* — `0.5` is a literal, not a column. The tokenizer's number scanner consumed digits only, so `0.5` tokenized as `NUMBER(0)` `PUNCT(.)` `NUMBER(5)`; the WHERE parser then read `< 0.5` as a column reference `0.5`, the `collectCrossColumnWarnings` check flagged it, and the predicate was dropped (evaluated to false). The scanner now consumes a single decimal point plus fractional digits when a digit actually follows — so `0.5` is one `NUMBER` token, `1..3` (hop-range) and a trailing `.` stay separate. Decimal-literal comparisons (`confidence`, `extraction_confidence`, any numeric column) now filter correctly. Found during v0.56 dogfooding.
- **Bare-name call resolution mis-bound to same-named methods, starving the real function.** `trace name=Extract` on `ast.Extract` — the package-level extraction entry point called by ~40 test functions — returned **zero inbound callers**. Every bare `Extract(...)` call had resolved to `bashExtractor.Extract#Method` instead: `resolveCalls`'s bare-name fallback (`lookupName`) ran `pickCanonical` over the mixed candidate set (one `func Extract` + several `(x).Extract` methods) and picked the lexicographically-smallest symbol ID, which was a method. A bare `Foo()` call in Go is syntactically never a method invocation — methods require a receiver. `lookupName` now drops Method-kind candidates entirely (`excludeMethodSyms`); if that empties the set the call is left unresolved, which is correct — a false bind to an arbitrary same-named method is worse. The `resolveReads` binding-pass (`lookupNameInLang`) had a related failure mode — a same-named function-and-method pair could bind the 0.4 binding-pass CALLS edge onto the method — and now applies a Variable > Function > Method priority, keeping Method only as the last resort that #565's `w.doFn = w.defaultDo` method-value binding depends on. Found during v0.56 dogfooding.
- **The #326 tail-pass GC couldn't prune orphan symbols whose `files` row was missing.** A `symbol` probe found `internal/cypher/zz_dbg_test.go::TestDebug593` — a long-deleted scratch file, never in git history — still fully indexed and queryable, with `symbol` returning an empty `source` (stale byte offsets pointing past a file that no longer exists). The #326 GC iterated only the `files` table (`ListFilesForProject`), so symbols whose `files` row was never written — a crash between `flushBatch` (writes symbols) and `SetFileHash` (writes the files row) — stayed orphaned forever, invisible to the GC. This is the exact shape of the original #326 report ("paperclip: 4820 orphan symbols, 0 files"), which #326 only partially closed. The GC now iterates the **union** of the `files` table and the distinct file paths in `symbols` (`ListSymbolFilePaths`), so any path with orphan symbols is reconsidered and pruned when it's gone from disk. The per-file deletes are all idempotent, so reconsidering a path with symbols-but-no-file_hash (or vice versa) is safe. Found during v0.56 dogfooding.
- **A selector's trailing component (`strings.Index` → `Index`) false-bound to a same-named project Method.** `trace resolveCalls` surfaced bogus CALLS edges like `resolveCalls → *Indexer.Index#Method` and `resolveCalls → bashExtractor.Confidence#Method`. Two seams leaked the same shape. (1) `resolveByReceiverType` (#423 resolve_pass) rebuilt the target QN from the *enclosing* method's receiver type without verifying `segments[0]` was the receiver variable — so `strings.Index(...)` inside `func (b *box) scan()` built `box.*box.Index` and matched the real `Index` method. (2) `extractGoReads`'s `walkRead` flattened every `Ident` under an expression with `ast.Inspect`, including the `.Sel` of a `SelectorExpr` that was a call subject — `strings.Index(...)` emitted a bare read of `Index`, which the binding pass (#565) then bound to the project Method. Fix: `resolveByReceiverType` now applies the `isStdlibReceiver` stoplist that the #410 receiver-method fallback already uses; `walkRead` is CallExpr-aware and walks only the receiver side of a selector-call plus its args, leaving the call subject to `extractGoCalls`. Non-call selectors (`w.defaultDo` as a function value) still emit `.Sel`, so #565 bindings are unaffected. The complete fix threads the receiver *variable name* through `ExtractedEdge` (a schema migration) — deferred. Found during v0.56 dogfooding.
- **`resolveCalls` could resolve a Go call to a config-file key.** A bare Go call name (`build`, `test`, `run`) can collide with a config-file key whose qualified name is literally that string — npm script keys and top-level YAML/JSON keys all extract as `Setting` with `QN = key name`. `resolveCalls`'s `lookupQN`/`lookupName` had no kind or language filter, so `pickCanonical` (lexicographically-smallest ID) could resolve a cross-file Go call to the `Setting`, emitting a false `Function → Setting` CALLS edge and starving the real function of its inbound edge. Unlike `resolveReads` (which has the #436 language guard, extended by #731), `resolveCalls` had no such guard at all. Fix: a new `excludeNonCodeSyms` filter drops `Setting`/`Section`/`Document`/`Resource`/`Output`/`Local`/`Provider`/`Block`/`DataSource` candidates from both the QN and bare-name lookups — a CALLS-edge participant is always a code symbol, so when the filter empties the candidate set the lookup correctly returns unresolved rather than false-binding. Found during v0.56 dogfooding (while fixing #731).
- **`resolveReads` bound bare identifiers project-wide, ignoring Go package scope.** After #731 unmasked it, `main.version` collected 37 inbound READS edges — only ~7 (from `cmd/pinch/*`, the real `package main`) correct; the rest were functions reading their *own* local or parameter `version`. The bare-name fallback resolved any unqualified identifier to any same-named project symbol, and `version` / `name` / `result` are universal local names. Two tangled causes, fixed together: (1) `extractGoReads` flattened `db.CorpusCode` into bare `db` + `CorpusCode` reads, so the resolver couldn't distinguish a legitimate cross-package selector read from a truly-bare same-package identifier — it now threads the file's imported-package identifiers through and emits the *qualified* `db.CorpusCode` for package selectors (non-package selectors like `w.defaultDo` still emit bare, so #565 function-value bindings are unaffected); (2) `resolveReads`' bare-name fallback is now scoped to the reader's Go package, approximated by source-file directory — a bare unqualified read can only ever bind same-package. The same qualified-emit rule was applied to `extractGoFileLevelReads` (#576 file-level var-init reads) for consistency. Found during v0.56 dogfooding, verifying #731 live.
- **Fetched `Document` symbols had sloppy fields — empty search snippet, doubled `symbol` payload, redundant signature.** `search kind:Document` returned `"snippet": ""` because the snippet path byte-seeks an on-disk file, but a fetched Document has no file — its text lives in `Docstring`. That broke the "skip a follow-up call" contract `search` advertises. `symbol` on a Document echoed the full text in *both* `source` and `docstring` (identical bytes, ~2× payload). And `Signature` was a third verbatim copy of the URL (already `file_path` and `qualified_name`). Fixes: `search` now sources the snippet from `Docstring` for Document-kind hits; `symbol`/`symbols` blank `docstring` for Documents (the text is the `source`); `handleFetch` stores the page title as `Signature` so `search` results get a meaningful one-line label. `QualifiedName` stays the normalized URL — it's the stable per-URL key the #733 re-fetch dedup depends on. Found during v0.56 dogfooding, probing the `fetch` tool.
- **`guide` didn't recognize the most technical way to describe dead code.** `guide task="which methods have no inbound callers"` returned `shape: "unknown"` and recommended a useless literal `search` — when it should route to the purpose-built `dead_code` tool (as `"find unreachable dead code"` already does). `classifyTaskShape`'s dead-code trigger list had `"no callers"`, but `strings.Contains("no inbound callers", "no callers")` is false — `inbound` sits between the words, and "no inbound callers/edges" is exactly the language `dead_code`'s own docs use. Added `"no inbound caller"`, `"no inbound edge"`, `"nothing calls"`, and `"never used"` to the dead-code shape triggers. Found during v0.56 dogfooding, probing `guide`.
- **pinchQL's documented property aliases (`project`, `qn`, `label`) returned `null` in `RETURN`.** `knownPropertyList` advertises `project_id (project)`, `qualified_name (qn)`, `kind (label)`, and the aliases worked in `WHERE` (translated by `cypherPropToCol`'s SQL pushdown) — but `symRowToMap` only populated the *canonical* keys, so `RETURN n.project` / `n.qn` / `n.label` hit a missing map key and silently resolved to null. Most visible on a cross-project (`project='*'`) query: rows came back from many repos with `n.project` null, leaving no way to attribute a row to its repo unless you knew the canonical name `project_id`. `symRowToMap` now carries the alias keys alongside the canonical ones, so the aliases resolve everywhere — RETURN projection and the in-Go `matchesWhere` fallback, not just SQL pushdown. Found during v0.56 dogfooding.
- **pinchQL string literals weren't unescaped, so exact-match `WHERE` on a Windows file path silently matched nothing.** The tokenizer's scan loop already treated backslash as an escape — it skips the char after it so an escaped quote does not terminate the literal early — but the token value kept the raw backslashes. A Windows path literal then compared as its double-backslash form against the single-backslash stored value and never matched, so `WHERE n.project_id = ...` / `n.file_path = ...` / `n.id = ...` returned zero rows on every Windows-indexed project. The new `unescapeString` completes the escape the scanner already committed to (quote, backslash, and control-char escapes); an unrecognised escape keeps the backslash verbatim so regex predicates still work. Found during v0.56 dogfooding (#779).
- **`pincher init` injected an LF policy block into a CRLF file, leaving mixed line endings.** On a Windows repo whose `CLAUDE.md` (or other target file) uses CRLF, `init` kept the original content's CRLF endings but wrote the `<!-- pincher:start -->...<!-- pincher:end -->` block with bare LF, and the append-path `TrimRight` left a dangling lone CR. `MergePolicyBlockBare` now normalizes `existing` to LF for the merge logic, remembers whether the original used CRLF, and restores that convention on output — CRLF-input files come out uniformly CRLF, LF-origin files stay LF. Found during v0.56 dogfooding (#778).
- pinchQL now rejects a `WHERE` clause after `RETURN` (HAVING-style aggregate filtering) instead of silently folding it into the pre-aggregation node filter — `RETURN count(*) AS c WHERE c > 40` previously returned every group unfiltered (#780).
- `guide` now routes "find every function with no callers"-style tasks to the `dead_code` tool instead of the hardcoded find-undocumented pinchQL query — `classifyTaskShape` evaluated `auditShapePattern` before the `shapeDeadCode` keyword case, so any caller/coverage audit got the wrong recommendation (#782).
- `guide` now classifies "refactor the error handling …"-style tasks as refactor work instead of bug-fix work — `shapeFix`'s keyword list matched the noun `error` and ran before `shapeRefactor`. The `extract` refactor verb is also word-bounded so it no longer matches the nouns `extraction`/`extractor` (#784).
- `search` now detects a slash-delimited regex literal (`/handle[A-Z]w+/`) in the `#509` preflight and returns the friendly redirect to the `query` tool's `=~` operator, instead of letting the `#424` sanitizer mangle the meta-chars into a zero-result query with a misleading "lower min_confidence" diagnosis (#786).
- `search`'s `#786` regex redirect now strips the surrounding `/` delimiters before embedding the query in the `=~` example — pinchQL's `=~` takes a bare regex, so the redirect previously recommended `=~ '/handle.*/'`, which matches a literal slash and returns zero rows (#788).
- The MCP `index` tool now refuses the catastrophic index targets (filesystem root, `$HOME`) — the bloat-trap guard previously lived only in the `pincher index` CLI path, so an MCP `index` call could walk `/` or `$HOME` and bloat the shared DB. `IsBloatTrap` moved to `internal/index` so both surfaces share it (#790).
- pinchQL inline brace props (`MATCH (n:Kind {prop: val})`) now apply consistently across all three runners. `runJoinQuery` and `runBFS` previously dropped them entirely — `(a)-[:CALLS]->(b:Function {name:"X"})` ignored the `{name:"X"}` predicate and returned callers of every Function — and `runNodeScan` bound bool literals verbatim so `{is_exported: true}` matched zero rows (#792).
- pinchQL now rejects `RETURN *` with a clear error instead of returning a garbage `{"": null}` row. The tokenizer reads `*` as an empty variable-length-path token, which `parseReturn` previously accepted as a return variable with an empty name (#794).
- `pincher <unknown-subcommand>` now errors with the usage banner and exits 1 instead of silently falling through to the MCP stdio server (which read EOF on a non-tty stdin and exited 0, making a typo'd subcommand look like it succeeded) (#796).
- CLI subcommands now honor flags placed after a positional argument (`pincher project rm NAME --force`, `pincher index PATH --force`). Go's stdlib `flag.Parse` stops at the first non-flag token, so a trailing flag was silently dropped — a shared interspersed-parse helper now collects flags from anywhere in the arg list (#798).
- `Store.DeleteProject` (and `pincher project rm`) now clears every per-project table. It previously cleaned only 5 of ~11 — `closure` and `extraction_failures` carry a `projects` FK, so the delete hard-failed with `FOREIGN KEY constraint failed` for any project with extraction failures or a closure table, and the unconstrained tables (`pending_edges`, `struct_fields`, `interface_methods`, `symbol_moves`, `slow_queries`) leaked orphan rows on every delete (#799).
- `pincher project rm --json` now emits a structured JSON error object on every failure path (no match, ambiguous substring, store errors) instead of plain text — previously only the success path honored `--json`, so a scripted caller got unparseable output on exactly the cases it needed to branch on (#801).
- `pincher init --dry-run` now prints a grammatical "would write/update/append" line — it previously concatenated `"would "` with the past-tense plan action, yielding "would wrote" (#803).
- The Ruby extractor now spans each `def`/`class` to its own `end` keyword. It previously used the `blockChar=0` path, which falls through to the Python "just return 80 lines" heuristic — so every Ruby symbol got an 80-line span clamped to EOF, and `symbol`/`context` returned wildly wrong source for Ruby methods (#805).
- The Python (and other indentation-delimited regex-tier) extractor now spans each `def`/`class` to the first line dedented to its opening indent. It previously used the `blockChar=0` path, which was a literal "just return 80 lines worth of bytes" heuristic — so every Python symbol got an 80-line span clamped to EOF, and `symbol`/`context` returned wildly wrong source (#807).
- Brace-delimited regex-tier extractors (Kotlin, Swift, C#, Java, Rust, PHP, C/C++) no longer let an expression-bodied or body-less declaration swallow the next sibling's braced block. The brace matcher previously scanned forward for the first `{` anywhere ahead with no upper bound, so `fun double(x) = x * 2` ran to the end of the following function — `symbol`/`context` returned both functions' source. The opener search is now bounded to the declaration line (and its parameter-paren continuations). The JS AST extractor's `locateMethodInRange` is also fixed: its leading-whitespace match used `s` (which spans newlines), so a blank line before a method landed `startByte` on a stray newline and the now-bounded brace matcher returned a zero-width span (#809).
- Regex-tier `Class` symbols (Java, PHP, C#, Swift, Rust, Kotlin, Python, TypeScript) no longer report their own name as `parent` when the class has no superclass. The `extractGroup` helper ignored its `name` argument and returned the first non-empty positional capture group, so asking for `parent` fell through to the `name` group whenever the `extends`/`:` clause was absent. Replaced with `namedGroup`, which resolves capture groups by name (#811).
- The PHP regex extractor now extracts indented class methods. `phpRE.funcRE` and `phpRE.classRE` were anchored `^` with no leading whitespace class, so any `function`/`class` not at column 0 — i.e. every method inside a class — was silently dropped. Both patterns now lead with `^s*` (#813).
- The HTTP dashboard now renders project symbol counts correctly. `db.Project.SymCount` marshals to JSON as `symbol_count`, but the dashboard JS read it as `p.SymCount` / `p.sym_count` — neither key matched, so every project tile showed "0 symbols" regardless of the real count. Any project that also had zero edges (common for non-Go repos, which get per-file extraction with no cross-file edges) then tripped the `isEmpty` heuristic and was mislabelled "no data — may be a ghost project" — a zelosMCP user hit exactly this on a repo with 2040 indexed symbols. The same mismatch zeroed the symbol column in CSV/JSON dashboard exports and broke the "remove N empty projects" count. All five read sites now use the canonical `symbol_count` key, and a regression test pins the JS to whatever key `db.Project` actually marshals to (#815).
- A Rust function whose `where` clause or wrapped `-> Type` return type sits on a line after the `(params)` close is now spanned correctly. `findBraceBlock` treated any newline at paren depth 0 as end-of-declaration, so `pub fn f<T>(x: T) -> T` followed by `where T: Clone,` on the next line got a one-line span — `symbol`/`context` returned just the signature, missing the entire body. The newline at paren depth 0 now peeks the next non-blank line: a leading `{`, `->`, or `where` keyword means the declaration continues, and a `where` clause holds the scan open across its own multi-line body until the braced body opens. A `;` at paren depth 0 still ends a bodyless declaration (trait method, abstract decl). This is the residual of #809 (#816).
- Swift and C# `Class` symbols no longer carry a trailing space in their `parent` field. The `swiftRE`/`csRE` `classRE` parent capture groups use a `[..., ]*` char class (to allow multi-inheritance lists), so the `*` greedily ate the trailing space before `{` — yielding `parent="Drawable "` instead of `"Drawable"`. The shared regex `extract()` loop now `TrimSpace`s the parent capture (#817).
- Members declared inside a Java/C# `interface` are now scoped to it — extracted as `Kind=Method` with `Parent=<interface>` instead of as top-level `Function` with `Parent=""`. The regex `extract()` loop's Interface case never set `currentClass`/`currentClassEnd` the way the Class case does, so interface member lines fell through to the unscoped Function path (#819).
- Java methods are no longer reported `is_exported=false` just for being lowercase. `extractJava`'s `exportedFn` used Go's exported-by-capitalization rule, but Java visibility is keyword-based and Java methods are conventionally lowercase — so ~every Java method was mislabeled not-exported, which made `dead_code` flag them all. Java now matches the other regex-tier extractors (PHP/Rust/C#/Kotlin/Swift), conservatively treating symbols as exported (#820).
- Java methods with generic or array return types (`List<String> getNames()`, `int[] getArray()`) are now extracted. `javaRE.funcRE`'s return-type token was `(?:w+s+)+`, which can't match a `List<String>`/`int[]` run because the `<...>`/`[]` breaks the bare-word sequence before any whitespace — so those methods were silently dropped. The token now allows an optional generic arg list and array brackets (#823).
- JS AST variable declarations whose initializer continues onto the next line via a method chain (`const x = items` then `.filter(...)` / `.map(...)` on following lines) now span the whole statement. `findStatementEnd` returned at the first newline at bracket-depth 0, but JS ASI does not insert a semicolon when the next line starts with a continuation token — so the span was truncated to the first line. It now keeps scanning when the next non-blank line starts with `.`, `?`, or `:` (#825).
- The JS AST extractor's `locateFunc` no longer mis-spans a function declaration whose `function` keyword sits on its own line. The byte-location regex used a newline-spanning whitespace class between `function`, the name, and the open paren — so a `function` keyword on its own line landed `startByte` on the keyword and `findBraceBlock` handed back an ~8-byte span instead of the whole body. Inter-token whitespace is now restricted to spaces and tabs, matching the #809 `locateMethodInRange` fix; the rare genuine `function`-on-its-own-line case is skipped rather than mis-spanned, since a dropped symbol is safer than confidently-wrong byte offsets (#826).
- The watcher now re-indexes after a pure file deletion. `changedFiles` compared only file mtimes against `project.IndexedAt`, and a deleted file has no mtime to compare — so a deletion with no other change was invisible, the watcher never triggered `Index()`, and the deleted file's symbols/edges stayed orphaned in the DB (still returned by `search`/`query`) until some unrelated edit. `changedFiles` now also diffs the walked source set against the `files` table; any indexed path no longer on disk counts as a change, which triggers the re-index whose tail-GC prunes the orphan (#828).
- `pincher index --data-dir <missing-dir>` now creates the directory instead of failing with a misleading `failed to open database: ... out of memory (14)`. `db.DataDir()` already `MkdirAll`s the default and `PINCHER_DATA_DIR` paths, but `db.Open` never created the dir it was handed — so a `--data-dir` flag pointing at a not-yet-existing directory hit a `SQLITE_CANTOPEN` that modernc.org/sqlite surfaces as "out of memory (14)". `db.Open` now `MkdirAll`s the data dir, making all three data-dir sources consistent (#830).
- `pincher doctor --json` now emits `[]` for `slow_queries` and `extraction_failures` when there are none, instead of `null`. Both fields in `buildDoctorReport` were append-only and started as the nil zero value, so a clean database produced `null` — breaking JSON consumers that iterate without a null-check. They are now initialised to empty slices alongside the already-correct `advisories` field (#832).
- `pincher doctor --json` now emits `[]` for `projects` when there are none, instead of `null`. `DoctorReport.Projects` is append-only and started from the nil zero value, so a clean install with no indexed projects produced `"projects": null` — the same JSON-slice invariant violation #832 fixed for `extraction_failures` and `slow_queries`, but `projects` was missed in that pass. It is now initialised to an empty slice alongside them (#837).
- `trace` no longer silently returns zero hops when given a non-canonical `direction` value. The DB-layer traversal only branches on `inbound`/`outbound`/`both`, so passing `direction=callers` or `direction=callees` — the obvious words, primed by the tool's own "find callers (inbound)" description — fell through every branch and produced `{"hops":[],"total":0}`, byte-identical to a genuine "this symbol has no callers" result. `callers`/`callees` now map to `inbound`/`outbound` with a `_meta.warnings` entry naming the canonical term, and any other unrecognised value falls back to `both` with a warning — the same failure-as-pedagogy shape `trace` already applies to unknown edge `kinds` and out-of-range `depth` (#839).
- The `fetch` tool now closes a DNS-rebinding TOCTOU window in its SSRF protection. `validateFetchURL` resolved the host and checked the IPs against the block-list, but the HTTP transport then did its own independent DNS lookup when dialing — so a host whose DNS the attacker controls could answer the validation lookup with a public IP and the dial lookup with `127.0.0.1`, `169.254.169.254`, or an RFC1918 address. A `net.Dialer.Control` hook (`fetchDialControl`) now runs after resolution, immediately before `connect(2)`, with the literal `ip:port` being dialed — the IP that is checked is the IP that is connected to, regardless of how many lookups happen. `validateFetchURL` and the per-redirect `CheckRedirect` re-validation stay as the fast pre-flight; the dial-control hook is the belt-and-suspenders. The custom transport also adds explicit TLS-handshake and response-header timeouts (#843).
- The pinchQL parser now rejects unbalanced delimiters instead of silently running the query. The parser's `skip()` helper is lenient — it no-ops when the token doesn't match, which is correct for genuinely optional tokens (`BY`, `WITH`) but was also used for structural closers. A query missing a `)`, `]`, or `}` — e.g. `MATCH (n:Function WHERE n.complexity > 50 RETURN n.name` — parsed and ran identically to the well-formed query, while a clause-keyword typo like `WHRE` was already caught with a clear error. A new strict `expect()` helper now guards every closer that pairs with a consumed opener: node/to-node `)`, edge `]`, inline-props `}`, and `RETURN`/`ORDER BY` aggregate-function `)`. `parseProps` gained an error return so a missing `}` can no longer let the inline-props block swallow the rest of the query (#845).
- `query` with `ORDER BY` now sorts the full match set instead of an arbitrary sample. Both the node-scan and JOIN paths applied the `max_rows` safety cap as a SQL `LIMIT` with no `ORDER BY` in the `SELECT`, then sorted the truncated rows in Go — so `ORDER BY complexity DESC LIMIT 3` returned the top 3 of a random `max_rows`-sized slice, not the global top 3. On the pincher-repo self-index (~2200 functions, default `max_rows` 200) the three genuinely most-complex functions never appeared in the result. `ORDER BY` on a bare `var.property` is now pushed into the SQL `SELECT` (with the correct `a.`/`b.` alias for JOIN queries) so the database sorts the full set before the cap truncates it; aggregate `ORDER BY` (e.g. `COUNT(n)`) and bare var-less `ORDER BY` on JOINs are unaffected (still handled post-scan). The variable-length BFS path was checked and does not have the cap-before-sort pattern. Tests missed this because fixture datasets are smaller than `max_rows`, making cap-before-sort and cap-after-sort identical (#847).
- The `init` MCP tool's dry-run `action` field is now grammatical. It was built as `"would_" + plan.Action`, and `plan.Action` is past tense — so a dry-run reported `would_updated` / `would_wrote` / `would_appended`. #803 fixed the same construction for the `pincher init --dry-run` CLI text but the MCP handler drifted back to the ungrammatical form. The present-tense mapping now lives in the `pinit` package as `PresentTenseAction`, shared by the CLI text path and the MCP JSON `action` field, so the dry-run reports `would_update` / `would_write` / `would_append` consistently across both surfaces (#849).
- `dead_code` now warns when the `kinds` filter contains an unrecognised symbol kind instead of silently matching nothing. A typo'd kind — e.g. `kinds=Funktion,Method` — was split, passed straight to the query, matched zero rows, and echoed back verbatim in `filters.kinds` with no flag, so the caller couldn't tell "Funktion isn't a kind" from "those kinds are all clean". Each `kinds` entry is now validated against the symbol-kind taxonomy actually present in the project (the `kindCounts` map `GraphStats` returns); unrecognised kinds are dropped from the query with a `_meta.warnings` entry listing the valid kinds, mirroring the edge-kind validation `trace` already does. Recognised kinds in the same list are unaffected (#851).
- The streamable-HTTP MCP transport (`--mcp-http-path`) was mounted after the HTTP gateway's gzip-compression wrap, so its long-lived `text/event-stream` was routed through a buffering `gzipResponseWriter` that doesn't implement `http.Flusher` — every SSE event was stranded in the gzip buffer and concurrent MCP sessions hung. The MCP path is now routed before the gzip wrap (auth, rate-limiting, and basepath-strip still apply). Surfaced by the new #687 concurrent-session loadtest, which also confirms no response interleaving and no goroutine leaks across N=10–100 simultaneous sessions.
## [v0.55.0] — 2026-05-13 — CI hardening + exhaustive dogfood pass: failure-as-pedagogy everywhere, AFK-safe self-healing loop

Phase 1 — release 4 of 9. Scoped as CI hardening (umbrella [#681](https://github.com/kwad77/pincher/issues/681): Windows flake fixes, bench-regression noise removal, CHANGELOG-conflict elimination, workflow-isolation lint, workflow ergonomics) — then grew a second half. An exhaustive dogfood pass over the entire MCP + HTTP surface turned up roughly a dozen failure-path and accuracy bugs, all fixed here:

- **Failure-as-pedagogy, finished.** Every arg-validation, not-found, and silent-coercion path across `search`/`query`/`trace`/`symbol`/`symbols`/`context`/`neighborhood`/`guide`/`fetch`/`adr`/`index` now returns the rich `_meta.next_steps` envelope or a `_meta.warnings` advisory instead of a bare string or a silent coerce. The v0.17 "audit every empty-response path" throughline is now complete for the input-rejection class.
- **A ~2500× perf fix.** Variable-length pinchQL BFS was mis-planning its `JOIN symbols` against a recursive CTE — `bfsViaCTE` now pre-aggregates + `CROSS JOIN`s, taking a depth-2 `query` from 1.3s to 0.5ms on the dogfood corpus.
- **AFK-safe self-healing loop.** `make install` + `scripts/swap-active-binary.sh` + `scripts/build.sh` make the build→swap→auto-restart loop work on Windows without a manual `/mcp`; orphaned pincher processes now reap themselves on parent death and can no longer stomp shared project metadata ([#724](https://github.com/kwad77/pincher/issues/724)).

No schema change — still **v25**.

### Added
- **CHANGELOG stub-files convention ([#681](https://github.com/kwad77/pincher/issues/681) Bucket C).** Per-PR `CHANGELOG.d/<num>.<type>.md` stubs replace the "edit `[Unreleased]` directly" pattern that produced merge conflicts on every concurrent-PR pair (~5 conflicts in the v0.53/v0.54 cycles alone). New `scripts/changelog-assemble.sh` collects stubs into `CHANGELOG.md` at release-prep time (`--apply` rewrites + clears stubs); new `scripts/changelog-stub-check.sh` CI gate fails any code-touching PR without a stub (doc-only PRs exempt). Pattern adapted from `towncrier` but pure-bash + zero-dependency. Legacy "edit [Unreleased] directly" path still works — assembler is additive, not exclusive. Fourth v0.55 deliverable.
- **`gh-rerun-queue.sh` — automated CI rerun-when-rerunnable wrapper ([#681](https://github.com/kwad77/pincher/issues/681) Bucket E2).** Pain point this fixes: `gh run rerun N --failed` returns `run N cannot be rerun; This workflow is already running` if invoked while sibling jobs (including `continue-on-error: true` advisory ones) haven't reached terminal state yet. Standard advice is "just wait and click Re-run failed" — puts burden on the operator. New `scripts/gh-rerun-queue.sh <run-id> [--failed|--all]` polls every 30s up to 30 min for the run to enter rerunnable state, then triggers rerun. Catches the v0.53 PR #679 pattern where back-to-back rerun attempts were rate-limited by sibling-job state. Sixth v0.55 deliverable.
- **Stacked-PR base-retargeting helper ([#681](https://github.com/kwad77/pincher/issues/681) Bucket E1).** New `scripts/gh-pr-retarget-orphans.sh` lists open PRs whose base ref was deleted from origin (the failure shape after merging the bottom of a stack with `--delete-branch`) and retargets them to `master` (or `--base <branch>`) on `--apply`. Dry-run prints the orphan list + plan; apply mode shells out to `gh pr edit --base`. Operator still drives the per-orphan rebase/force-push since conflict resolution needs human judgement. Pairs with the v0.55 `gh-rerun-queue.sh` (Bucket E2) — completes the workflow-ergonomics half of #681.
- **Inline-divergence detector ([#690](https://github.com/kwad77/pincher/issues/690) Bucket 2).** `cmd/workflow-lint` learns a second rule: flag any workflow `run:` block that fingerprint-matches `scripts/release-channel.sh` (modulo-10 stable rule + beta/alpha/rc pre-release routing) WITHOUT shelling out to the canonical script. Catches the v0.54.0-beta.1 bug shape preemptively — release.yml had inline channel-detection that drifted from the canonical script, mis-labelling the beta tag as "dev" until #689 made release.yml shell out properly. Rule fires regardless of checkout state (divergence is always a bug). All-fingerprints-must-match heuristic prevents single-pattern false positives (e.g., unrelated `% 10` arithmetic). New entries get added to the `canonicalScripts` slice as we discover more divergence-prone scripts.
- **`make install` + `scripts/swap-active-binary.sh` — autonomous dogfood loop ([#705](https://github.com/kwad77/pincher/issues/705)).** On Windows, replacing the running pincher binary fails with `Device or resource busy` because the OS holds an exclusive file lock on every running .exe. The previously-documented dogfood workflow (`auto_restart_workflow_proven` memory) said "build → cp to active path → next tool call auto-restarts" — only worked on macOS/Linux. New `scripts/swap-active-binary.sh` uses the rename-out trick: `mv $TARGET $TARGET.old` (running process keeps its handle to the old inode) + `cp source $TARGET`. Supervisor's `PINCHER_AUTO_RESTART_ON_DRIFT=1` picks up the swap on the next MCP tool call. New `make install` target chains `build` + the swap script so the developer types one command and the live MCP session sees the new code without any `/mcp` restart. Per session feedback `user_cannot_restart_mcp`: when the user is AFK, manual restart isn't an option — the loop must be fully automatic. POSIX path also covered (cp over live binary is safe there via standard open-handle-survives-unlink semantics). Argued by the v0.55 dogfood pass where a Windows session caught the cp-busy error and lost ~20 minutes diagnosing it.
- **Workflow-isolation lint ([#690](https://github.com/kwad77/pincher/issues/690), [#681](https://github.com/kwad77/pincher/issues/681) follow-up).** New `cmd/workflow-lint/` Go binary catches the "missing checkout before script reference" bug shape that bit `v0.54.0-beta.1` at tag time (`bash: scripts/release-channel.sh: No such file or directory` because the checksums job ran on a fresh runner with no checkout). Walks every job in `.github/workflows/`, flags any `run:` block referencing repo-local scripts (`bash scripts/`, `./scripts/`, `make <target>`, `go run ./...`) without an earlier `actions/checkout@vN` step in the same job. Pre-tag-time gate via new `Workflow isolation lint` CI job — workflow bugs of this shape now caught at PR-merge time instead of tag-push time. 10 unit tests cover the v0.54 failure shape, all canonical script-reference forms, per-job isolation, and version-agnostic checkout matching.

### Changed
- **`t.Parallel()` across `internal/server` tests — 2× local compression, repo convention ([#697](https://github.com/kwad77/pincher/issues/697) Bucket D).** Zero `t.Parallel()` calls existed anywhere in the repo before this PR (`grep -c 't.Parallel()' **/*.go` returned 0). Added `t.Parallel()` to every top-level `Test*` function across 84 files in `internal/server/`, skipping 5 with hard incompatibilities: `auto_restart_test.go`, `session_persist_test.go`, `stale_binary_test.go` (all use `t.Setenv`), `large_dataset_test.go` (per-endpoint 5s wallclock budget the file's comment says would flake on Windows under parallel load), and `bench_setup_test.go` (only `TestMain`). Local 32-core A/B: 15.8s → 8.0s (2× compression). On Windows CI runners (4 vCPU + Hyper-V + filesystem-lock overhead), the change is flat in the noise — real win is local-dev iteration speed plus establishing `t.Parallel()` as the repo convention so newly-added tests opt-in. The #697 headline acceptance criterion (internal/server <60s on Windows) is not reachable via `t.Parallel()` alone; the actual Windows pole is `cmd/pinch` (228s in this run vs 150s master baseline) — Phase 2 reframed in PR comment.

### Fixed
- **Windows stdio-timing test flakes ([#681](https://github.com/kwad77/pincher/issues/681) Bucket A).** Bumped hardcoded 5s JSON-RPC round-trip timeouts to 15s in the supervisor probe + integration tests. The original budget was borderline on the windows-latest CI runner under `-p 2` parallelism, surfacing as `TestRun_EndToEnd_BareMode` and `TestSupervisor_CapturesAndReplaysInit` flakes (`timeout 5s waiting for id=10`) — two different flakes in two consecutive PR #679 runs during v0.53. Tunable via `PINCHER_TEST_RPC_TIMEOUT` env var (Go duration syntax) so CI can override without code change. Production runtime `defaultProbeTimeout` (5s liveness-ping budget) unchanged — this fix is test-side only. First v0.55 deliverable.
- **Failure-as-pedagogy on `trace`/`symbol`/`neighborhood` not-found + `trace` negative-depth clamp ([#703](https://github.com/kwad77/pincher/issues/703), [#704](https://github.com/kwad77/pincher/issues/704)).** Two adjacent failure-surface bugs caught by EXPLORE-mode probing during the v0.55 dogfood pass. **#703**: `trace name=X depth=-1` returned `{hops:[], total:112, risk_summary:{populated}}` — internal invariant violation because the depth-grouping loop `for d:=1; d<=depth` never executed (depth was -1) but the upstream BFS output still populated `total` and `riskCounts`. Now clamps depth<1 to 1 and emits `_meta.warnings` documenting the clamp. **#704**: `trace`/`symbol`/`neighborhood` returned bare `errResult("symbol X not found")` text with no `_meta` envelope — agents got stuck with no remediation. Now returns a JSON-shaped error body with `_meta.next_steps` suggesting `search query=<short-name>` (the obvious next move, handles typos/case/stale IDs) and `list` (to verify the right project is indexed). Short-name extraction is new helper `shortNameFromID` (parses the `{file}::{qn}#{kind}` format down to the last dotted segment). Aligns with the v0.17 "failure as pedagogy" theme and the `standardized_error_envelope` capability that was already advertised but not honored on these handlers. Backwards-compatible: text-only clients still see the error message; JSON-aware clients now also see the remediation hints. Four new tests pin the contract.
- **AFK-safe build + swap loop ([#710](https://github.com/kwad77/pincher/issues/710), follow-up to [#705](https://github.com/kwad77/pincher/issues/705) / [#708](https://github.com/kwad77/pincher/pull/708)).** Two adjacent gaps in the autonomous dogfood loop, both hit minutes after #708 shipped while continuing the v0.55 polish pass. **Build-side lock**: `go build -o pincher.exe ./cmd/pinch/` fails on Windows with `open pincher.exe: The process cannot access the file because it is being used by another process` when anything is holding the project's `pincher.exe` open (a `pincher web` from the project dir is the common case). New `scripts/build.sh` always builds to `pincher{.exe}.new` then atomically renames over the target — Windows allows renaming over a locked file because handle resolution is by inode, not path. Makefile `build:` delegates to it. **Pre-swap safety probe**: `scripts/swap-active-binary.sh` now runs `$SOURCE --version` before performing the swap and refuses if it fails — protects the AFK user from a broken-build crash-loop where the supervisor respawns onto a binary that can't start, leaving zero MCP available. Set `SKIP_PROBE=1` to bypass intentionally. Together: `make install` is now a single bulletproof step the autonomous loop can call after every fix without the user needing to be present to recover.
- **Input-handling hardening: clamp warnings + corrected `dead_code` diagnosis ([#712](https://github.com/kwad77/pincher/issues/712) Groups B + C.1).** Caught during the v0.55 exhaustive dogfood probe. **Silent input clamping** — `search limit=-5` / `limit=9999999`, `trace depth=99`, `list limit=-5` / `active_within_days=-7`, `trace kinds=<unknown>` were all coerced or ignored with no signal, so the caller got different behaviour than it asked for and couldn't tell. Each now surfaces a `_meta.warnings` entry naming the original value, the adjusted value, and the valid range. `trace depth>5` now clamps to 5 (was silently honored — an off-by-typo `depth=50` could pin a goroutine on a hotspot BFS). `trace kinds=` with an unrecognized edge kind warns instead of returning a silent 0-hop traversal that's indistinguishable from "no edges of this kind." **Inverted `dead_code` diagnosis** — the empty-result diagnosis said "tighten min_confidence to find more candidates", but tightening *raises* the floor and surfaces *fewer*. Corrected to "lower min_confidence" and added a `next_steps` entry that re-invokes `dead_code` at 0.7 so regex-extracted sub-1.0 symbols enter the candidate pool. 8 new tests in `input_clamp_warnings_test.go` + updated `TestHandleDeadCode_EmptyDiagnosis` (the pre-fix test asserted empty results should carry *no* next_steps — that was the wrong default, exactly the silent-empty anti-pattern the v0.17 theme set out to kill).
- **HTTP dispatcher: malformed-JSON body + GET-on-unknown-tool status codes ([#714](https://github.com/kwad77/pincher/issues/714)).** Two HTTP-gateway bugs caught during the v0.55 exhaustive probe. **Malformed JSON masked as a field error** — a `POST /v1/search` with a body like `{bad json` fell through to `parseArgs`, which logged a warning and returned an empty args map, so the caller saw a misleading `query is required` instead of "your JSON is broken." The dispatcher now runs `json.Valid` on the body and returns a distinct `invalid_json_body` 400 before reaching the tool handler. **GET on an unknown tool returned 405 not 404** — the non-POST branch checked the method before tool existence, so `GET /v1/never_existed` said "method not allowed — use POST" even though POSTing would also 404. Now resolves the tool name first: a path that isn't a known tool gets a 404 + `available_tools` list (same shape the POST path already produced); only real POST-tools hit with the wrong verb get the 405. New error code `invalid_json_body` registered in `error_envelope.go`. 3 new tests in `http_method_test.go` + a guard test that an empty body still defaults to `{}`.
- **Failure-as-pedagogy on 11 arg-validation error paths ([#712](https://github.com/kwad77/pincher/issues/712) Group A).** #709 fixed the *not-found* paths on `trace`/`symbol`/`neighborhood`; this closes the *arg-validation* paths across the rest of the tool surface. Every "X is required" / "unknown action" / "invalid url" rejection now returns the rich JSON envelope (`{error, _meta.next_steps}`) instead of a bare text string — the caller gets a concrete valid call shape, not just a complaint. Sites converted: `search` empty-query (→ exact + prefix + architecture examples), `query` empty-pinchql (→ a working MATCH query + `schema`), `trace` no-seed (→ by-name + by-id shapes), `symbol`/`context`/`neighborhood` no-id (→ `search` to get an ID), `guide` empty-task (→ example task strings), `fetch` no-url + bad-scheme (→ a valid https example, names the rejected URL), `adr` unknown-action + set/get/delete missing-key (→ enumerates valid actions + a working set/get/list triple), `index` no-path + index-error (→ path shape + `list` to see what's indexed). This honors the `standardized_error_envelope` capability that was advertised but not consistently delivered, and completes the v0.17 "audit every tool's empty-response path" throughline for the input-rejection class. 11 new tests in `group_a_pedagogy_test.go`. Backwards-compatible: text-only clients still see the error message (JSON is a superset in their renderers); JSON-aware clients pick up the remediation.
- **`context` with an all-unknown `fields` list no longer ships an empty body ([#712](https://github.com/kwad77/pincher/issues/712) Group C.2).** `context fields=id` (whose real top-level keys are `symbol`/`imports`/`callees`) used to silently project down to a `{_meta}`-only response — the caller got nothing back and no signal why. `context` now reports unknown field names in `_meta.warnings` alongside the valid key list, and when *every* requested field is bogus it falls back to the full response rather than an empty one. A partial mix keeps the valid projection and still warns about the dropped names.
- **Three silent-failure gaps in `symbols`, `query`, and `adr` ([#712](https://github.com/kwad77/pincher/issues/712) follow-up).** The original Group A failure-as-pedagogy sweep missed `symbols`' two arg-rejection paths — empty `ids` array and over-cap batch both still returned a bare text error. They now return the rich `_meta.next_steps` envelope: an empty batch points at `search` / `query` (the tools that produce IDs), an over-cap batch explains the paging split. `query` silently dropped the legacy `cypher` alias when a caller passed *both* `pinchql` and `cypher` — same silent-ignore class as #473's unknown-property bug. It now runs `pinchql` and surfaces a `_meta.warnings` entry naming which one ran. And `adr action=get` on a key that doesn't exist returned a bare text error; it now points at `adr action=list` so the caller can spot a typo or wrong-project scope.
- **Variable-length pinchQL BFS (`MATCH (a)-[:CALLS*1..N]->(b)`) was ~2500× slower than it needed to be.** On pincher-repo's own graph (5k symbols, 15k edges) a depth-2 `query` BFS took ~1.3s — the recursive CTE itself runs in <1ms, but the final `JOIN symbols` was the whole cost: SQLite has no row-count statistics for a recursive CTE, mis-planned the join, and full-scanned the 5k-row `symbols` table probing the ~200-row CTE for every row. `bfsViaCTE` now pre-aggregates reachable nodes into a `reachAgg` CTE and `CROSS JOIN`s `symbols` — the CROSS JOIN pins the tiny CTE as the outer loop so `symbols` is seeked by its `id` primary key instead. Same results, 1.3s → 0.5ms on the dogfood corpus. Found while dogfooding v0.55; `trace`'s BFS was already fast (different query path), which is why this hid for so long.
- **`guide` was blind to the `dead_code` tool and over-eager with trace-internals hints.** Two dogfood-found `guide` quality bugs. A task like `guide task="find functions that have zero callers"` matched `coverage`/`test` keywords, routed to the test shape, and recommended `search`+`context` — never `dead_code`, the purpose-built zero-inbound-edge tool. `guide` now has a `dead_code` shape: tasks mentioning dead code / unused functions / unreachable / zero-or-no callers / never-called route straight to `dead_code` (with `trace inbound` + `context` as the verify-before-delete follow-ups). Separately, the `domainConcepts` table matched a bare `"callers"` substring and prepended a "trace BFS uses a recursive CTE" *source-pointer* hint to any task mentioning callers — wrong for someone who just wants to find dead code. That pattern is now tightened (#616-style) to only fire on tasks investigating trace's own implementation (`traceViaCTE`, `trace internals`, `how does trace`, …).
- **`neighborhood` silently clamped negative `limit`/`offset` inputs.** A `neighborhood` call with `limit<=0` or `offset<0` coerced the value (to 50 and 0 respectively) without telling the caller — the same silent-input-coercion class the #712/#713 sweep fixed in `search`, `list`, and `trace`. `neighborhood` now surfaces each clamp in `_meta.warnings`, merged alongside any pagination `next_steps` without clobbering them.
- **HTTP gateway double-wrapped `errResultRich` responses.** When an MCP tool returned the rich failure-as-pedagogy envelope (`errResultRich`, #709/#712) — e.g. `POST /v1/search` with no `query` — the HTTP dispatcher took the entire JSON envelope string and stuffed it verbatim into the standardized error envelope's `message` field, producing a double-encoded `{"error":{"code":"tool_error","message":"{"_meta":...,"error":"..."}"}}` blob no client could pattern-match on. The dispatcher now detects the rich shape, lifts the inner `error` string into `message`, and carries `next_steps` through as `details` so HTTP clients keep the remediation hints. Bare `errResult` text isn't JSON, so it passes through unchanged. Found while dogfooding v0.55's HTTP surface.
- **Stale pincher processes can no longer stomp shared project metadata ([#724](https://github.com/kwad77/pincher/issues/724), part 1 of 2).** When several pincher processes Watch() the same project against one shared DB — the common case being an orphaned process whose parent died but whose `Watch()` loop lives on — each one's `UpsertProject` was last-writer-wins on `schema_version_at_index` and `binary_version`. A 2-day-old orphan running an ancient binary was observed silently rewriting a current project's metadata back to `0.21.0` / schema 23, which breaks the `index_drift` detector and CLAUDE.md's freshness check — the very signals meant to catch staleness. `UpsertProject`'s `ON CONFLICT` clause is now monotonic: `schema_version_at_index` can only move forward (`MAX(stored, incoming)`), and `binary_version` is only adopted when the incoming writer's schema is `>=` the stored one (an older schema means an older binary, by definition). Path/name/counts still update unconditionally. The companion fix — parent-liveness reaping so orphans self-exit instead of accumulating — is tracked as part 2 of #724.
- **Orphaned pincher processes now reap themselves ([#724](https://github.com/kwad77/pincher/issues/724), part 2 of 2 — closes the issue).** A stdio MCP server and the supervisor both detect a *graceful* parent disconnect via stdin EOF — but a parent killed with SIGKILL, crashed, or lost to power-off may never close the pipe, leaving an orphan whose `Watch()` loop keeps running against the shared DB. Three such orphans were observed on one machine during v0.55 dogfooding, one of them two days old running an ancient binary. Every long-lived stdio process (`pincher`, `pincher supervised`) now captures its parent PID at startup and polls liveness every 30s; when the parent is gone it cancels its context for a clean shutdown (session-stat flush, `Watch()` teardown), then hard-exits as a backstop. Intentionally-detached servers (`pincher web`'s `--no-stdio` HTTP child) are exempt — they're *supposed* to outlive their spawner. Together with part 1's monotonic metadata guard (#726), the shared-DB metadata-corruption class is closed: orphans can't accumulate, and in the window before one is reaped it can't downgrade another project's freshness fields.

### Removed
- **Bench-regression CI gate ([#681](https://github.com/kwad77/pincher/issues/681) Bucket B).** The `Benchmark regression (advisory)` job ran with `continue-on-error: true` and failed on most PRs (variance on shared runners). Always-red advisory check polluted every PR's check view without ever blocking — reviewers learned to ignore it, signal-to-noise went to zero. Removed in favor of the existing `bench-smoke` job (100ms/bench, required) for compile-test coverage and `make corpus-bench` for local perf validation. Baselines under `testdata/bench/` stay committed as a re-promotion starting point if we ever capture the N≥20 variance dataset (per `feedback_bench_gate_promotion.md`). Second v0.55 deliverable.

## [v0.54.0] — 2026-05-13 — closure tables + streamable-HTTP transport: structural perf + cluster-friendly

Phase 1 — release 3 of 9. First beta-tag-shape release exercising v0.53's release-channel infrastructure end-to-end (`v0.54.0-beta.1`). Three deliverables that turn pincher from a single-tenant local primitive into a cluster-mountable backend with materialized perf:

- **Closure tables** materialize the depth-3 transitive closure of the edges graph at index time so `trace` becomes a single indexed SELECT (~1ms) instead of a recursive CTE (5–50ms). Off by default; opt-in via `PINCHER_CLOSURE_TABLES=1` while phase 1 collects field validation.
- **Streamable-HTTP MCP transport** mounts on the existing HTTP server at a configurable path. Stdio + HTTP transports share the same in-process `*mcp.Server` — same registered tool set, same `_meta` envelope. Routers (zelos, bifrost) deployed in k8s skip per-backend stdio sub-process spawning.
- **Closure-table storage measurement tool** (`cmd/closurebench/`) — the decision-blocking measurement that validated default depth=3 (~325 MB worst-case at 10k files; well under the 500 MB phase-1 budget).

Schema **v25** — adds the `closure(project_id, from_id, to_id, depth)` table.

### Added
- **Closure tables phase 1 ([#652](https://github.com/kwad77/pincher/issues/652), [#403](https://github.com/kwad77/pincher/issues/403)).** Schema **v25** — new `closure(project_id, from_id, to_id, depth) WITHOUT ROWID` table with `idx_closure_to(project_id, to_id)` for inbound queries. Off by default; opt-in via `PINCHER_CLOSURE_TABLES=1` (env: `PINCHER_CLOSURE_MAX_DEPTH`, default 3, clamped to [1, 8]). When enabled, the indexer's tail-pass materializes the depth-N transitive closure of the edges graph; the `trace` tool then routes to a single indexed SELECT (~1ms) instead of a recursive CTE (5–50ms). Phase-1 trade-off: closure rows don't store per-hop edge kind, so the `via` field on trace results is empty when the fast-path fires — callers needing `via` get the CTE path automatically by passing a non-default `kinds` filter. Capability `closure_tables` advertised in `_meta.capabilities` when the table has rows. Storage cost validated by [#639](https://github.com/kwad77/pincher/issues/639) — pincher-repo at depth=3 = 10.8 MB, linear-worst-case extrapolation to a 10k-file repo = ~325 MB (under the 500 MB phase-1 budget).
- **Closure-table storage measurement tool ([#639](https://github.com/kwad77/pincher/issues/639)).** New `cmd/closurebench/` Go binary + `scripts/closure-table-bench.sh` wrapper. Builds the depth-N transitive closure of a project's edge graph into a fresh side-DB, vacuums via `wal_checkpoint(TRUNCATE)`, stats the file. Closure-only bytes — no other rows polluting the size measurement. Pincher-repo result: 14,244 edges → 50,274 rows / 10.8 MB at depth=3, 116,204 rows / 24.7 MB at depth=5. Decision (informs [#652](https://github.com/kwad77/pincher/issues/652) phase 1): ship default depth=3 (worst-case ~325 MB on a 10k-file repo, well under the 500 MB threshold); depth=5 should not be the default. Tool reusable for follow-up measurements on Kubernetes / Linux subsets.
- **Streamable-HTTP MCP transport ([#651](https://github.com/kwad77/pincher/issues/651)).** First v0.54 deliverable, paired with v0.53's capability + complexity-tier router-integration contract. New `--mcp-http-path /mcp` flag (env: `PINCHER_MCP_HTTP_PATH`) mounts the MCP SDK's `StreamableHTTPHandler` on the existing HTTP server. Stdio and streamable-HTTP can run simultaneously and share the same in-process `*mcp.Server` — same registered tool set, same `_meta` envelope. The transport inherits `--http-key` bearer auth, rate limiting, and basepath stripping. Capability `streamable_http` advertised in `_meta.capabilities` when the transport is active. Routers (zelos, bifrost) deployed in k8s skip per-backend stdio sub-process spawning. Documented in [`docs/streamable-http.md`](docs/streamable-http.md).

### Why now
v0.52 reset the surface (every tool reachable everywhere). v0.53 made it self-describing (capabilities + tier + channel). v0.54 makes it **cluster-mountable + structurally fast** — pincher becomes the kind of backend a routing-shaped consumer (zelos, bifrost, detour-shape) can deploy as a fleet. First beta tag (`v0.54.0-beta.1`) validates the release-channel pipeline against a real beta channel before any stable promotion ramp.

### Phase-1 trade-offs (deliberate; phase-2 follow-ups filed)
- Closure rows don't store per-hop edge kind — `trace`'s `Via` field is empty when fast-path fires. Workaround: pass non-default `kinds` filter for CTE path. Resolved in [#685](https://github.com/kwad77/pincher/issues/685) (v0.65).
- Storage measurement validated only at pincher-repo scale. At-scale validation (Kubernetes / Linux / VSCode subsets) tracked in [#686](https://github.com/kwad77/pincher/issues/686) (v0.55).
- Streamable-HTTP wiring contract tested but no concurrent-session loadtest. Tracked in [#687](https://github.com/kwad77/pincher/issues/687) (v0.56).

## [v0.53.0] — 2026-05-13 — router-integration contract: capabilities, complexity tiers, release channels

Phase 1 — release 2 of 9. Three deliverables that together make pincher a first-class backend for routing-shaped consumers (zelos / bifrost / detour-shape). Every tool response now declares what it can do (`_meta.capabilities`), what kind of model should handle its output (`_meta.complexity_tier`), and which channel produced it (release-channel infrastructure). Routers can plan calls, route follow-up steps, and pick install paths without scraping version strings or trial-and-error calls.

No schema change — runs on schema v24.

### Added
- **`_meta.capabilities` advertisement field ([#649](https://github.com/kwad77/pincher/issues/649)).** First v0.53 deliverable. Every tool response now includes a `capabilities` slice on the `_meta` envelope declaring runtime-detected feature support: `schema_v24`, `hook_check`, `supervised`, `operator_tools_on_mcp`, `session_persistence`, `binary_drift_warning`, `tokens_used_envelope`, `tokens_saved_pct`, `standardized_error_envelope`, `complexity_tier`, plus conditional `http_auth` when `--http-key` is set. Routers consume the field to make integration decisions (subscribe to SSE? expect operator tools via MCP? require auth?) without scraping version strings or doing trial-and-error calls. Lockstep gate test (`capability_test.go`) requires every advertised tag to have a runtime probe — false advertising fails the build.
- **Per-tool `complexity_tier` classification ([#650](https://github.com/kwad77/pincher/issues/650)).** Second v0.53 deliverable. Three tiers (`lite` / `standard` / `heavy`) classify every registered tool by response shape — small structured data, medium structured data, or synthesis-style output. Two surfaces, same data: `x-pincher-tier` per-endpoint annotation in the OpenAPI spec (planning-time consumers like detour-shape model routers can decide before the call); `_meta.complexity_tier` on every tool response (call-time confirmation). Routers consume both to decide which model handles the agent step that consumes the response. Capability advertisement (#649) extended with `complexity_tier` tag. Gate test (`complexity_tier_test.go`) enforces every registered tool has a classification, only known tier values, and lockstep between OpenAPI annotation and runtime injection.
- **Release channels ([#642](https://github.com/kwad77/pincher/issues/642)).** Third v0.53 deliverable, operational backbone for v0.54's first beta tag. Stable channel = minor version divisible by 10 (v0.60, v0.70, ..., v1.0); everything else is dev. Pre-release suffixes (`-beta.N`, `-alpha.N`, `-rc.N`) override the modulo rule and route to their respective channels. The release workflow now consults `scripts/release-channel.sh` to decide channel promotion: Homebrew formula bump + Docker `latest`/`stable` tags only on stable promotions; Docker `dev` tag for dev releases; `beta`/`alpha`/`rc` tags for pre-release suffixes. GitHub release marks dev/beta/alpha/rc as pre-release. Channel rule covered by `scripts/release-channel_test.sh` (22 cases) wired into CI as the `Release channel rule` job. New `docs/release-channels.md` documents install paths per channel.

### Why now
v0.52 reset the surface (every tool reachable everywhere). v0.53 makes the surface *self-describing* — capabilities + tier + channel are the contract that lets a router plan a multi-step agent run without per-call discovery. v0.54 ships the first beta tag (`v0.54.0-beta.1`) which exercises the channel infrastructure end-to-end.

## [v0.52.0] — 2026-05-13 — full MCP restoration; bedrock-layer surface

The aggregator-deployment correction. v0.35 #624 narrowed the MCP surface from 22 tools to 9 on the theory that agents face decision tax from large tool lists. That argument doesn't hold under aggregator deployment (zelos / bifrost / detour-shape) where the agent already faces N backends × M tools each — pincher having 22 vs 11 is invisible noise relative to the cluster surface. Real-user feedback through zelos's `pincher__index` failure surfaced the gap; v0.51.0 restored `index` + `adr`, v0.51.1 added redirect stubs for the rest. v0.52 ships the full reversal: every operator tool now agent-callable via MCP with its own typed schema; the stub mechanism is deleted entirely.

The reframe: pincher is the bedrock-layer code-intel primitive that any routing-shaped consumer (zelos for MCP, bifrost for LLMs, detour for model-tier routing) wants to incorporate. The cleanest backend wins. Every tool reachable through every transport is the surface contract that supports that positioning.

No schema change — runs on schema v24.

### Changed
- **MCP surface 11 → 22 tools ([#645](https://github.com/kwad77/pincher/issues/645) follow-on; full reversal of [#624](https://github.com/kwad77/pincher/issues/624)).** Every operator tool restored to MCP-visible with its real handler: `architecture`, `dead_code`, `neighborhood`, `health`, `stats`, `schema`, `list`, `doctor`, `rebuild_fts`, `init`, `self_test`. Per-tool typed InputSchemas preserved; descriptions cleaned (the `[OPERATOR-ONLY ...]` prefix is gone). HTTP routes preserved for ops automation. CLI ↔ HTTP ↔ MCP parity gate stays green.
- **Stub mechanism deleted.** `addOperatorTool` and `makeOperatorRedirectHandler` removed from `internal/server/server.go`. The v0.51.1 redirect-stub tests in `operator_redirect_test.go` deleted. `mcp_surface_split_test.go` repurposed as a single-set MCP-surface contract test (all registered tools must be agent-callable).
- **README leading paragraph** updated: "9 agent-facing MCP tools plus 13 operator/diagnostic tools on the HTTP REST API" → "22 agent-callable MCP tools, every one also reachable via the HTTP REST API at `/v1/<tool>`".

### Why now
Aggregator deployment changes the calculus on tool-surface narrowing. The original argument was "fewer tools = lower agent decision tax." Under zelos/bifrost/detour-shape consumption, the agent's working set is `N backends × M tools each` — pincher's 22 vs 11 is invisible noise. Meanwhile, the surface-narrowing's cost (bare "unknown tool" errors when wrappers route by tool name) was real and biting users. Full reversal corrects both sides.

## [v0.51.1] — 2026-05-12 — operator-tool MCP redirect stubs

Patch follow-on to v0.51.0. The remaining 11 operator-only tools (architecture, dead_code, doctor, health, init, list, neighborhood, rebuild_fts, schema, self_test, stats) now ship with an MCP redirect stub so an agent that calls one over MCP gets a structured `operator_tool_not_on_mcp` error pointing at the HTTP endpoint and CLI subcommand — instead of the SDK's bare `unknown tool "X"` that trains users to think the tool is missing. The stub description is prefixed `[OPERATOR-ONLY — call POST /v1/<tool> over HTTP or pincher <tool> from CLI]` so an agent reading `tools/list` skips them upfront without having to invoke and parse the redirect.

No schema change — runs on schema v24.

### Fixed
- **Bare `unknown tool "X"` MCP error replaced with structured redirect ([#644](https://github.com/kwad77/pincher/issues/644)).** `addOperatorTool` in `internal/server/server.go` now mirrors operator tools on MCP via `s.mcp.AddTool` with a thin redirect handler. Body shape: `{"error": {"code": "operator_tool_not_on_mcp", "message": "...", "details": {"tool", "http_endpoint", "cli_command", "since_version"}}}`. Test coverage: 11 redirect cases in `internal/server/operator_redirect_test.go` (one per operator tool).

## [v0.51.0] — 2026-05-12 — restore index + adr to MCP; explain indexing in README

The dogfood-driven correction release. A real user hit `unknown tool "index"` trying to call index over MCP — the v0.35 #624 surface narrowing had swept index into the operator-only bucket on the theory it was diagnostic noise. It isn't. Index is core: it's how an agent helps a user onboard a fresh repo, recovers from binary-version drift surfaced in `_meta.binary_version_warning`, and closes the in-session-edit race the watcher's 2s tick can't cover. v0.51 restores it to the agent-facing surface (along with `adr`, which has the same shape — institutional memory the agent reads + writes mid-session per the global CLAUDE.md policy). README gains a new "Indexing & staleness" section that walks through the four staleness-defense mechanisms and the three cases where manual `index` is the right lever.

No schema change — runs on schema v24.

### Changed
- **MCP-visible tool surface 9 → 11 ([#645](https://github.com/kwad77/pincher/issues/645)).** `index` and `adr` move back from `addOperatorTool` to `addTool`. HTTP routes preserved for backward compat. `mcp_surface_split_test.go` partition updated; tool-contract golden file regenerated.
- **README adds an "Indexing & staleness" section.** Four-mechanism table (initial index / watcher / cross-file resolution / SessionStart hook), explicit "when to call `index` manually" guidance for the three cases the watcher can't cover, and the four signals that say the index is stale.

### Notes
- Pairs with [#644](https://github.com/kwad77/pincher/issues/644) (structured `operator_tool_not_on_mcp` error for the remaining 11 operator tools — keeps future surface changes from biting users with bare "unknown tool" errors). #644 ships separately in v0.51.
- The v0.35 narrowing was a deliberate experiment, not a bug. Framing this as repositioning rather than fix.

## [v0.50.0] — 2026-05-12 — maturity consolidation + README repositioning

The version-number-truthing release. Eighteen minor releases shipped in roughly 24 hours (v0.21 through v0.38), each picking up real work — dashboard hardening, hook foundation, conversion-rate metric, polyglot install warning. The changelog reflects every step of that, but the leading single-digit version number understated the actual maturity of the codebase. v0.50.0 is the discipline correction: bump the version to where the test coverage (85.2% sustained), tool surface (frozen since v0.35), and schema (v24, stable since the v0.36 hook table) actually sit. No new functionality past v0.38; this is purely the README differentiator from #641 plus the version-number truthing.

The remaining path to v1.0 is tracked in [#638](https://github.com/kwad77/pincher/issues/638). Eight named release themes remain (v0.51 through v0.56 plus a v0.46-equivalent field-data slot, ending in v1.0).

No schema change — runs on schema v24.

### Added
- **README \"What it does\" leads with a differentiator paragraph ([#641](https://github.com/kwad77/pincher/pull/641)).** Names the category overlap with Sourcegraph / OpenGrok / IntelliJ in one sentence, then compresses the three concrete differences (agent-context-window-shaped responses, runtime hook interception of Read/Grep, local-only) into a single trailing clause. The mechanism paragraph (single Go binary indexing into three co-located layers) stays — it's now the second paragraph. Closes the README differentiator open question from [#638](https://github.com/kwad77/pincher/issues/638).

### Notes
- Versions v0.39 through v0.49 are not skipped in the SemVer sense — they were never tagged. The version sequence jumps from v0.38 directly to v0.50, and no installer behavior changes for users who previously held at v0.38.
- Planning milestones have shifted: v0.39 → v0.51, v0.40 → v0.52, v0.41 → v0.53, etc. Open issues moved to the new slots: [#635](https://github.com/kwad77/pincher/issues/635), [#639](https://github.com/kwad77/pincher/issues/639), [#640](https://github.com/kwad77/pincher/issues/640), [#642](https://github.com/kwad77/pincher/issues/642).

## [v0.38.0] — 2026-05-12 — polyglot install warning

The expectation-setting release. Without this, a Ruby- or Scala-heavy repo's first session lands ~1-3% savings against a README promising 95-99% — the user concludes "pincher doesn't work" when actually they're in a different tier. `pincher init` now reports per-language extraction confidence + expected savings tier at install time, before the disappointment can happen.

No schema change — all v0.38 work runs on schema v24.

### Added
- **Per-language extraction-tier profile in `pincher init` ([#631](https://github.com/kwad77/pincher/issues/631)).** New `internal/init/profile.go` walks the target directory via gocodewalker (respecting `.gitignore`, capped at 5000 files for install-time latency), classifies each file via the existing `ast.DetectLanguage` + `ast.RegisteredConfidence` plumbing, and prints a one-screen summary. Four tiers map to the v0.34 README #621 vocabulary: AST (95-99% on retrieval), stable-regex (80-95%), approximate-regex (60-85%, structural queries less reliable), and stub (1-3% — pincher won't accelerate this workflow). Headline tier picks the majority-by-file-count and emits a typical-session savings band. New `--quiet` flag suppresses the profile for CI/scripted installs while still running the wiring. Vendor / node_modules / .git / build / dist directories are excluded from the install-time sample (full re-index later sees them if relevant).

## [v0.37.0] — 2026-05-12 — hook conversion-rate dashboard

The measurement release. v0.36 wired runtime interception via the PreToolUse hook; v0.37 surfaces the headline metric — what fraction of redirected Read/Grep calls the agent actually follows through on. Two panels triangulate the diagnosis: an override rate that isolates "saw and rejected" from "no signal yet", and a per-tool breakdown so a low conversion rate has somewhere to drill into.

No schema change — all v0.37 work runs on schema v24. The remaining triangulating panels (entropy, payload size, per-tier %) carve out as [#635](https://github.com/kwad77/pincher/issues/635) for v0.38; they need new data plumbing not currently tracked.

### Added
- **`GET /v1/hook-stats` endpoint + headline conversion-rate dashboard panel ([#628](https://github.com/kwad77/pincher/issues/628)).** Returns trailing-7-day `redirects`, `taken`, and `conversion_pct` from the v0.36 `hook_invocations` table. Dashboard renders a "Read/Grep → pincher (7d)" card showing the bounded percentage. When no intercepts exist yet the panel renders an onboarding hint pointing at `pincher init --target=claude` rather than flapping zero-percent. Endpoint is GET-only; POST returns 405 with `Allow: GET, HEAD`. Local-only — every byte originates in the user's `pincher.db`.
- **Triangulating panels: override rate + per-tool breakdown ([#629](https://github.com/kwad77/pincher/issues/629), partial).** Two supporting cards beneath the headline. **Override rate** isolates "agent saw the suggestion and rejected it" (`took_recommendation=0`) from "no signal yet" (`took_recommendation IS NULL`) — distinct from `100%-conversion_pct` because the unresolved bucket is excluded from both numerator and denominator. **Per-tool breakdown** reports redirects and takes for Read vs Grep separately so an imbalance flags which decision tier needs rebalancing. Backed by new readers `HookOverrideRate7d` and `HookCountsByTool7d`. The remaining three panels (tool-call entropy, median payload, per-tier saved-pct medians) carve out to [#635](https://github.com/kwad77/pincher/issues/635) — they each require new data plumbing not currently tracked.

## [v0.36.0] — 2026-05-12 — hook foundation: PreToolUse interception + telemetry

The leverage release. Instruction-layer nudges plateaued — `CLAUDE.md` saying "use pincher first" is a soft prior that competes with the strong prior "Read/Grep always works." This batch ships runtime interception via Claude Code's PreToolUse hook so the redirect happens at the moment of decision, not by persuasion. One install — `pincher init --target=claude` — wires both the MCP server config AND the hook entry. Conversion-rate metric on the dashboard ships in v0.37.

Schema migration v23 → **v24** — adds the `hook_invocations` telemetry table. Local-only; no phone-home.

### Added
- **`pincher hook-check` subcommand ([#625](https://github.com/kwad77/pincher/issues/625)).** Reads a Claude Code PreToolUse JSON payload from stdin; writes a hook-spec response on stdout. Pass-through is silent (no `stopReason`, no `systemMessage`); redirect carries `continue:false` plus a one-line message naming the suggested call. Decision logic for Read: pass through on unindexed paths, files < ~3.5 KB, files with < 5 indexed symbols, and Read calls already narrowed via offset/limit; otherwise redirect to `context id=<largest-symbol> lite=true`. Latency budget < 50 ms.
- **Grep redirect logic ([#630](https://github.com/kwad77/pincher/issues/630)).** Within `hook-check`, Grep with a single-identifier pattern (`Foo`, `pkg.Bar`, `Class::method`) on an indexed project rewrites to `search query="<pattern>"`. Regexes (`func \w+\(`), multi-word phrases (`hello world`), special-char-only patterns (`->`), and non-indexed paths pass through. Identifier check runs before the regex-metachar check so qualified ids (which contain `.` and `:`) aren't misclassified as regex.
- **`pincher init --target=claude` writes/merges the PreToolUse hook into `.claude/settings.json` ([#627](https://github.com/kwad77/pincher/issues/627)).** One install wires both MCP and the hook. `--no-hook` flag skips the hook write for users who want only the MCP wiring. Idempotent: re-running init detects existing `pincher hook-check` entries (including those with custom command paths or args) and leaves them alone. Existing settings.json keys (theme, telemetry, other event hooks) are preserved.
- **Hook telemetry table ([#626](https://github.com/kwad77/pincher/issues/626)).** Schema v24 `hook_invocations(id, ts, session_id, tool_name, file_path, file_bytes, decision, suggested_tool, suggested_args, next_tool_within_3, took_recommendation)`. `pincher hook-check` writes one row per invocation (best-effort — never blocks the decision on a failed insert). `ResolveHookInvocationsForSession` is a post-hoc joiner that walks the session's subsequent tool calls and sets `took_recommendation=1` when the suggested tool fires within 3 calls. Conversion rate (`taken / redirects`) is the v0.37 headline dashboard metric. New `HookConversionRate7d` reader returns the trailing-7-day percentage. Local-only — no phone-home, no CLI flag for upload.

### Migration
Schema migrates v23 → v24 automatically on first `pincher` run. Additive — no existing data touched. Re-index is **not** required.

## [v0.35.0] — 2026-05-12 — envelope discipline + MCP surface split

Three changes that shift how pincher's tool surface presents to agents. Pedagogy-shape `next_steps` no longer ride on every successful response (kept on empty/ambiguous results where the pedagogy is load-bearing). New `context lite=true` mode returns source-only minimum-envelope for the v0.36 PreToolUse hook redirect path. The MCP-visible tool surface drops from 22 to 9 — operator/diagnostic tools (architecture, health, schema, list, index, adr, neighborhood, stats, doctor, rebuild_fts, self_test, dead_code) remain reachable via `POST /v1/<tool>` HTTP for monitoring dashboards and ops automation.

No schema change — all v0.35 work runs on schema v23.

### Added
- **`verbose=true` universal opt-in for the full `_meta` envelope ([#622](https://github.com/kwad77/pincher/issues/622)).** Default behavior on the success path now strips pedagogy `next_steps` (workflow hints like "you found Foo, now run context on its ID" — useful once, then noise on every subsequent call). Stripping is gated to leave warning-bearing responses (`warnings`, `diagnosis`) and same-tool pagination entries (`tool: "list"` continuation on a `list` call) untouched. Agents that want the full instrumentation envelope pass `verbose=true` on any tool.
- **`context lite=true` source-only minimum-envelope mode ([#623](https://github.com/kwad77/pincher/issues/623)).** Returns `{id, source}` and nothing else — no imports walk, no callees walk, no next_steps. Used by the v0.36 PreToolUse hook redirect when replacing a Read call: agent gets exactly the bytes Read would have given them, with the smallest possible envelope. Same retrieval semantics as positional Read but with byte-offset precision. Skips the `IMPORTS`/`CALLS` edge walks that account for most of `context`'s per-call latency on big symbols.

### Changed
- **MCP-visible tool surface narrows from 22 to 9 ([#624](https://github.com/kwad77/pincher/issues/624)).** Agent-facing set: `search`, `symbol`, `symbols`, `context`, `trace`, `query`, `guide`, `changes`, `fetch`. The other 13 (operator/diagnostic tools) are now registered via the new `addOperatorTool` helper which populates `s.handlers` (HTTP dispatch) and `s.tools` (registry / OpenAPI / output schemas / parity gates) but skips the MCP `AddTool` call. The MCP `tools/list` response shrinks to the working set most agents reach for; the rest stay on `POST /v1/<tool>` for operators. CLI ↔ HTTP parity (#558 phase 3) is preserved — every CLI subcommand still has its corresponding endpoint.

## [v0.34.0] — 2026-05-12 — measurement honesty: bounded percentages, structured fields, per-tier README claims

Three changes that shift how pincher reports its value to a more useful shape. Bounded percentages are easier to reason about per-call than compounding multipliers; structured fields scale better across clients than parseable prose; per-tier README claims let users match expected savings to the workflow shape they actually have. Forward-looking repositioning — the prior shapes were fine for a first pass; these shapes are clearer for the use cases that have emerged.

No schema change — all v0.34 work runs on schema v23.

### Added
- **`tokens_saved_pct` field on every `_meta` envelope ([#619](https://github.com/kwad77/pincher/issues/619)).** Bounded form of `tokens_saved` — capped at 100%, can be negative when the response envelope cost more than the savings. Reported alongside the absolute count so consumers have both shapes available. Stats CLI box now leads with the percentage; `(97%)` rather than `37x`. Compounding ratios obscure per-call value and are easy to inflate; bounded percentages aren't. The dashboard's absolute counts are unchanged.

### Changed
- **`binary_version_warning` surfaces once per session per project, not once per response ([#620](https://github.com/kwad77/pincher/issues/620)).** When the running binary is older than the project's indexed-by version, the drift advisory previously fired on every tool response — visible noise that trains agents to filter `_meta` entirely, which kills the useful warnings (#473/#499/#612). Now emitted once per (project, indexed-version) pair per server process; a fresh process or a re-stamp at a different version re-arms emission.
- **README repositions savings claims around per-tier percentages ([#621](https://github.com/kwad77/pincher/issues/621)).** New section breaks expected savings down by workflow tier — symbol retrieval (95-99% on large files), structural traversal (80-95%), BM25 search (60-90% on conceptual queries, near-zero on exact-token greps), orientation tools (`null` because no honest baseline), persistence-shaped tools like `fetch` (~0% first call, 85-90% on re-access). New best-case / typical / break-even framing helps users match the tool to their codebase shape rather than expect an aggregate ratio that doesn't apply to their workflow.

### Removed
- **`_meta.savings` human-prose string.** Redundant with the structured `tokens_saved` + `tokens_saved_pct` fields and added ~100B of envelope cost on every successful response. Dashboard renders the percentage as the headline; nothing in the codebase consumed the prose.

## [v0.33.0] — 2026-05-12 — loop-3 dogfood haul: guide methodology routing + fetch JS-render warning

Three issues filed by autoresearcher round 3 against v0.32, fixed in one batch. Loop-3 was scoped to *usability* — friction agents hit even when the underlying tool works correctly.

No schema change — all v0.33 work runs on schema v23.

### Fixed
- **`guide` methodology questions stop extracting category nouns as the hint ([#615](https://github.com/kwad77/pincher/issues/615)).** Pre-fix, `guide task="how do I find what calls a private function"` extracted `"private"` as the hint and templated useless `search query="private"` recommendations. Now visibility/category nouns (`private`, `public`, `exported`, `unexported`, `internal`, `external`, `global`, `local`, `stub`, `static`, `dynamic`) are stop words for hint extraction — the discriminator falls through to the actual subject (or empty when the task is a methodology question with no concrete symbol).
- **`guide` "use pinchQL to ..." routes to the `query` tool with a starter template, not pinchQL source ([#616](https://github.com/kwad77/pincher/issues/616)).** Pre-fix, bare `pinchql` matched a domain concept whose recommendation was `search query="runJoinQuery"` — pointing the user at the dispatcher's source code when they wanted to *use* pinchQL as a query language. Tightened the engine-internals concept to require a disambiguating phrase (`cypher engine`, `pinchql parser`, `where pushdown`, `how does pinchql`, etc.); added a new concept that catches `use pinchql to`, `via pinchql`, `with pinchql`, `pinchql query` and recommends a `query` tool call with a `MATCH (n:Function) RETURN n.qualified_name LIMIT 20` starter template.
- **`fetch` warns when extracted text is suspiciously small relative to raw bytes ([#617](https://github.com/kwad77/pincher/issues/617)).** JS-rendered SPAs (GitHub, Twitter, Reddit, modern docs sites) returned a "successful" response with `stored: true`, plausible `raw_bytes`, real `title` — but `text` was just the inert accessibility skip-link. Agents acted on the empty text. Now `_meta.warnings` carries an entry when raw is > 10 KB AND extracted text is < 0.5% of raw, naming the heuristic and pointing at common workarounds (raw README URL for GitHub repos, the project's REST API, etc.). Skipped on `text/markdown` / `text/plain` inputs where extraction is verbatim.

## [v0.32.0] — 2026-05-12 — loop-2 dogfood haul: 0%-coverage gates + edge-property warning pedagogy

Three issues filed by autoresearcher round 2 against v0.31, fixed in one batch. Two are 0%-coverage gates on load-bearing helpers (#611 pinchQL `NOT (...)` Go-side eval, #613 `db.CurrentSchemaVersion`) — same risk class as #607. The third is a pedagogy refinement: edge-property warnings now list edge properties instead of misleading users with the symbol property list.

No schema change — all v0.32 work runs on schema v23.

### Fixed
- **`pinchQL` edge-property warnings now list edge properties ([#612](https://github.com/kwad77/pincher/issues/612)).** Pre-fix, querying `r.source` (an unrecognized edge property) emitted `Valid properties: id, name, qualified_name, kind, file_path, language, ...` — every property listed was a SYMBOL property even though the user wrote a property reference on an edge variable. Now `collectUnknownPropertyWarnings` tracks each pattern's `edgeVar` and emits `Valid edge properties: kind, confidence` when the offending variable was bound to an edge. Same pedagogy spirit as #473/#499/#501 — fix the part that actually misleads.

### Tested
- **`notExpr.eval` Go-side fallback ([#611](https://github.com/kwad77/pincher/issues/611)).** The `NOT (...)` in-Go evaluation path was 0% covered. SQL pushdown handles most cases but variable-length BFS and RETURN-time post-filter fall through to `notExpr.eval` — a typo flipping `!` to `==` would silently invert every fall-back result. Six new tests pin the contract: `notExpr` inversion, double-negation, NOT over `binaryExpr`, `binaryExpr` AND/OR semantics, and the `matchesWhere` entry point. Same risk class as #607 (`redactSensitiveSlice`).
- **`db.CurrentSchemaVersion` version-drift gate ([#613](https://github.com/kwad77/pincher/issues/613)).** Was 0% covered despite being load-bearing for `pincher list` and `pincher doctor` cross-binary drift detection (#236). Two new tests: pin the `len(schemaMigrations) + 1` relationship (catches `+1`→`+0` regression), and assert it equals the freshly-opened DB's `schema_version` row (catches migration-application skips).

## [v0.31.0] — 2026-05-12 — autoresearcher haul: dead code, NULL pinchQL, redact tests, audit-shape, HTTP method semantics

Five issues filed by an autoresearcher dogfood probe of v0.30 against pincher-repo, fixed in one batch. The bug class is "silently wrong UX": dead unreferenced helper, predicates that match nothing because of SQL tri-state, untested credential redaction, guide misroutes, and a 404 that misleads operators about whether an endpoint exists.

No schema change — all v0.31 work runs on schema v23.

### Fixed
- **`pinchQL n.docstring=""` and `n.is_test=false` now match NULL rows ([#606](https://github.com/kwad77/pincher/issues/606)).** The canonical "find undocumented APIs" demo (`MATCH (n:Function) WHERE n.is_exported=true AND n.docstring="" AND n.is_test=false`) returned 0 rows on every realistic corpus because SQL `col=''` is false for NULL. `condLeafToSQL` and `evalCondition` now expand zero-valued comparisons to `(col IS NULL OR col=?)`. Three nullable bool scan sites converted to `sql.NullInt64`. Same UX class as #473/#578/#591/#593 — predicates that look natural but silently return wrong/empty answers.
- **`guide` routes "find every X without Y" to query, not search ([#608](https://github.com/kwad77/pincher/issues/608)).** New `auditShapePattern` regex catches the structural-audit phrasing ("find every function without a test", "list every endpoint missing auth") that #467's docstring-only trigger missed. Routed before `shapeFix` so the keyword sweep on "error"/"fix" doesn't intercept "find every handler that has no error return". Restores the canonical demo from #438.
- **POST/PUT/DELETE on a known GET-only endpoint returns 405, not "unknown tool" 404 ([#609](https://github.com/kwad77/pincher/issues/609)).** Pre-fix, `POST /v1/dashboard` returned `{"error":{"code":"not_found","message":"unknown tool 'dashboard'","details":{"available_tools":[…22 names…]}}}` — implying the endpoint didn't exist. Now returns `405 Method Not Allowed` with `Allow: GET, HEAD` and a clear message. Same fix applies to /v1/dashboard.{js,css}, /v1/stats, /v1/sessions, /v1/openapi.json, /v1/health. HEAD support added everywhere per RFC 7231 §4.3.2 — `headResponseWriter` wraps the response writer to drop body bytes while preserving every header (Content-Type, ETag, Cache-Control, CSP, Allow). Unblocks container liveness probes that prefer HEAD.
- **`dedupCSymbolsByQN` removed from `internal/ast/extractor.go` ([#605](https://github.com/kwad77/pincher/issues/605)).** Helper had no callers — the dedup it claimed to do is now handled by a different path. Caught by `dead_code`; removal also dropped a stale comment block that referenced it. Closes the dead-code-FP loop on this file (one fewer unreachable function in the audit baseline).

### Tested
- **`redactSensitiveSlice` recursion ([#607](https://github.com/kwad77/pincher/issues/607)).** The slice-recursion path inside `redactSensitiveArgs` was 0% covered despite being reachable from every tool response via `maybeRecordSlowQuery`. Four new tests cover slice-of-maps, slice-of-slice-of-maps, mixed scalar+map slices, and nil input. Coverage 0% → 100% on the helper. A bug here would have silently leaked credentials into the `slow_queries.args_json` column.

## [v0.30.0] — 2026-05-12 — dashboard E2E essentials + #519 umbrella close

Closes umbrella [#519](https://github.com/kwad77/pincher/issues/519) — dashboard hardening — after a 10-release march that ran v0.21 → v0.30 in one continuous session. Four functional issues land here (#550 keyboard shortcuts, #551 export, #554 deep links, #556 ETag); three E2E items defer to a future release with documented rationale; #529 closes as covered-differently.

No schema change — all v0.30 work runs on schema v23.

### Added
- **Keyboard shortcuts ([#550](https://github.com/kwad77/pincher/issues/550), [#604](https://github.com/kwad77/pincher/pull/604)).** `/` focuses the search box, `Esc` closes the project detail panel, `g s/p/o/a/h` switch to search/projects/overview/adrs/sessions, `j`/`k` move the keyboard cursor through project cards (`.kbd-focused` outline). Typing-target guard prevents shortcuts from firing inside inputs/textareas/select. Leader pattern (`g + key`) times out after 1.5s.
- **CSV/JSON export buttons on Projects + Sessions tables ([#551](https://github.com/kwad77/pincher/issues/551), [#604](https://github.com/kwad77/pincher/pull/604)).** New `exportTable(format, kind)` helper builds a Blob from the rendered table data and triggers a browser download. CSV-escapes embedded commas/quotes/newlines per RFC 4180. JSON path emits an array of header-keyed records. Sessions export re-fetches `/v1/sessions` to get the live data; Projects exports the cached `_allProjects`.
- **Deep links to project detail ([#554](https://github.com/kwad77/pincher/issues/554), [#604](https://github.com/kwad77/pincher/pull/604)).** Hash format `#<tab>` extends to `#<tab>/<projectID>` so `https://localhost:18080/v1/dashboard#projects/proj-0042` opens the dashboard with that project's architecture detail panel pre-opened. `openDetail`/`closeDetail` `history.replaceState` (not `pushState`) so back/refresh behave naturally without history pollution. `hashchange` listener restores state on URL edits.
- **Dashboard asset ETag (#556 partial, [#604](https://github.com/kwad77/pincher/pull/604)).** `/v1/dashboard.js` and `/v1/dashboard.css` now respond with `ETag: "<sha256-prefix>"` + `Vary: Accept-Encoding`. Subsequent requests with `If-None-Match: <etag>` get a 304 + zero-body response. Gzip compression was already wired (transparent middleware in the dispatcher); the helper docstring documents why we don't double-gzip.

### Closed (deferred or covered-differently)
- **#520 E2E harness, #524 mobile snapshots, #525 auth E2E** — deferred. A Playwright/rod harness needs a Node toolchain alongside Go in CI, which is out of scope for the umbrella close. The non-runtime gates we added across v0.24-v0.30 (HTML + CSS snapshot tests + 23 dashboard JS template-inspection contract tests) catch most of the regression class an E2E harness would. Reopen when adopting Playwright is itself the desired work item; track separately rather than gating umbrella close on it.
- **#529 backend coverage gate on dashboard.go** — closed as covered-differently. The file is 90% string templates with no executable Go logic; the renderer wrappers are one-liners around `strings.ReplaceAll`. A line-count threshold tracks template byte-count, not regression risk. The 23 snapshot+wiring tests gate the meaningful surface.
- **#521 HTML snapshot test** — already shipped in v0.21.0 (PR #561). Stale tracking issue closed.

### #519 umbrella scoreboard
After 10 releases (v0.21 → v0.30):
- 28 issues closed across the dashboard hardening surface
- 3 issues deferred with documented rationale (E2E harness)
- 23 dashboard-specific test functions added (snapshot, contract, wiring, large-dataset)
- 1 BREAKING API change shipped + documented (v0.25 #537 error envelope)
- 0 regressions in CI across the run

## [v0.29.0] — 2026-05-12 — dashboard interactive polish

Six issues from umbrella #519's interactive-polish batch — empty-state CTAs, loading skeletons, toast variants, custom confirm dialog, configurable refresh interval, and ADR rich render.

No schema change — all v0.29 work runs on schema v23.

### Added
- **Empty-state CTAs ([#540](https://github.com/kwad77/pincher/issues/540), [#603](https://github.com/kwad77/pincher/pull/603)).** New `emptyStateCTA(kind)` helper renders a centered card with icon + title + body + optional command, replacing bare "No X yet" text in Projects, Sessions, and ADRs tabs.
- **Loading skeletons ([#541](https://github.com/kwad77/pincher/issues/541), [#603](https://github.com/kwad77/pincher/pull/603)).** New `skeletonRows(count, kind)` helper + CSS skeleton classes with pulse animation. Wired into `loadSessions` (8 lines) and `loadADRs` (4 cards). Replaces the literal "Loading…" text on async re-fetches.
- **Toast variants + ARIA live region ([#542](https://github.com/kwad77/pincher/issues/542), [#603](https://github.com/kwad77/pincher/pull/603)).** `showToast(msg, kind, opts)` supports success/error/info; default TTL varies by kind (4500ms for errors). Backwards-compatible with the legacy `(msg, ok)` form. `aria-live="polite"` so screen readers announce updates.
- **Custom confirm dialog ([#543](https://github.com/kwad77/pincher/issues/543), [#603](https://github.com/kwad77/pincher/pull/603)).** Promise-returning `showConfirmDialog(title, body, opts)` replaces `window.confirm()` at all three call sites (delete project, delete empty projects, delete ADR). Styled card on translucent backdrop, ARIA `role="dialog"` + `aria-modal="true"`, Escape key cancels, initial focus on Cancel for safer keyboard ergonomics. Destructive variant gets red emphasis.
- **Configurable refresh interval ([#552](https://github.com/kwad77/pincher/issues/552), [#603](https://github.com/kwad77/pincher/pull/603)).** Header `<select>` with 5s/30s/1m/5m/off choices, persisted in `localStorage`. Re-uses v0.28's `_pollers` registry so the visibility-aware pause/resume + the user-controlled cadence compose cleanly. `off` clears all timers.
- **ADR rich render ([#553](https://github.com/kwad77/pincher/issues/553), [#603](https://github.com/kwad77/pincher/pull/603)).** ADR value renders inside `<pre class="adr-val">` with `white-space: pre-wrap` so multi-line content + code snippets keep their line breaks. Still text-only — no markdown parser, no `innerHTML` on raw values; the `esc()` pipeline is unchanged.

## [v0.28.0] — 2026-05-12 — dashboard auto-refresh polish

Four issues from umbrella #519's auto-refresh batch: a projection-banner guard against insufficient data, a freshness indicator + a polling manager that pauses when the tab is hidden, and a three-state dark/light/auto theme toggle. All four touch dashboard JS/CSS only.

No schema change — all v0.28 work runs on schema v23.

### Added
- **Projection banner guard against insufficient data
  ([#544](https://github.com/kwad77/pincher/issues/544),
  [#602](https://github.com/kwad77/pincher/pull/602)).** Pre-fix the projection extrapolated from 2 sessions over <1 day produced "1M tokens/mo" or NaN. Now `computeProjection(sessions)` returns `{needsMoreData: true, days}` below the 7-day floor (rendered as "Need 7+ days of history to project"); `null` on zero savings or invalid timestamps; capped result on monthly projections >100M tokens. Pure function — testable independently of DOM.
- **Freshness indicator + visibility-aware poll manager
  ([#545](https://github.com/kwad77/pincher/issues/545),
  [#546](https://github.com/kwad77/pincher/issues/546),
  [#602](https://github.com/kwad77/pincher/pull/602)).** New `pollManager(label, fn, ms)` wraps `setInterval` and tracks last-fetch time per label in `_lastRefresh`. A `visibilitychange` listener clears every poller's timer when the tab is hidden and fires an immediate refresh + restarts on visible. The header gains a `.updated-ago` badge that re-renders every second from `_lastRefresh` ("just now", "23s ago", "2m ago", "1h ago"). Replaces the bare `setInterval(load, 30000)` + `setInterval(loadProjection, 60000)` calls — bare calls would short-circuit the visibility wrapper.
- **Three-state theme toggle (auto/light/dark)
  ([#549](https://github.com/kwad77/pincher/issues/549),
  [#602](https://github.com/kwad77/pincher/pull/602)).** CSS adds `:root[data-theme="light"]` palette + `@media (prefers-color-scheme: light)` for the system-default path. Header `🌗`/`☀️`/`🌙` button cycles auto → light → dark → auto, persisted via `localStorage`. `applyStoredTheme()` runs before first paint to avoid a dark-flash on light-mode reload. Auto mode = no `data-theme` attr, system query takes over; explicit attr always wins.

## [v0.27.0] — 2026-05-12 — dashboard search polish

Four issues from umbrella #519's UX-polish batch: search-as-you-type, in-snippet match highlighting, sparkline tooltip, and a "Show all" toggle on the architecture detail truncation. All four touch dashboard JS only — no schema or API changes.

No schema change — all v0.27 work runs on schema v23.

### Added
- **Search-as-you-type with debounce
  ([#547](https://github.com/kwad77/pincher/issues/547),
  [#601](https://github.com/kwad77/pincher/pull/601)).** New `debounce(fn, wait)` wrapper + 200ms-debounced `debouncedSearch` bound to the search input via `data-action-input`. Pre-fix Enter was the only trigger; pre-fix typing "supervisor" sent zero requests. Post-fix typing "supervisor" sends one request after the user pauses. Skips queries shorter than 2 chars to avoid BM25-rank noise. Combines with #539's `tabFetch` so an in-flight search is aborted by the next keystroke.
- **Snippet highlighting via `<mark>`
  ([#548](https://github.com/kwad77/pincher/issues/548),
  [#601](https://github.com/kwad77/pincher/pull/601)).** New `highlightSnippet(snippet, query)` escapes the snippet for HTML, then wraps each query token in `<mark>` for visible highlighting. Pure-string substitution after escape — never touches `innerHTML` on raw snippet content, so no XSS surface beyond what `esc()` already gates. Strips FTS5 operators (quotes, asterisks, AND/OR, parens) from the highlight pass so `"login flow"` highlights `login` and `flow` (not the surrounding quotes). Wired into both name + snippet/signature render paths.
- **Sparkline per-point tooltip
  ([#555](https://github.com/kwad77/pincher/issues/555),
  [#601](https://github.com/kwad77/pincher/pull/601)).** Mousemove handler over the sparkline SVG computes the nearest data point via cursor-x → index mapping, looks up the cached session record, and renders a floating tooltip with date + tokens-saved + call count. Touch support: tap shows tooltip, mouseleave hides. Tooltip flips left when it would clip the right edge.
- **Architecture detail "Show all" toggle
  ([#533](https://github.com/kwad77/pincher/issues/533),
  [#601](https://github.com/kwad77/pincher/pull/601)).** Pre-fix the architecture detail panel hard-truncated entry-points at 8 and hotspots at 10 with no indication; users had no way to know the rest existed. Post-fix the section header shows "X of Y" when the list exceeds the cap, and a "Show all" button expands inline to a higher cap (50). Per-detail-section state in `_detailExpanded` so multiple open panels remember independently.

## [v0.26.0] — 2026-05-12 — dashboard reliability

Four issues from umbrella #519's reliability batch: ADR length limits + the per-tab error/abort wiring that makes a failed fetch visible without leaving the tab stuck on "loading…". The dashboard JS gains an AbortController-backed fetch wrapper so rapid tab switching no longer races stale responses onto the wrong tab.

No schema change — all v0.26 work runs on schema v23.

### Added
- **ADR field length limits + backend validation
  ([#534](https://github.com/kwad77/pincher/issues/534),
  [#600](https://github.com/kwad77/pincher/pull/600)).** Server-side: `action=set` now enforces key ≤256 chars and value ≤16 KB and returns the error through the v0.25 envelope. Form-side: `<input maxlength="256">` + `<textarea maxlength="16384">` plus a live counter that turns amber at 85% and red on overflow. Pre-fix a paste-of-an-entire-transcript blew up the row size and the `text` column accepted it silently.
- **Per-tab error state — fetch failure no longer stuck on "loading…"
  ([#538](https://github.com/kwad77/pincher/issues/538),
  [#526](https://github.com/kwad77/pincher/issues/526),
  [#600](https://github.com/kwad77/pincher/pull/600)).** New `setTabError(elementID, message, retryFn)` swaps the per-tab "loading…" placeholder for the error message + a Retry button. New `extractErrMsg(response)` reads `body.error.message` (the v0.25 envelope) with fallback to bare `body.error` (pre-v0.25 transitional shape) so partial-rollout proxies still yield useful messages. Wired through `loadProjects`, `loadSessions`, `loadADRs`. Closes #526 (the JS-side fetch error path test) since the per-tab wiring it gated on now exists — covered by `TestDashboardJS_HasPerTabErrorState` rather than a JS-runtime harness (we have no headless browser in CI).
- **XHR abort on tab switch — late responses can't overwrite newer tabs
  ([#539](https://github.com/kwad77/pincher/issues/539),
  [#600](https://github.com/kwad77/pincher/pull/600)).** New `tabFetch(tab, url, opts)` registers a per-tab `AbortController` and aborts the previous one for that tab before issuing the new fetch. `showTab` aborts every other tab's controller on switch. Catch handlers in each `load*()` short-circuit on `AbortError` so a superseded request quietly disappears instead of writing stale data into the now-active tab. Worst case pre-fix: spam-clicking between Projects → Sessions → Search left N parallel pending requests, and a late Projects response could overwrite the Sessions table.

## [v0.25.0] — 2026-05-12 — dashboard API hardening

Six issues from umbrella #519's API-shape batch: pagination on three GET endpoints, ETA on index-progress, dashboard-version probe, and a standardized error envelope. The error envelope is the **breaking change** in this release.

No schema change — all v0.25 work runs on schema v23.

### Changed (BREAKING)
- **Standardized error envelope across every /v1/ response
  ([#537](https://github.com/kwad77/pincher/issues/537),
  [#599](https://github.com/kwad77/pincher/pull/599)).** Pre-v0.25 error responses were `{"error": "<text>"}` (the v0.22.1 transitional shape). v0.25 returns `{"error": {"code": "<snake_case>", "message": "<text>", "details": {...optional}}}` so clients can pattern-match on the machine-readable code instead of substring-checking the message text. Standard codes: `bad_request`, `not_found`, `unauthorized`, `rate_limited`, `method_not_allowed`, `internal_error`, `tool_error`. The OpenAPI `Error` component schema (#581) was updated in lockstep — generated SDKs need a regen. Hand-written clients reading `body.error` as a string need to read `body.error.message` instead.

### Added
- **GET /v1/projects pagination
  ([#530](https://github.com/kwad77/pincher/issues/530),
  [#599](https://github.com/kwad77/pincher/pull/599)).** Accepts `?limit=&offset=` (default 50, max 200). Returns `{projects, total, has_more}` so the dashboard can render a "Load more" button without re-counting. In-Go slicing for now (the projects list is bounded; the v0.24 large-dataset test gates the cliff) — push to SQL `LIMIT/OFFSET` if/when ListProjects becomes the bottleneck.
- **GET /v1/sessions limit parameter
  ([#531](https://github.com/kwad77/pincher/issues/531),
  [#599](https://github.com/kwad77/pincher/pull/599)).** Accepts `?limit=` (default 90, max 500). Pre-v0.25 the 90-row count was hardcoded server-side. Returns the same `{sessions, total, has_more}` envelope as `/v1/projects`. Sparkline-friendly defaults preserved.
- **POST /v1/search pagination
  ([#532](https://github.com/kwad77/pincher/issues/532),
  [#599](https://github.com/kwad77/pincher/pull/599)).** Accepts `limit` (default 20, max 500) + `offset` (default 0, max 5000) in the body. Returns `{results, count, total, has_more, offset, limit, ...}`. Pagination semantics: BM25-ranked top-(offset+limit), serve [offset:offset+limit]. `total` is a lower bound when has_more is true (FTS5 stops at fetchLimit). The dashboard renders "Showing 50 of 1234+" when `has_more` is true.
- **POST /v1/index-progress ETA
  ([#535](https://github.com/kwad77/pincher/issues/535),
  [#599](https://github.com/kwad77/pincher/pull/599)).** Returns `started_at` + `elapsed_ms` + `files_per_sec` + `eta_ms` alongside the existing `files_done`/`files_total`/`active`. ETA is `(total - done) / rate` where `rate = done / elapsed`. Null for inactive projects (no in-memory progress entry) so clients render "estimating…" rather than infinity. New indexer method `GetProgressDetail` exposes `IndexProgress.StartedAtUnix` to the HTTP handler.
- **GET /v1/health dashboard_version field
  ([#536](https://github.com/kwad77/pincher/issues/536),
  [#599](https://github.com/kwad77/pincher/pull/599)).** Equals server `version` in this release; carried as a separate field so they can advance independently later. The dashboard JS bakes its own build version in at render time and polls health to detect "your tab is running stale JS against a newer server" — common after a binary upgrade because the dashboard JS `Cache-Control: max-age=600` can keep stale JS in the browser for 10 minutes.

## [v0.24.0] — 2026-05-12 — dashboard test foundation

Patch-shaped minor — four test additions closing the umbrella #519 dashboard hardening's "no test coverage" gap. v0.23 already shipped the runtime hardening (CSP, basepath, error envelope); v0.24 surrounds those with regression tests so future edits surface drift immediately. Plus one small renderer change: `renderDashboard*` now normalize the basepath through `normalizeBasePath`, so a trailing slash on the input prefix doesn't silently produce double-slashed fetch URLs.

No schema change — all v0.24 work runs on schema v23.

### Added
- **Dashboard CSS regression test + byte snapshot
  ([#522](https://github.com/kwad77/pincher/issues/522),
  [#598](https://github.com/kwad77/pincher/pull/598)).** `TestDashboardCSS_RegressionSnapshot` asserts 200 + `text/css` + `Cache-Control` + body-length bounds + exact-byte snapshot under `testdata/dashboard/dashboard.css`. Mirrors the v0.21 #521 pattern. Regenerate after intentional CSS edits with `-update-dashboard-css-snapshot`.
- **Dashboard JS basepath substitution edge cases
  ([#523](https://github.com/kwad77/pincher/issues/523),
  [#598](https://github.com/kwad77/pincher/pull/598)).** Table-driven `TestDashboardJS_BasepathSubstitution` across empty, simple, trailing-slash, deep-path, BP-contains-`/v1/`, URL-encoded chars. Companion `TestDashboardJS_BasepathSubstitution_HTMLAndJSAgree` pins that `<script src>` in the HTML and `const BP` in the JS use byte-identical prefixes — drift between the two is the exact failure mode that motivated splitting `renderDashboard` from `renderDashboardJS`. Renderer change: trailing slashes are now normalized away by both renderers, eliminating a footgun where `renderDashboardJS("/pincher/")` would emit a BP that produced double-slashed fetch URLs (`BP + '/v1/...'` → `/pincher//v1/...`).
- **Per-endpoint API contract tests
  ([#528](https://github.com/kwad77/pincher/issues/528),
  [#598](https://github.com/kwad77/pincher/pull/598)).** Eleven new tests under `TestEndpointShape_*` and `TestEndpointNegative_*` covering the ad-hoc `/v1/` routes that don't flow through `registerTools` (and so don't get OpenAPI parity gates from #558/#581): `/v1/health`, `/v1/stats`, `/v1/sessions`, `/v1/projects`, `/v1/openapi.json`, `/v1/index-progress`, plus DELETE `/v1/projects` and DELETE `/v1/projects/empty`. Shape tests assert documented top-level keys present; negative tests assert malformed bodies return 4xx with a JSON `{error}` envelope, not 500 with a leaked stack. Includes a `#334`-class regression guard that `/v1/sessions` returns `[]` not `null` when empty.
- **Large-dataset fixture + perf cliff guard
  ([#527](https://github.com/kwad77/pincher/issues/527),
  [#598](https://github.com/kwad77/pincher/pull/598)).** `TestDashboard_LargeDataset` seeds 1000 projects + 1000 sessions + 5000 symbols, hits `/v1/stats` + `/v1/sessions` + `/v1/projects`, and gates each on a 5s wallclock budget + per-endpoint payload-size bound. Catches superlinear regressions and pagination cliffs. Current observed: stats 0.5ms / 219 B, sessions 0.5ms / 14.5 KB, projects 2.5ms / 188 KB. The bounds intentionally trail v0.25's pagination work (#530/#531/#532) — once those land, tighten the bounds.

## [v0.23.0] — 2026-05-12 — HTTP gateway hardening + pinchQL data integrity

Patch-shaped minor — four fixes from a continuous v0.22 dogfood probe of the HTTP gateway and pinchQL deep queries. Net effect: container orchestrators can now liveness-probe pincher behind `--http-key`, the bare URL routes to the dashboard, and pinchQL stops silently inflating result sets via two distinct edge-cases (multi-sourced edges + column-vs-column comparisons).

The originally-planned "dashboard hardening" theme rolls forward to v0.24.0 — this release was the dogfood haul that surfaced through the previous round's probes.

No schema change — all v0.23 work runs on schema v23.

### Fixed
- **HTTP `/v1/health` + `/v1/openapi.json` bypass bearer auth
  ([#588](https://github.com/kwad77/pincher/issues/588),
  [#594](https://github.com/kwad77/pincher/pull/594)).** Container orchestrators (Docker, Kubernetes, fly.io, ECS) ping these as liveness probes — they can't carry a bearer token without significant config gymnastics. Pre-fix operators behind `--http-key` either couldn't liveness-probe at all, or had to drop their auth. Both paths are documentation-shaped (version + auth_required + binary_stale; the dynamic spec) and don't leak project state. Every other endpoint still enforces auth.
- **HTTP root `/` redirects to `/v1/dashboard`
  ([#590](https://github.com/kwad77/pincher/issues/590),
  [#595](https://github.com/kwad77/pincher/pull/595)).** Pre-fix the bare URL hit "method not allowed — use POST /v1/{tool}" — a confusing front door for operators typing the URL in a browser. 302 redirect (not 301) so we can change the front door later without poisoning bookmarks. Honors basepath: `/pincher/` → `/pincher/v1/dashboard`.
- **pinchQL: dedup multi-sourced CALLS/READS edges in MATCH JOIN
  ([#591](https://github.com/kwad77/pincher/issues/591),
  [#592](https://github.com/kwad77/pincher/pull/592)).** The edges table stores one row per source tag (`per_file` / `resolve_pass` / `binding_pass`) by design (#475 / #565), but pinchQL JOIN was returning all of them — silently inflating caller counts. Pre-fix repro on pincher-repo: `MATCH (a)-[:CALLS]->(b) WHERE a.name="mustProject" AND b.name="errResult" RETURN a.id, b.id` → 2 rows with identical IDs. Now dedup at engine level on `(from_id, to_id, kind)`, keeping highest-confidence variant. Same UX class as #473 / #578: silent data inflation, agent makes decisions on the wrong count.
- **pinchQL: rejects + warns on column-vs-column comparisons in WHERE
  ([#593](https://github.com/kwad77/pincher/issues/593),
  [#596](https://github.com/kwad77/pincher/pull/596)).** Pre-fix `WHERE a.col <op> b.col` silently parsed as always-true — RHS treated as unmatched literal — so the predicate inflated result sets. Discovered while validating the v0.15.5 cross-language scoping (#436): canonical `WHERE a.language <> b.language` returned Go→Go pairs. Fix surfaces a warning naming the offending clause + makes evaluation return false (consistent with #473 unknown-property handling).

## [v0.22.1] — 2026-05-12 — HTTP error response contract honored

Patch — HTTP gateway error responses now match the OpenAPI `Error` schema shipped in v0.22.0 (#582). Pre-fix the dispatcher wrote raw `errResult()` text under a `Content-Type: application/json` header, breaking the contract that #582 explicitly promised.

### Fixed
- **HTTP error responses are JSON `{error}` matching OpenAPI Error schema
  ([#586](https://github.com/kwad77/pincher/issues/586),
  [#587](https://github.com/kwad77/pincher/pull/587)).** Discovered during v0.22 dogfood probe spinning up `pincher --http 127.0.0.1:18080`; the first `/v1/search` call without a session project returned 400 with plain-text body. Generated SDKs against `/v1/openapi.json` would have choked on the body shape mismatch. Fix wraps `tc.Text` in `{"error": tc.Text}` when `result.IsError`; success path unchanged.

## [v0.22.0] — 2026-05-12 — dogfood haul + OpenAPI contracts

Patch-shaped minor — five fixes and one feature, all surfaced in a single ~3-hour dogfood probe of v0.21.0. Headline: every `/v1/<tool>` endpoint now ships a real OpenAPI response schema with typed fields, a shared `_meta` envelope component, and an `Error` component for the default response — generated SDKs from `/v1/openapi.json` get typed response models per endpoint instead of `{ [k: string]: any }`. The other four fixes are dogfood-driven precision: dead_code's FP triangle's last leg gets a real-world close (the v0.21 README claim retroactively becomes true), fetch stops corrupting markdown, doctor stops blowing the MCP token cap on multi-project installs, and pinchQL stops silently accepting unknown function names.

No schema change — all v0.22 work runs on schema v23.

### Added
- **Real OpenAPI response contracts + shared Meta/Error components
  ([#581](https://github.com/kwad77/pincher/issues/581),
  [#582](https://github.com/kwad77/pincher/pull/582)).** v0.20 (#560)
  landed dynamic request-side schemas; v0.22 completes the response
  side. Every `/v1/<tool>` endpoint declares typed top-level fields,
  required-field list, and a `_meta` `$ref` to a shared component.
  Default response on every endpoint references a shared `Error`
  schema. Two new gate tests
  (`TestOpenAPI_EveryToolHasNonPlaceholderResponseSchema` +
  `TestOpenAPI_HasSharedMetaAndErrorComponents`) prevent future
  endpoints from regressing to the bare `{type: object}` placeholder.
  Closes the response-side parity claim made in v0.20.

### Fixed
- **dead_code: file-scope composite-literal function-value binding
  ([#576](https://github.com/kwad77/pincher/issues/576),
  [#577](https://github.com/kwad77/pincher/pull/577)).** The v0.21
  binding pass (#565) only handled assignment-statement bindings
  (`s.handler = fn`); file-scope composite-literal bindings
  (`var X = T{Field: fn}` — the canonical "registry of handlers"
  pattern) were silently uncovered. Pincher's own
  `var CodexTarget = Target{DetectFn: detectCodex, …}` exposed the
  gap during v0.21 dogfood. New `extractGoFileLevelReads` walks
  identifier references inside top-level var/const initializer
  expressions; the resolveReads binding pass converts function-value
  READS to confidence-0.4 CALLS edges. The v0.21 README limitation
  rewrite claimed `T{Handler: someFn}` was covered — with this PR
  landed, that claim is true.
- **fetch: corrupts markdown — eats `>` chars + runs HTML stripper on
  text/markdown URLs
  ([#579](https://github.com/kwad77/pincher/issues/579),
  [#580](https://github.com/kwad77/pincher/pull/580)).** Three bugs
  from one root cause. `extractTextFromHTML` ran on every fetched
  URL regardless of Content-Type → markdown bodies got tag-stripped
  → arrows (`=>`), generics (`Vec<T>`), blockquotes, and literal
  angle-bracket content all silently lost characters. Discovered
  fetching pincher's own CHANGELOG: `/v1/<tool>` rendered as `/v1/ `,
  every `=>` lost its `>`. Fix: handler dispatches on
  `Content-Type` (text/markdown / text/plain skip stripping); new
  `firstMarkdownH1` parses the title from `# Title`; the HTML
  scanner only consumes `>` when it actually closes a tag we
  entered, defending even on real HTML.
- **doctor: handler caps projects + failures globally
  ([#575](https://github.com/kwad77/pincher/issues/575),
  [#583](https://github.com/kwad77/pincher/pull/583)).** Pre-fix the
  handler iterated every project and pulled `top` failures per
  project; on a 125-project install with default top=10 the response
  ballooned to ~119 KB and exceeded the MCP per-call token cap. Two
  caps: projects list capped at `top` (sorted by symbol count desc
  so the largest projects — most likely to surface a problem — are
  kept), and extraction failures capped GLOBALLY at `top` instead of
  per-project. Overflow surfaced in `projects_truncated` /
  `extraction_failures_truncated` so the caller knows. Discovered
  same-session as #573 shipped — first call against my own install
  repro'd it.
- **pinchQL: rejects unknown function calls in RETURN
  ([#578](https://github.com/kwad77/pincher/issues/578),
  [#584](https://github.com/kwad77/pincher/pull/584)).** Pre-fix
  `RETURN LENGTH(f.docstring)` parsed silently — `LENGTH` became a
  bare variable ref, the `(f.docstring)` was tolerated, every row
  evaluated to null. Same UX class as #473 (typo'd properties):
  malformed input silently returns "an answer" that isn't. Fix
  surfaces a clear pinchQL error naming the offender + the supported
  aggregator set.

## [v0.21.0] — 2026-05-11 — function-value-binding edges + build-tag fan-out + admin tools on MCP/HTTP

Minor — the v0.21 theme is **closing the dead_code FP triangle's last leg + cross-platform call-graph correctness + finishing the API parity work started in v0.20.** Function values bound to struct fields (`s.handler = fn`) no longer false-flag the bound function as dead; build-tag duplicate-implementation siblings (`web_windows.go` / `web_unix.go` pattern) both surface as inbound-reachable instead of just the lex-smallest variant; and `doctor` / `rebuild-fts` / `self-test` graduate from CLI-only to MCP+HTTP, exposed via the dynamic dispatcher built in v0.20. A CLI↔MCP parity gate prevents future user-facing CLI commands from being silently CLI-only.

No schema change — all v0.21 work runs on schema v23.

### Added
- **Function-value-binding CALLS edges
  ([#565](https://github.com/kwad77/pincher/issues/565),
  [#570](https://github.com/kwad77/pincher/pull/570)).** Closes the
  third leg of the dead_code FP triangle (after #423 receiver-type in
  v0.19 and #493 interface-dispatch in v0.20). When `s.handler = fn`
  binds a function value to a struct field, the binding pass now
  emits a low-confidence (0.4) CALLS edge from the surrounding
  function to `fn` so dead_code stops false-flagging `fn` as having
  no callers. Polymorphic-method blocklist applies to the binding
  pass too — `s.action = strFn` doesn't false-bind every project-local
  String method.
- **Build-tag duplicate-implementation siblings get full inbound edges
  ([#566](https://github.com/kwad77/pincher/issues/566),
  [#572](https://github.com/kwad77/pincher/pull/572)).** Pre-fix
  `pickCanonical` chose the lex-smallest variant when multiple files
  defined the same QN — the `web_windows.go` / `web_unix.go` /
  `web_darwin.go` pattern meant only `web_darwin.go`'s
  `platformPIDAlive` got an inbound CALLS edge from `launch()`; the
  windows + unix variants surfaced as dead. Cheap filename-based
  heuristic (`isBuildTagSibling`) detects siblings during
  `resolveCalls` and fans out the edge to ALL variants. Makes
  cross-platform Go projects' dead_code reports honest.
- **Run added to the polymorphic-method blocklist
  ([#567](https://github.com/kwad77/pincher/issues/567),
  [#571](https://github.com/kwad77/pincher/pull/571)).** `cmd.Run()`
  on `*exec.Cmd`, `srv.Run()` on `*http.Server`, and goroutine pool
  `Run()` shapes were false-binding to any in-project Method named
  `Run` via the trailing-component fallback. Adds Run to
  `isPolymorphicInterfaceMethodName` symmetric across both the
  call-pass and binding-pass — the trace inbound on `(*Supervisor).Run`
  drops from 6 noisy callers to the 1 real one.
- **doctor / rebuild_fts / self_test as MCP tools
  ([#558](https://github.com/kwad77/pincher/issues/558) phase 2,
  [#573](https://github.com/kwad77/pincher/pull/573)).** The three
  CLI-only admin commands now register as MCP tools and surface
  through `/v1/<tool>` via the dynamic dispatcher built in v0.20.
  Dashboards and ops automations can poll diagnostic state without
  shelling out. `rebuild_fts` defaults to dry-run (returns row count);
  pass `confirm=true` to trigger the rebuild.
- **CLI↔MCP parity gate (#558 phase 3, #573).**
  `TestCLISurface_HasMCPParity` enforces that every user-facing CLI
  subcommand has an MCP equivalent, with an explicit ops-only
  carve-out (`web`, `supervised`, `update`, `project`, `health-check`).
  Future CLI-only data commands fail CI. Closes #558.

## [v0.20.0] — 2026-05-11 — JS AST default-on + interface-dispatch precision + parity foundation

Minor — the v0.20 theme is **multi-language coverage Tier 1 + the dead_code FP triangle's third leg + API drift gates.** JS files now extract through the pure-Go AST extractor by default (was opt-in via `PINCHER_EXPERIMENTAL_JS_AST=1`), surfacing classes with their methods, correctly classifying arrow-bound consts as Functions, and respecting ES2015+ export semantics. The `dead_code` precision arc that started in v0.18 (init/TestMain/main filter) and v0.19 (receiver-type field-method resolution) closes with interface-dispatch satisfaction analysis — methods reachable only via interface dispatch stop showing as dead. The HTTP REST gateway gets a parity gate so newly-added MCP tools can never silently disappear from the OpenAPI spec again.

Schema v23 — adds the `interface_methods` table for the dead_code interface-reachability heuristic.

### Added
- **JS AST extractor flipped to default-on
  ([#562](https://github.com/kwad77/pincher/issues/562),
  [#563](https://github.com/kwad77/pincher/pull/563)).** Tier 1 of the
  v0.20 multi-language coverage sequence. `.js` / `.mjs` / `.cjs` now
  extract through `tdewolff/parse/v2/js` instead of the regex
  extractor — produces Class symbols with their child Methods,
  promotes arrow-bound consts (`const f = () => {}`) to the Function
  kind, descends `const handlers = {onClick: () => {...}}` patterns
  to surface object-method symbols, and applies ES2015+ module
  semantics (only `export`-prefixed decls are exported). Opt-out via
  `PINCHER_DISABLE_JS_AST=1`; the legacy `PINCHER_EXPERIMENTAL_JS_AST=0`
  is honored for one release as a compat path. Both env vars go away
  in v0.21.
- **Interface-dispatch dead_code precision
  ([#493](https://github.com/kwad77/pincher/issues/493),
  [#564](https://github.com/kwad77/pincher/pull/564)).** Closes the
  third leg of the dead_code FP triangle (after #423 receiver-type
  field-method resolution in v0.19 and #492 init/TestMain/main filter
  in v0.18). New `interface_methods` table populated from Go
  Interface symbols' declared method-name sets (schema v23). The
  dead_code SQL excludes Methods whose name matches any interface
  method declared in the same project — cheap heuristic that
  over-includes (a Method named String gets spared even if no
  interface uses it) but the dead_code direction prefers
  false-negatives over false-positives (suggesting deletion of a
  method actually called via interface dispatch breaks runtime
  silently). Cypher engine's `whereExpr.eval` family — the canonical
  repro from #493 — stops showing as dead.
- **OpenAPI spec dynamic from registered handlers + parity gate
  ([#558](https://github.com/kwad77/pincher/issues/558) phase 1,
  [#560](https://github.com/kwad77/pincher/pull/560)).** The
  hardcoded 15-tool slice in `openAPISpec` was silently dropping 4
  registered tools (`dead_code`, `guide`, `neighborhood`, `init`)
  even though they were reachable via the generic `/v1/<tool>`
  dispatcher — invisible to OpenAPI consumers (Postman imports,
  Cursor, copilots). Spec now iterates `s.handlers` in sorted
  order; per-tool description + InputSchema pulled from the
  registered `mcp.Tool` so OpenAPI mirrors what the agent sees in
  `tools/list`. New `TestOpenAPI_ParityWithRegisteredHandlers`
  fails CI when a future tool is added without surfacing in the
  spec.

### Fixed
- **JS AST extractor — four polish bugs blocking promote-to-default
  ([#557](https://github.com/kwad77/pincher/pull/557)).** Arrow
  functions and function expressions assigned to a binding now
  promote the binding's kind to Function (was Variable, breaking
  call-graph reachability); top-level decls only flag
  `IsExported=true` when prefixed with `export` (ES2015+ semantics);
  object-literal arrow methods (`onClick: (ev) => ...` — React
  event-handler shape) now extract; same binding never double-emits
  Function + Variable.

### Schema
- **v22 → v23**: new `interface_methods` table
  `(project_id, interface_id, method_name)` PK with reverse-lookup
  index on `(project_id, method_name)`. Cascade-deleted by
  `DeleteSymbolsForFile`. No re-index required after upgrade — the
  table is populated incrementally as files are re-extracted; the
  next `pincher index --force` (or any per-file change picked up by
  the watcher) populates the table for a project.

## [v0.19.0] — 2026-05-11 — receiver-type tracking + savings honesty

Minor — the v0.19 theme is **deeper resolution + honest counters.** Receiver-type tracking lets `dead_code` stop false-positiving methods called via struct fields (the `Server.idx.Watch` family); the polymorphic-method blocklist (Close/String/Run) no longer over-drops calls when we know the receiver type. `_meta.baseline_method` makes every response state which Read it replaced — or honestly say "none" instead of fabricating a zero.

Schema v22 — adds `pending_edges.receiver_type` column + `struct_fields` table for the Go receiver-type resolver.

### Added
- **Go receiver-type tracking, four-piece stack
  ([#423](https://github.com/kwad77/pincher/issues/423)).** Closes the
  long-standing dead_code FP family where methods called via struct
  fields (`s.cache.Close()`, `s.idx.Watch()`) were either dropped by
  the polymorphic-method blocklist or false-bound across same-name
  methods on different types. Pieces: (1) extractor stamps
  `ExtractedSymbol.Fields` for struct symbols + `ExtractedEdge.ReceiverType`
  for CALLS edges from method bodies (#514), (2) schema v22 persists
  both into a new `struct_fields` table and a `pending_edges.receiver_type`
  column (#517), (3) `resolveCalls` consults receiver_type +
  struct_fields BEFORE the polymorphic-method blocklist to bind
  precisely (#518). Same-package only for v0.19 — qualified types
  (`io.Writer`, `*foo.Bar`) need import-graph awareness and are
  deliberately deferred.
- **`_meta.baseline_method` stamp on every tool response
  ([#477](https://github.com/kwad77/pincher/issues/477)).** Three values:
  `"full_file_read"` for tools that replace a Read of source files
  (search/symbol/symbols/context/trace/changes/dead_code/neighborhood/
  query), `"partial_read"` for repeat-access (per-session dedup'd via
  the #478 accessedFiles set), and `"none"` for admin / orientation
  tools that have no Read alternative (architecture/schema/list/stats/
  health/guide/adr/init/index/fetch). Tools stamped `"none"` emit
  `tokens_saved: null` (not 0) and suppress the `savings:` human-
  readable line — there's no honest baseline to draw against, so the
  field is explicitly absent rather than zeroed. The session stats
  accumulator also skips them, so cumulative `tokens_saved` no longer
  silently includes "saved zero on architecture, repeated 100×"
  contributions. Closes the SAVINGS_HONESTY thread that started in
  v0.17.0 (#476/#478/#479). New classification gate test
  (`TestBaselineMethodForTool_AllRegisteredToolsClassified`)
  prevents future tools from drifting unclassified.

## [v0.18.0] — 2026-05-11 — failure-as-pedagogy v2 + dopamine + tool-output trust

Minor — the v0.18 theme is **failure-as-pedagogy v2 + dopamine + tool-output trust.** v0.17 made every silent zero in pinchQL teach the agent (#473); v0.18 extends that pedagogy across the entire tool surface (tool args, property values, MATCH labels, search regex), adds an occasional dopamine signal so the agent + user see the savings tier they crossed, and tightens two failure surfaces — `dead_code` no longer cries wolf on Go runtime-invoked symbols, and `changes` no longer inflates blast radius into an unfittable payload when a single function changes in a large file.

Schema v21 — adds the `celebrations` table for one-shot per-installation milestone tracking.

### Added
- **Occasional milestone celebration in `_meta`
  ([#494](https://github.com/kwad77/pincher/issues/494)).** Tool
  responses now surface a one-line `_meta.celebration` when cumulative
  all-time `tokens_saved` crosses a tier
  (100k / 500k / 1M / 5M / 10M / 50M / 100M / 500M / 1B). Each tier
  fires exactly once per installation (persisted in a new
  `celebrations` table, schema v21). When a single huge call vaults
  past multiple tiers, only the highest one fires — no spam. 5×
  spacing means real milestones, not nagging.
- **guide: new `tool_audit` shape for empirical tool-output investigation
  ([#497](https://github.com/kwad77/pincher/issues/497)).** Pre-fix,
  asking guide "find false positives in dead_code" returned the generic
  "fix" recipe (search for the tool's source, read it, trace callers) —
  the wrong investigation. New shape detects a known tool name + audit
  keyword combination. Recipe: (1) run the audited tool, (2) `trace`
  inbound on each result to verify, (3) `context` the unexpected ones
  to identify the missed-edge mechanism.

### Changed
- **neighborhood: description rewritten to lead with what it ISN'T
  ([#498](https://github.com/kwad77/pincher/issues/498)).** The name
  suggests graph adjacency; the tool returns same-file symbols.
  Description now leads with "Returns same-file symbols, NOT graph
  adjacency" and points at `trace direction=both` for what agents
  actually want. Pairs with #500's unknown-args warnings — passing
  `depth=1` (the natural-but-wrong arg) now surfaces in
  `_meta.warnings`. Full rename deferred.
- **list: filtered_out lump-sum split into per-reason breakdown
  ([#505](https://github.com/kwad77/pincher/issues/505)).** Pre-fix,
  `"filtered_out": 116` was opaque. Now: `filtered_breakdown:
  {dead_path, inactive, low_edges}` always present, plus
  `_meta.filter_diagnosis` naming the recovery args
  (`include_dead=true`, `active=false`, `min_edges=0`) only when
  something was filtered. Healthy responses stay clean.

### Fixed
- **All tools: unknown args surface in `_meta.warnings` instead of
  silent ignore ([#499](https://github.com/kwad77/pincher/issues/499)).**
  Pre-fix, calling `neighborhood id=... depth=1` silently dropped the
  `depth` arg. Same failure family as #473 (typo'd pinchQL properties).
  Per-tool arg allow-lists computed once from the registered
  InputSchema on first call. Zero overhead when args are valid.
- **query: empty-result diagnosis extends to enum-shaped property
  values + MATCH-pattern labels
  ([#501](https://github.com/kwad77/pincher/issues/501)).** Pre-fix,
  `WHERE n.kind = 'init'` and `MATCH (n:Funtion)` both silently
  returned 0 rows. Now: when a query returns 0 rows AND filters on
  `kind` / `language` (incl. alias `label`) with `=`, OR uses one of
  those values as a MATCH-pattern label, the engine queries the
  project's distinct values and surfaces a warning naming the offender
  + listing actually-observed values. Gated on `Total == 0`.
- **search: regex meta-pattern (".*", ".+", ".?") in query rejected
  with redirect to `query` tool
  ([#509](https://github.com/kwad77/pincher/issues/509)).** Pre-fix,
  `search query="handle.*Changes"` leaked `SQL logic error: fts5:
  syntax error near "."`. Now returns a friendly redirect to `query`
  with pinchQL `WHERE n.name =~ 'pattern'`. Narrow check — single `.`
  (e.g. `db.Open`) and prefix wildcards (`auth*`) continue to work.
- **changes: blast radius now intersects diff hunks with symbol line
  ranges ([#502](https://github.com/kwad77/pincher/issues/502)).**
  Pre-fix, every symbol in any changed file was treated as "changed"
  — a 3-function PR on `server.go` inflated `changed_symbols` to 240,
  BFS to 472 critical, payload to 345 KB (didn't fit in context).
  Now: hunk headers from `git diff --unified=0` are intersected with
  each symbol's `[StartLine, EndLine]`. Same 3-PR workload:
  changed_symbols 240 → 3, payload 345 KB → ~3 KB. Fallback to the
  pre-fix behaviour when hunk parsing fails so the tool stays usable.
- **dead_code precision: Go init / TestMain / main filtered
  ([#492](https://github.com/kwad77/pincher/issues/492)).** Runtime-
  invoked Go symbols (`init` called at package load, `TestMain` by
  `go test`, `main` by the runtime) have no inbound CALLS edges by
  definition — necessarily false positives. Language-gated +
  name-list bounded so legitimately-dead symbols in other languages
  aren't hidden. Interface-dispatch false positives ([#493]) tracked
  for v0.19 — needs satisfaction analysis.

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
