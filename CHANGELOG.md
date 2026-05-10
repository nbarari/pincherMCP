# Changelog

All notable changes to pincherMCP. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/) — once 1.0 ships, schema
breaking changes will be major bumps and tool-contract additions will be
minors.

## [Unreleased]

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
