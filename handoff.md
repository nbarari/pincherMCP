# Handoff — 2026-05-16 late evening

## Resume in 30 seconds

You are mid-loop. The autonomous loop directive from the user is in effect:

> **"begin a loop and not stop until we get right next to .7 release."**
> **"we need to uncover as much as possible between .65-.69 so that .70 is as stable as possible."**
> **"Restart out find/fix/iterations loop. I think that provided a ton of value. so continue with .66 bug #1212 and the handoff"**

**The biggest open thing is #1231** (critical-class persistence-loss bug discovered this session — see below). Picking it up first this next session is the highest-leverage move.

## What shipped this session (2026-05-16 late evening continuation)

Four v0.66 PRs merged this session, on top of the six already in flight:

- **#1229** (closes #1208) — TS extractor drops `function name(...): T;` overload signatures + suppresses `qualified_name_collision` diagnostic for TS wholesale (mirrors #1207 Markdown carve-out; residual cases are AST-tier work)
- **#1223** (closes #1204) — C/C++ `cMacroRE` restricted to column 0; indented `UE_LOG(LogTemp, ...)` no longer over-emitted as `LogTemp` Symbols
- **#1222** (closes #1209) — `doctor` `nested-project` advisory surfaces strict-subdir registration (warp_rc / warp_rc/warp-fork case)
- **#1221** (closes #1212) — `query` filters `testdata/__fixtures__/test-fixtures/` paths by default with `include_fixtures=true` opt-out

