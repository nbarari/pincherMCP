# Changelog

All notable changes to pincherMCP. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/) — once 1.0 ships, schema
breaking changes will be major bumps and tool-contract additions will be
minors.

## [Unreleased]

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
