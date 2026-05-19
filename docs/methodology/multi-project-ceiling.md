# Multi-project ceiling — watcher cost by indexed-project count

Per-tier wall-clock cost of a single watcher poll cycle across N indexed projects. Anchors the **"recommended max concurrent indexed projects"** budget published in REFERENCE.md. Tracked by [#1529](https://github.com/kwad77/pincher/issues/1529) (FILE-J, v0.86 phase-1 advisory; v0.91 phase-2 hard budget per acceptance criterion).

## Why measure this

The pincher operating model encourages indexing every project a user touches — the global `CLAUDE.md` says "Be effective with information; probe widely." Without a published ceiling, we can't honestly answer:

- How many indexed projects is "too many"?
- At what point does watcher CPU dominate the user's machine?
- When should the watcher auto-back-off?

FILE-J is orthogonal to FILE-I (single-project resource limits): FILE-I measures peak RSS at N files per project; FILE-J measures watcher CPU per tick at N projects. A user can hit either ceiling first depending on their machine.

## What the bench measures

`scripts/multi-project-ceiling-bench.sh`:

1. Generates N synthetic projects, 50 files each (small per-project so the signal is N, not per-project size).
2. Indexes each project once (post-condition: N indexed projects in the DB).
3. Times one watcher-equivalent poll cycle by running `pincher list --json` — that traverses every indexed project, which is what the watcher's per-tick body does. Three runs, median wins (filters cold-cache outliers).
4. Records `{project_count, per_project_files, poll_cycle_ms_median}` per tier.

CI tiers: **10 + 50** on cron. **200** available via `workflow_dispatch` with `TIERS=10,50,200` — the 200-project generation takes ~5-7 min on a fresh runner, too long for weekly cron.

## Why "list --json" as the watcher proxy

The actual watcher's tick body calls `runtime/disk-check` paths per project. Wiring up an in-process probe would require new instrumentation. `list --json` exercises the same disk-traversal pattern (per-project DB hits + `os.Stat` walks) and reports a single wall-clock number that scales linearly with the project count. The number isn't the watcher's exact tick cost — it's a stable proxy that moves the way the tick does.

When/if FILE-J phase-2 promotes to a hard budget (v0.91), this proxy gets reviewed against in-process numbers. If the divergence is meaningful (>10%), we add a `pincher watcher-tick` admin subcommand that times one real tick and report against that.

## Phase rollout

| Release | Workflow | Effect |
|---|---|---|
| v0.86-v0.90 | `continue-on-error: true` | Advisory; numbers feed budget |
| v0.91+ | `false` per FILE-J acceptance | Hard budget — `poll_cycle_ms_median` exceeding the published max blocks release |

## Published budget shape (REFERENCE.md)

Once phase-1 numbers settle, REFERENCE.md grows a "Multi-project scaling" section:

```
| Indexed projects | Watcher poll cycle (CI baseline) | Auto-backoff threshold |
|---|---|---|
| ≤10 | ~XX ms | n/a |
| ≤50 | ~XXX ms | n/a |
| ≤200 | ~X.X s | warn |
| >200 | exceeds recommended | watcher auto-disables until pruned |
```

The auto-backoff column is FILE-J acceptance bullet #3 (deferred to v0.87 follow-up, same way FILE-I deferred its degradation work).

## Scope cut (acknowledged)

FILE-J has four acceptance bullets:

1. ✅ Bench measuring per-tick wall-clock at 10/50/200 — this PR (10/50 in CI, 200 via dispatch).
2. ✅ Published recommended max — placeholders in REFERENCE.md fill in over phase-1.
3. ❌ Auto-backoff above threshold — runtime change in the watcher; deferred to v0.87 follow-up.
4. ❌ Pairs with FILE-I — yes, but the actual cross-axis story (high N projects × high N files) is filed as a v0.90 follow-up.

Splitting lets the budget PUBLISH at v0.86 (numbers exist) and ENFORCE at v0.91 (gate live). Runtime degradation work has the bench in place to measure against.

## Related

- [#1529](https://github.com/kwad77/pincher/issues/1529) FILE-J — this gate.
- [#1528](https://github.com/kwad77/pincher/issues/1528) FILE-I — single-project resource limits (orthogonal axis).
- `scripts/generate-synthetic-corpus.sh` — same generator both benches use.
- `internal/index/indexer.go` — `Watch()` is the runtime caller this bench proxies.
