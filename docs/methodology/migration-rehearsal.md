# Migration rehearsal — `v0.4` → current

A weekly CI smoke test that catches silent migration regressions before externals see them. Tracked by [#1539](https://github.com/kwad77/pincher/issues/1539) (FILE-U, v0.93); pairs with [#1390](https://github.com/kwad77/pincher/issues/1390) external migration-guide review.

## What it does

`.github/workflows/migration-rehearsal.yml` runs weekly (Wednesday 11:00 UTC) and on `workflow_dispatch`:

1. Builds the current pincher binary from HEAD.
2. Downloads the `v0.4.1` release tarball — the earliest release with a stable extraction surface.
3. Creates a small deterministic corpus (Go + YAML + Markdown).
4. Indexes the corpus with the `v0.4.1` binary, producing a schema-v4 SQLite DB.
5. Opens the same data directory with the current binary, triggering in-place migration to the current schema (currently v33+).
6. Re-indexes the corpus with the current binary to exercise the migrated schema.
7. Starts the current binary's HTTP gateway and probes a curated tool set: `health`, `stats`, `schema`, `search`.
8. Fails the workflow if any probe returns an error envelope or `search` returns zero results (the v0.4-era rows didn't survive the migration).

## Why a synthetic corpus, not a real repo

A real repo (kubernetes, react, the linux kernel) drifts. The same commit hash indexed today produces a different symbol set six months from now if any extractor evolves between v0.4 and current — and 28 extractor changes have shipped between those two versions. A synthetic corpus pins the migration signal at the bytes the v0.4 binary actually saw.

The corpus deliberately exercises:

- **Go extraction** — the highest-confidence path at v0.4. Hits the most schema-touching code in the migration chain.
- **YAML config** — config-corpus routing (introduced v0.9 schema migration); confirms the v9 trigger backfills correctly against pre-v9 rows.
- **Markdown docs** — docs-corpus routing; same reasoning, different content kind.

## Probe set rationale

The four HTTP probes were picked to cover orthogonal failure modes:

| Probe | What breaks if it fails |
|---|---|
| `/v1/health` | Server can't open the DB or schema-version check fails — total migration failure. |
| `/v1/stats` | Session stats schema (sessions table, v4 migration) didn't survive the upgrade. |
| `/v1/schema` | Schema-introspection itself broke — every downstream tool would also break. |
| `/v1/search` | FTS5 virtual table didn't rebuild correctly against migrated rows — the most common silent-break shape per the v9 corpus-routing migration. |

When a probe fails, the workflow uploads the `datadir-v04/` artifact (90-day retention) for forensics.

## Drift triage

When the workflow goes red:

1. Download the `migration-rehearsal-data-<run_id>` artifact.
2. Open the `pincher.db` with `sqlite3` and run `SELECT version FROM schema_version;` — confirms which migration step the upgrade reached before breaking.
3. Cross-reference the migration history table in `docs/REFERENCE.md` to identify the responsible step.
4. Add a section to the migration guide explaining the break + the workaround, OR fix the migration step + add a regression test pinning the failure mode.

The third option is preferred — the rehearsal exists so we fix breaks before users hit them, not so we document workarounds for users who hit them.

## When to update the baseline

The baseline binary version (`v0.4.1`) does **not** advance with releases. The whole point is to test the longest possible migration chain pincher commits to support. The baseline only advances if we explicitly drop support for upgrading from older versions — that decision goes through an ADR, not a workflow edit.

The synthetic corpus may need to grow when we promote a new language to a confidence tier that the rehearsal should exercise. When that happens:

1. Add a file under `.github/workflows/migration-rehearsal.yml`'s `Create synthetic corpus` step.
2. Bump the expected probe-set if the new file should produce queryable rows.
3. Run the workflow via `workflow_dispatch` to confirm the addition doesn't false-flag.

## Why this is not a per-PR gate

The bench is slow (~3 min wall-clock per run) and the signal is drift — silent migration regressions accumulate over many minors, not single PRs. Catching it weekly is the right cadence.

The schema-migration parity test (`TestClassifyCorpus_MatchesSQLTriggerRouting`) and the snapshot tests catch per-PR migration regressions cheaply; the rehearsal catches the cross-migration interaction failures those tests can't see.

## Related

- [#1539](https://github.com/kwad77/pincher/issues/1539) FILE-U — this gate.
- [#1390](https://github.com/kwad77/pincher/issues/1390) — external migration-guide review (consumer of this gate's findings).
- `docs/REFERENCE.md` — migration history table.
- `internal/db/db.go` — `schemaMigrations` slice (canonical migration source).
