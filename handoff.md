# Handoff — 2026-05-16 evening

## Resume in 30 seconds

You are mid-loop. The autonomous loop directive from the user is in effect:

> **"begin a loop and not stop until we get right next to .7 release."**
> **"we need to uncover as much as possible between .65-.69 so that .70 is as stable as possible."**
> **"after every .xx pushed to GitHub ... check through issues and make sure we aren't missing context."**
> **"after each .xx in a loop - just verify [the env] - if action needs to happen ....then take the action - if human action is required at keyboard to remedy environment - send Claude notification."**

Don't pause without a sound reason. Continue probing + fixing v0.66 bugs. Cycle ends at v0.69 hardening; v0.70 is the stable promotion target.

## What shipped this session (substantial)

| Release | PRs | Notes |
|---|---|---|
| **v0.64.0** tagged | #1189-#1193 (5) | Dashboard data plumbing (schema v27 `session_tool_calls` + per-call event logger) + description-honesty audit start |
| **v0.65.0** tagged | #1194-#1201 (7 + release prep) | Description-honesty audit sweep: search / architecture / query / fetch / stats / symbol / doctor |
| **v0.66.0** in flight | #1210, #1211, #1214, #1215, #1216, #1218 (6) | DOGFOOD round 1 + 2 — 6 silent-wrong bugs fixed |

**v0.66 fixes merged (most recent first):**

- **#1218** (closes #1217) — pinchQL column-vs-column predicate dropped, not substituted with `false`. Silent-confidently-wrong: `WHERE x AND <unsupported>` was killing all rows.
- **#1216** (closes #1213) — `isTestFile` recognizes bash `_test.sh` + `test_*.sh` conventions
- **#1215** (closes #1206) — doctor advisory: WAL bloat detection (>512 MiB OR >10% of DB)
- **#1214** (closes #1207) — Markdown sibling-heading dups no longer flood `qualified_name_collision`
- **#1211** (closes #1203) — empty `__init__.py` no longer floods `byte_range_negative`
- **#1210** (closes #1202) — `_meta.capabilities` `schema_vN` tag is now a runtime probe (was hardcoded, lying after migrations)

## CRITICAL CONTEXT: the v0.65 DOGFOOD pivot

The user pushed back mid-session:

> "that .65 dogfood loop seemed so much lighter than the .55 dogfood loop - did we test as thorough and probe as hard as we did in .55?"