Total v0.66 PRs merged across both halves of the day: **10** (#1210/#1211/#1214/#1215/#1216/#1218/#1221/#1222/#1223/#1229).

## CRITICAL — #1231 awaiting investigation

**The most important find this session.** v0.66 DOGFOOD EXPLORE pass surfaced silent symbol-loss in pincher's own dogfood corpus:

- `internal/server/server.go` has **73 `*Server` methods in source**.
- pincher-repo's live index has **8 Methods** for that file (and only 38 total symbols, vs ~233 the clean-room CLI extracts).
- Clean-room repro (single-file index into a fresh data-dir) extracts **75 Methods correctly** — so the AST extractor is fine.
- The bug is in the **shared-DB / live-Watch / multi-process persistence path**.

The 8 indexed Methods are the ones ADDED to server.go *since* sniffer's older snapshot was taken. The 65 OLDER Methods that DID exist in the file (and still exist) are missing from pincher-repo's index. Pattern: **incremental delete-or-insert bug** that only re-inserts newly-added Methods after a partial DELETE.

Downstream user-visible failures from this loss:
- `dead_code` returns false positives (callers aren't in the index)
- `trace name=handleQuery` returns "symbol not found"
- `neighborhood id=...handleQuery#Method` silently falls back to a stale sniffer mirror, returning 194 cross-project neighbors with only a warning (also filed: #1232)

`SQLITE_BUSY` errors are observable in real time when force-reindexing — confirms shared-DB contention.

**Next-session attack path on #1231:**

1. Read `internal/db/db.go` `BulkUpsertSymbols`. Look for: silent `INSERT ... ON CONFLICT` paths that could swallow a row delta; SQLITE_BUSY retry paths that discard the batch instead of replaying.
2. Read `internal/index/indexer.go` per-file delete-then-insert path (`DeleteSymbolsForFile` → BulkUpsertSymbols flow). Look for partial-DELETE-then-skip-INSERT shape.
3. Read `internal/index/lockfile.go` cross-process project lock. Verify the lock is held across the ENTIRE BulkUpsertSymbols call, not just the extraction phase.
4. Repro recipe: copy pincher-repo's `internal/server/server.go` into `/tmp/probe-src/`, run `/c/tools/pincher.exe index --json-summary --data-dir /tmp/probe-db /tmp/probe-src` → produces 75 Methods. Then in the live shared DB: query and observe 8.
5. Suggested fix to ship alongside the root cause: post-BulkUpsertSymbols `SELECT COUNT(*)` parity check that logs WARN on >10% delta from extracted count. Even before the root-cause fix, this guard surfaces the bug to users in real time.

## Issues filed this session

**v0.66 (this cycle):**
- #1231 CRITICAL — silent symbol loss in shared-DB live Watch path (described above)

**v0.67 (next cycle):**
- #1219 — `pincher vacuum` one-shot DB reclaim command (advisory already names it)
- #1220 — `doctor` per-project byte-size estimate (paired with #1219)
- #1224 umbrella — thin-client payload optimization (Cursor/Continue/Claude Desktop)
  - #1225 — `trace compact=true` + default-on fixture filter
  - #1226 — `search compact=true` mirroring trace
- #1230 — `guide` audit-shape always recommends docstring-missing query regardless of task
- #1232 — `neighborhood`/`symbol`/`context` silent cross-project fallback (warn-only is not enough)

**v0.68 (next cycle):**
- #1227 — `meta=lite` envelope across all tools (env var or per-call arg)
- #1228 — `trace max_hops` cap + `neighborhood` default-on fixture filter

## Open bugs remaining on v0.66

Triaged after this session:

- **#1231** (the critical above) — pick up first
- **#1205** doctor 68s latency — deep SQL/query plan work; partially addressed by v0.67 #1219 vacuum + #1220 per-project bytes
- **Multi-day carryovers (move to next minor if they slip):**
  - #1162 closure-tables-default-on (gated by #639 measurement)
  - #1177 TS receiver-type resolver (multi-day TS AST work)
  - #1182 Rust AST extractor (multi-day)
  - #1183 Java AST extractor (multi-day)
  - #635 Dashboard panel rendering on v0.64 substrate

## Environment state

- **master**: clean, up to date with origin. Latest commit is #1229 merge.
- **on-PATH pincher**: `/c/tools/pincher.exe` v0.65.0 (still — needs a rebuild + swap to v0.66 development binary for new fixes to be live)
- **MCP supervisor**: auto-restart-on-drift firing correctly; respawned multiple times mid-session
- **DB**: 12.1 GB at `C:\Users\kevin\AppData\Roaming\pincherMCP\pincher.db` (grew slightly during ClaudeCode reindex; user notified earlier session about vacuum need). `pincher vacuum` still doesn't exist — see #1219.
- **WAL**: 280 MB (down from 370 MB earlier; healthy direction)
- **Test suite**: green on master.
- **pincher.exe processes**: 4 daemons running on the box (per `tasklist`). Leave them — supervisor manages them. (This concurrency is ALSO the most likely contributor to #1231.)

## Standing rules established earlier this session (memory'd)

1. **`feedback_post_release_issue_triage.md`** — After every `.xx` tag, walk open issues and categorize them.
2. **`feedback_post_release_environment_check.md`** — After every `.xx` tag, verify env. Auto-action where possible; PushNotification when human keyboard action needed.

## Patterns to keep using

- Table-from-the-start tests (#1152).
- CHANGELOG.d/`<num>.<type>.md` stub convention.
- Cut fix branches from fresh master.
- Co-Authored-By: Claude Opus 4.7 (1M context) footer.
- `gh pr merge --auto --squash` + `git checkout master && git pull --ff-only` after every PR.
- Pincher first (per CLAUDE.md). Read only when authoring new files or Edit forces it.

## Patterns to NOT repeat

- **Description audits aren't DOGFOOD.** They're useful but they're text work.
- Don't ScheduleWakeup long delays.
- Don't ask for permission on routine PRs / fixes / probes.
- Don't bundle unrelated fixes — one bug per PR, table-from-the-start tests.

## Token savings note

The user said: *"pincher is saving a ton of tokens so things are starting to pay dividends with these efforts - good work."*

Keep using pincher first, Read/Grep as documented fallbacks only.

---

**Branch:** master (clean)
**Last commit:** `git log --oneline -1`
**In-flight PR:** none — everything in this session is merged
**Memory:** `~/.claude/projects/D--ClaudeCode-pincher-repo/memory/MEMORY.md` + this session's `project_v0_66_dogfood_session.md` (loaded automatically)

**First action next session:** Read this file + investigate #1231 per the attack path above. Don't release v0.66 with #1231 outstanding — the silent extraction loss undermines every other pincher claim.
