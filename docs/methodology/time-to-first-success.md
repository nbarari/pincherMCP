# Time-to-first-success budget

**Goal:** a brand-new user goes from `git clone` of a target repo to a useful pincher answer in **≤5 minutes**.

This budget is enforced by `.github/workflows/time-to-first-success.yml` (weekly schedule + manual dispatch). Tracked by [#1535](https://github.com/kwad77/pincher/issues/1535) (FILE-Q, v0.91).

## What it measures

`scripts/time-to-first-success.sh` times four phases end-to-end:

| Phase | What runs | Why it's in the budget |
|---|---|---|
| `install` | `cp $PINCHER_BIN $PATH_DIR/pincher` | Every install path (Homebrew, Scoop, direct download) ends in "binary on PATH" — we time the cheapest representation of that step. Real install paths add network IO, but the floor is meaningful. |
| `clone` | `git clone --depth=1` of the target repo | Real user wall-clock starts at `git clone`, not at "binary on PATH." `--depth=1` is honest — a new user does not pull full history. |
| `index` | `pincher index .` | First-index cost — the dominant phase for any non-trivial repo. |
| `first_query` | `pincher search "<symbol>"` against the just-indexed corpus | Validates the result is usable, not just that indexing completed. |

Total: `install + clone + index + first_query` ≤ `BUDGET_SECONDS` (default 300).

## Reading the output

The script prints JSON to stdout:

```json
{
  "schema_version": 1,
  "target_repo": "https://github.com/kwad77/pincher",
  "query": "Indexer",
  "phases_ms": {
    "install": 12,
    "clone": 8421,
    "index": 1187,
    "first_query": 14
  },
  "total_ms": 9634,
  "budget_ms": 300000,
  "within_budget": true
}
```

`schema_version` exists so downstream consumers (CI gates, dashboard graphs) can detect format changes.

## Baseline policy

The committed baseline lives at `testdata/bench/time-to-first-success.bench.txt`. Format is plain `key: value` lines (not JSON) so a quick `awk` extracts a single number without requiring `jq` in CI.

Refresh policy: same as the bench-baseline policy (`CLAUDE.md` release-prep checklist item 7). The baseline is pinned to **CI hardware** — running locally on a different machine produces meaningless deltas.

To refresh:

1. Trigger the workflow via `workflow_dispatch` on the Actions UI.
2. Download the `time-to-first-success-<run_id>` artifact.
3. Translate the JSON into the `key: value` baseline format.
4. Commit `testdata/bench/time-to-first-success.bench.txt` and push.

When to refresh:

- Release-prep cycles for the `.x9` hardening minors (workstream 2 of the #672-shape umbrella). FILE-Q acceptance criterion has the bench live by v0.91; pinning a fresh baseline in that cycle is part of the release.
- After a deliberate perf-affecting refactor whose new numbers ARE the rationale.

When NOT to refresh:

- Any PR that doesn't intentionally change perf shape. Auto-refresh defeats the gate's purpose (per the v0.79 prep audit, the committed bench baseline drifted 8 minors silently because every release auto-refreshed without justification).

## Regression threshold

20% over the committed baseline's `total_ms` fails the workflow. The number comes from the FILE-Q acceptance criterion. Per-phase regressions (e.g., index +30% but total stays within 20%) do not fail the workflow yet — the per-phase signal is useful for triage, not yet a gate.

## Why not gate per-PR?

Two reasons:

1. **Variance.** CI runner network IO drives the `clone` phase. Per-PR signal is too noisy to act on.
2. **The signal is drift, not stepwise change.** Time-to-first-success regressions accumulate over many minors — a single PR almost never moves the number by 20%. Catching it weekly + at release-prep is the right cadence.

The corpus-bench gate (`make corpus-bench`) is the per-PR perf gate for in-process operations. This bench measures the orthogonal axis of process-startup + IO + first-query — much slower-moving territory.

## Related

- [#1535](https://github.com/kwad77/pincher/issues/1535) FILE-Q — this gate.
- [#1536](https://github.com/kwad77/pincher/issues/1536) FILE-R — per-host conformance matrix that uses the same script per supported MCP host.
- [`docs/methodology/token-savings.md`](token-savings.md) FILE-A — token-savings methodology, the orthogonal user-value axis.
- `.github/workflows/bench-baseline.yml` — the in-process bench-refresh workflow (refreshed via the same UI dispatch pattern).