**This was correct.** v0.65 was a description audit — text-vs-runtime parity — not real DOGFOOD. v0.55-era DOGFOOD found bugs by actually CALLING tools with edge-case inputs, indexing external corpora, cross-checking output against ground truth. v0.65 found 1 real silent-wrong (fetch's `kind:Document`) and 6 description undersells.

After the pivot in v0.66, DOGFOOD round 1 (one `mcp__pincher__doctor` call) surfaced **8 real bugs in 60 seconds**. That's the .55-caliber yield. **Keep probing this way through .66-.69.** Don't drift back into low-yield description audits.

## Open bugs in v0.66 (still to fix)

Filed but unfixed — pick by ergonomic impact:

- **#1204** C++ LogTemp dups — extractor emits per-call-site duplicates of macro identifiers (`LogTemp` x11 in one file). Needs extractor inspection (`internal/ast/extractor.go` C/C++ funcRE).
- **#1205** doctor 68s latency — deep. Likely needs SQL/query-plan work + WAL-checkpoint tuning. May naturally improve after the user runs `pincher vacuum` (see ENV section below).
- **#1208** TS dup QN — `skwad-app` and similar TS files emit duplicate QNs. Hypothesis: re-exports + overloads + type/value collisions. Needs file inspection.
- **#1209** Rust nested-project collision — `warp_rc/warp-fork` nested project paths produce cross-project QN collisions because the collision detector isn't partitioned by `project_id`. Two fixes: scope detector by project_id, AND warn on nested-project registration.
- **#1212** pinchQL `testdata/` paths leak into audit results — dead_code and architecture filter them via `isTestFixturePath`; pinchQL doesn't. Same family as #1213.

**Carry-forwards still in v0.66 from earlier cycles:**
- **#1162** closure-tables-default-on (gated by #639 measurement)
- **#1177** TS receiver-type resolver (multi-day TS AST work)
- **#1182** Rust AST extractor (multi-day)
- **#1183** Java AST extractor (multi-day)
- **#635** Dashboard panel rendering on the v0.64 substrate

## Environment state

- **master**: clean, up to date with origin. Latest commit is #1218 merge.
- **on-PATH pincher**: `/c/tools/pincher.exe` v0.65.0 (swapped this session via `bash scripts/swap-active-binary.sh`)
- **MCP supervisor**: auto-restart-on-drift firing correctly; respawned onto v0.65.0 after the swap
- **DB**: 11 GB at `C:\Users\kevin\AppData\Roaming\pincherMCP\pincher.db` (advisory recommends `pincher vacuum` + `list prune_dead=true`). User was notified via PushNotification 2026-05-16. **Action required is user-keyboard, not auto.**
- **WAL**: 2.3 GB (same advisory). Will auto-truncate after vacuum.
- **Test suite**: green on master. Two flaky `cmd/pinch` + `internal/supervisor/cmd/probe` tests fail LOCALLY because of `SQLITE_BUSY` against the running pincher MCP daemons holding the global DB. **They pass in CI** — CI workers have no local pincher daemons.
- **pincher.exe processes**: 4 daemons running on the box (per `tasklist`). Leave them — supervisor manages them.

## Standing rules established this session (memory'd)

1. **`feedback_post_release_issue_triage.md`** — After every `.xx` tag, walk open issues and categorize them. Multi-day work → next minor. Non-critical hardening → `.x9`. Critical correctness → skip queue.
2. **`feedback_post_release_environment_check.md`** — After every `.xx` tag, verify env. Auto-action where possible (binary swap, branch cleanup); PushNotification when human keyboard action is needed (DB vacuum, manual `/mcp` reconnect).

Both pair with the existing user-cannot-restart-MCP and auto-restart memories.

## Next concrete actions (in priority order)

1. **Read this file + `MEMORY.md` index.** Memory is loaded automatically; this file is the session-state delta.
2. **Verify env still healthy** (per the post-release env-check rule):
   ```bash
   git log --oneline -3                      # confirm master state
   /c/tools/pincher.exe --version            # should be v0.65.0 or newer
   ```
   Then call `mcp__pincher__health` — should show `schema_v27` in capabilities + `binary_version` matching tag. If binary_version is older than the on-PATH binary, run `bash scripts/swap-active-binary.sh` to swap; auto-restart will pick it up on next MCP call.
3. **Triage any un-milestoned issues:**
   ```bash
   gh issue list --state open --json number,title,milestone --jq '.[] | select(.milestone == null) | "\(.number) \(.title)"'
   ```
   Slot into v0.66 (active), v0.69 (hardening), or critical-correctness skip-queue.
4. **Pick the next v0.66 fix.** Highest-leverage open bugs:
   - **#1209** Rust nested-project collision — partition the dup detector by `project_id` is a clean, targeted change. Also surfaces the broader "nested-project registration warning" which is a UX feature.
   - **#1208** TS dup QN — start with `cat skwad-app/lib/state-laws.ts | grep -B 2 -A 2 'law'` (or equivalent in whichever Codex/ClaudeCode project has it indexed) to see the actual source shape, then decide if it's re-export / overload / type-value.
   - **#1212** pinchQL testdata filter — add an `include_fixtures=false` default to handleQuery, mirror the architecture / dead_code filter shape.
5. **After ~2-3 v0.66 fixes, probe again** (DOGFOOD round 3) — different vein. Try:
   - `mcp__pincher__changes scope=base:master` against pincher-repo to stress the changes handler
   - `mcp__pincher__trace` on a hotspot in a non-Go corpus
   - `mcp__pincher__neighborhood include_source=true` on a large file
   - Force a binary swap mid-call and verify auto-restart resilience under load
   - Index a fresh external project; observe what fails
6. **When v0.66 has ~5-7 PRs**, tag `v0.66.0` via the release-prep flow (read `CLAUDE.md` "Release-prep checklist"; assemble CHANGELOG.d; promote roadmap row).
7. **Continue cycle**: v0.67 → v0.68 → v0.69 hardening (close all v0.6x carryovers at v0.69) → stop just short of v0.70 stable promotion.

## Patterns to keep using

- **Table-from-the-start tests** (#1152): every PR ships positive + negative + control + cross-check.
- **CHANGELOG.d/`<num>.<type>.md`** stub convention. `bash scripts/changelog-assemble.sh --apply` at release prep.
- **Cut fix branches from fresh master** — never stack branches without explicit reason.
- **Co-Authored-By: Claude Opus 4.7 (1M context)** footer on every commit.
- **`gh pr merge --auto --squash`** + `git checkout master && git pull --ff-only` after every PR.
- **Pincher first** (per global CLAUDE.md): `mcp__pincher__*` tools beat Read/Grep for code navigation; Read only when authoring new files or when Edit forces a Read precondition.

## Patterns to NOT repeat

- **Description audits aren't DOGFOOD.** They're useful but they're text work. Real DOGFOOD is calling tools with weird inputs and finding silent-wrong handler logic. The .55 cycle's haul was 8 bugs in 90 minutes from probing untouched tool surface — that's the bar.
- **Don't ScheduleWakeup long delays** — `feedback_no_long_pauses.md`. Keep cranking.
- **Don't ask for permission** on routine PRs / fixes / probes — user is AFK, give-and-take is via the loop.
- **Don't bundle unrelated fixes** — one bug per PR, table-from-the-start tests, clean blast radius.

## Token savings note

The user said: *"pincher is saving a ton of tokens so things are starting to pay dividends with these efforts - good work."*

Keep using pincher first, Read/Grep as documented fallbacks only.

---

**Branch:** master (clean)
**Last commit:** see `git log --oneline -1`
**In-flight PR:** none — everything in this session is merged
**Memory:** `~/.claude/projects/D--ClaudeCode-pincher-repo/memory/MEMORY.md` (loaded automatically)

Pick up where the loop left off. Continue.
