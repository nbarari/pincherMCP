# External comparator — pincher vs raw Read+Grep agent loop

Head-to-head measurement that anchors the **token-savings** claims (FILE-A, `docs/methodology/token-savings.md`) with real numbers rather than internal-corpus estimates. Tracked by [#1521](https://github.com/kwad77/pincher/issues/1521) (FILE-B, v0.86 advisory).

## Why this gate exists

`make corpus-bench` measures pincher against itself. The `tokens_saved` envelope reports a baseline that assumes an agent without pincher would use full-file `Read`. Neither answers the question this gate does:

> When an agent has only the primitives `grep`, `find`, `cat` (the toolset before pincher), how much disk would it read to get the same information one pincher call delivers?

The ratio of comparator bytes to pincher bytes is the defensible token-savings claim. Per `SAVINGS_HONESTY` ADR, the published `tokens_saved` envelope is known to overcount; the comparator's measurements supersede that for marketing copy.

## What the comparator simulates

`scripts/comparator-raw-readgrep.sh` runs both sides for each task:

| Side | Tooling | Captures |
|---|---|---|
| **Pincher** | HTTP gateway `/v1/<tool>` invocation | wall-ms, response body bytes |
| **Raw Read+Grep** | `grep -rn "func X" $CORPUS` → `wc -c` over every matched file | wall-ms, sum-of-file-bytes |

The "comparator bytes" is the upper-bound on what a no-pincher agent would have to read to confirm the result. A real agent might `Read` with offset/limit instead of reading whole files; we cap-rather-than-estimate because:

1. Real agents (Claude Code, Cursor, codex) DO default to full-file `Read` when they don't know the exact byte range.
2. An optimistic estimate is harder to defend than a generous upper bound.

Comparator runs are not the same as Claude Code's actual reads; the comparator IS structurally what a real no-pincher loop has to do. We accept "comparable cost shape" rather than "literal trace of one user session."

## v0.86 initial scope (intentional cut)

This PR ships **one task** (`find-symbol-by-name`) as the working reference for the runner. FILE-B acceptance has four bullets:

1. ✅ Comparator implementation (raw Read+Grep loop, this PR).
2. ✅ Standardized public corpus — uses `testdata/corpus/go-project` in v0.86; public corpora (linux kernel, kubernetes, react) ship in v0.87-v0.91 follow-ups.
3. ✅ Measurements published per release in `testdata/bench/comparator/` — the v0.86 cron output is the first.
4. ✅ Runs on a fixed schedule (weekly), not per-PR.

The 20-task suite called for by the methodology doc lands across v0.87-v0.90 follow-ups (one task per minor — each task is non-trivial to write defensively).

## Task shape

Each task in the comparator suite has:

- A **name** (`find-symbol-by-name`, `read-with-context`, `find-callers`, `multi-symbol-aggregate`).
- A **pincher side** — the canonical tool call sequence.
- A **comparator side** — the grep/cat sequence a no-pincher agent runs.
- A **bytes_ratio = comparator_bytes / pincher_bytes** — the headline number.

The four task names above are the planned phase-1 suite. Each ships as its own PR so the methodology iterates one shape at a time, not a 20-task bundle that's all-or-nothing.

## Phase rollout

| Release | Workflow | Effect |
|---|---|---|
| v0.86-v0.90 | `continue-on-error: true` | Advisory; numbers feed methodology doc |
| v0.91+ | `false` per FILE-A acceptance | Hard floor — bytes_ratio dropping below the published claim regresses the v1.0 marketing position |

The phase-2 promotion does NOT block release on absolute byte counts — `pincher search` adding 10% more response bytes is fine. It blocks on the **ratio**: if the comparator becomes more efficient (unlikely) or pincher becomes less efficient (the regression we care about), bytes_ratio drops below the published floor and the gate fires.

## Why not measure during real agent sessions

Two reasons:

1. **Non-reproducible.** Each session is one path through one user's task; can't compare across releases.
2. **Tooling lock-in.** A measurement that requires Claude Code (or any specific host) couldn't be reproduced by a maintainer who uses a different host.

The synthetic-task comparator runs in CI on every release, deterministically. That's the honesty story FILE-A needed.

## Related

- [#1521](https://github.com/kwad77/pincher/issues/1521) FILE-B — this gate.
- [#1520](https://github.com/kwad77/pincher/issues/1520) FILE-A — token-savings methodology (consumer of these numbers).
- `SAVINGS_HONESTY` ADR (`mcp__pincher__adr get key:"SAVINGS_HONESTY"`) — the existing `tokens_saved` envelope overcounts; comparator measurements supersede.
- `internal/server/server.go` — `jsonResultWithMeta`'s `baseline_method` field (this gate's measured ratio replaces the `full_file_read` assumption).
