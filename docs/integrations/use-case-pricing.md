# Use-case pricing — the measurement framework behind the composite-tool claims

**Status:** v0.74 first draft ([#1395](https://github.com/kwad77/pincher/issues/1395)).
Establishes the four scenarios + measurement methodology that the
Phase 4 composite-tool work
([composite-tool-roadmap.md](./composite-tool-roadmap.md))
gets evaluated against. Atomic baselines measurable today; composite
cells fill in alongside their implementations in v0.81+.

---

## Why this doc exists

Pincher's narrative claims ("composites collapse 5–10 calls into 1",
"the agent loop rewires its planning") have to be priced out against
real use cases before they ship. Otherwise the first integrator who
actually measures finds the gap and we lose credibility we've built.
Sourcegraph et al. spent years getting trust back from exactly this
"published 10×, measured 2×" pattern.

This doc defines:

1. The **four scenarios** that constitute the pricing surface.
2. The **measurement methodology** — what counts as "the same answer",
   how we report tokens / latency / accuracy, what's manually labeled
   vs machine-measured.
3. **Atomic baselines available today** — what current tools measure
   per scenario. Composite measurements arrive with the composite
   implementations in v0.81+.
4. The **negative-control scenario** — the use case where pincher
   shouldn't be the answer. Surfaces what pincher isn't.

The composite-score rubric the bench harness uses lives in
[`cmd/pinch/bench_composite.go`](../../cmd/pinch/bench_composite.go).
That file is the audit trail for the weights; this doc is the audit
trail for the scenarios.

---

## The three measurement dimensions

Per scenario, every shape (atomic chain / composite call / raw
Read+Grep) gets measured along three axes. **All three are reported
in the published doc**, not just the headline.

### 1. Token cost (input + output)

Sum of `_meta.tokens_used` across every call in the agent shape, plus
the LLM's own input tokens for the surrounding planning loop.
Per-scenario breakdown, not a single global average — the cost shape
of `dead_code` is structurally different from `investigate_failure`,
and rolling them together obscures both.

The bench harness records token cost per call; the composite-score
rubric weights it as `(baseline - measured) * 0.001` (small per-token
weight to keep large absolute numbers from dwarfing
correctness/round-trip terms).

### 2. Wall-clock latency (p50 + p95)

Median + tail. Distinguishes "composite saves a round trip" (real)
from "composite is one call but takes longer than the chain" (the
trap). The p95 is the trap detector: a composite that wins on p50
but loses on p95 is masking a worst-case implementation bug behind
average-case timing.

Per-call timing from the bench harness; cross-validated against
OTLP per-tool spans (#1163 shipped v0.67) where available.

### 3. Accuracy (manual reference set)

Does the composite return the same answer the atomic chain would
have produced? Pincher cannot grade its own answers — accuracy is
labeled by hand against a small, high-quality reference set per
scenario. Small N is intentional: 10–20 carefully chosen cases beat
1000 noisy ones for measuring whether a composite drifts from its
atomic-chain answer.

Reference sets live in `testdata/usecase/<scenario>/` as JSON files
pairing inputs with the expected canonical answer. The bench harness
diffs the composite's output against the expected; mismatches drop
the per-case accuracy from 1.0 to a partial score (1.0 = exact match,
0.5 = correct symbol but wrong call-graph depth, 0.0 = wrong
symbol).

---

## The four serious-use-case scenarios

Each one is a real investigation pattern an agent runs daily against
a real codebase. We measure all three shapes against each.

### Scenario 1 — "Find dead code in this Go service"

**Reference set.** N=20 functions in `pincher-repo` itself, each
labeled dead/alive by manual inspection. Some live functions are
function-value-bindings (the v0.21 dead-code FP triangle's third leg)
to stress-test the suppression rationale.

**Shapes measured.**
- **Atomic.** `dead_code(language=Go)` (single call — already
  composite-shaped per v0.62's regex-CALLS sweep).
- **Composite.** Hypothetical `audit_unused` — `dead_code` plus
  per-candidate suppression rationale, surfacing which triangle legs
  ran and what survived. Lands v0.83 per the composite-tool roadmap
  sequencing.
- **Raw.** `grep` for function definitions, then `grep` for callers
  of each — the loop a new developer reaches for without code-intel.

**Pricing target.** Composite faster AND more accurate than atomic;
both vastly cheaper than raw. The accuracy difference comes from
the false-positive triangle close — raw `grep` cannot detect that a
function is bound to an interface via a function-value field at a
distant package boundary.

**Atomic baseline (measurable today).** Today, `dead_code` returns
results in ~30–80ms on `pincher-repo` (per `pincher bench`). Raw
`grep` measurement is N/A here because the false-positive rate is
the input, not a measurable output — the manual-labeled reference
set establishes the truth, then we report each shape's deviation
from it.

### Scenario 2 — "Why is this test failing"

**Reference set.** N=10 real test failures from this repo's git
history. For each: a stack trace + the known root-cause symbol (the
commit that fixed the failure tells us what the answer should have
been).

**Shapes measured.**
- **Atomic.** `search` (top stack frame) → `symbol` (resolve) →
  `context` (read source) → `trace inbound` (find caller) → `changes
  scope=branch` (recent edits intersecting). 5 calls.
- **Composite.** Hypothetical `investigate_failure(error_text)`. 1
  call. Lands v0.81 per the roadmap.
- **Raw.** Read every file mentioned in the stack trace plus the
  current branch diff.

**Pricing target.** Composite achieves ≥80% accuracy on the
reference set (identifies the same root-cause symbol the fix
commit named) with ~5× round-trip collapse and ~4× wall-clock p50
improvement. Raw is bounded by reading-time on the file count;
typically 10–50× the composite's token cost.

**Atomic baseline (measurable today).** The 5-call sequence runs end-
to-end at ~1.0–1.4s wall-clock p50 on `pincher-repo` (per OTLP
trace spans aggregated across 10 hand-replayed cases).

### Scenario 3 — "Blast radius of changing this function"

**Reference set.** N=10 real PRs from this repo's history where we
know which downstream symbols got touched (git diff tells us; the
labeled set names the symbol *changed* and the symbols *also
touched in the same PR*).

**Shapes measured.**
- **Atomic.** `changes` → `trace outbound` (depth=2) → `adr action=list`.
  3 calls.
- **Composite.** Hypothetical `plan_change(target)`. 1 call. Lands
  v0.82 per the roadmap.
- **Raw.** `grep` for callers of the function, read each caller.

**Pricing target.** Composite returns the risk-classified blast
radius in one envelope with depth-1 / depth-2 cohorts named
separately. Raw misses transitive callers entirely; atomic gets
them but requires the agent to assemble them itself.

**Atomic baseline (measurable today).** The 3-call sequence runs at
~150–400ms wall-clock p50 on `pincher-repo`, dominated by the trace
BFS depth.

### Scenario 4 — "Orient me on this unfamiliar module"

**Reference set.** Ask 3 contributors which 5 things they'd want to
know about an unfamiliar module — that's the rubric. The reference
set names a directory and the 5 facts a human contributor would
prioritize (e.g., "the entry points", "the external HTTP clients",
"the cross-package imports out").

**Shapes measured.**
- **Atomic.** `architecture(aspects=entry_points,packages)` →
  `search(file_pattern=path/**, label=Function)` → `context(seed)`
  × 5. 7 calls.
- **Composite.** Hypothetical `onboard_module(directory)`. 1 call.
  Lands v0.84 per the roadmap.
- **Raw.** Read `README` + browse files.

**Pricing target.** Composite returns the 5-fact orientation
envelope; accuracy measured by fraction of the human-rubric facts
the response covers (subjective but consistent across reviewers via
the labeled reference set).

**Atomic baseline (measurable today).** The 7-call sequence runs at
~600–900ms wall-clock p50 on `pincher-repo`; raw read of a 47-file
module averages 60–80K tokens depending on file size.

---

## Negative-control scenario

**"Format this file."** Pure write operation — no investigation. The
correct answer is `gofmt`, not pincher. Including this scenario in
the measurement set surfaces what pincher isn't: if any composite
shape returns a result for "format this file" without erroring out,
that's a bug.

The negative control is itself a test of the framework's calibration.
A measurement framework that scores well on every scenario it ever
runs is suspect. Including a case the framework *should* score zero
on validates that the framework can recognize misuse.

---

## Per-doc pricing-citation policy

Every claim in [composite-tool-roadmap.md](./composite-tool-roadmap.md)
/ [loop-leverage-layers.md](./loop-leverage-layers.md) /
[meta-envelope-contract.md](./meta-envelope-contract.md) about
composite-vs-atomic deltas must cite a specific number from one of
the four scenarios above. The doc that lays the claim links to the
specific scenario row in this table; the table cells fill in as
composites ship in v0.81+.

Examples of compliant claims:

- ✅ "investigate_failure averages 1 call at 800ms p50; the equivalent
  atomic chain averages 6 calls at 3.2s p50 (4× wall-clock collapse).
  Measured on N=10 real test failures from this repo's git history —
  Scenario 2 above."
- ✅ "audit_unused returns the same dead-code set as the dead_code
  atomic plus the suppression rationale, with no measured accuracy
  drift on the N=20 reference set — Scenario 1 above."

Non-compliant claims (the kind we're forbidding ourselves):

- ❌ "composites collapse 5–10 calls into 1." (No number tied to a
  measurement; abstract.)
- ❌ "agent loops get vastly more efficient." (No scenario, no
  comparison shape, no metric.)

This doc's existence is the substrate against which non-compliance
is auditable. The CHANGELOG.md `[Unreleased]` section for any future
narrative-doc PR carries a checklist line: "every new claim cites a
Scenario N row from `use-case-pricing.md`."

---

## Sequencing

| Release | What lands |
|---|---|
| **v0.74** (this PR) | Scenarios + methodology + atomic baselines (this doc). Composite-score rubric ([#1398](https://github.com/kwad77/pincher/issues/1398)). |
| **v0.81** | `investigate_failure` ships + Scenario 2 composite cells fill in. |
| **v0.82** | `plan_change` ships + Scenario 3 composite cells fill in. |
| **v0.83** | `audit_unused` ships + Scenario 1 composite cells fill in. |
| **v0.84** | `onboard_module` ships + Scenario 4 composite cells fill in. |
| **v0.85** | `why_empty` ships (no scenario — pure pedagogy composite). |
| **v0.86–v0.90** | Iteration + cross-comparator pricing (#1298 §2) + REFERENCE doc + v1.0 contract-freeze prep. |

External-comparator implementations (Sourcegraph CLI, etc.) live in
[#1298](https://github.com/kwad77/pincher/issues/1298) §2 and roll
into v0.85+. Pricing first vs ourselves, then vs comparators —
otherwise we're tuning to the wrong target.

---

## Cross-refs

- [composite-tool-roadmap.md](./composite-tool-roadmap.md) — the
  composites being priced
- [loop-leverage-layers.md](./loop-leverage-layers.md) — Layer 3
  framing this doc grounds in measurement
- [meta-envelope-contract.md](./meta-envelope-contract.md) —
  envelope shape every measured composite emits
- [#1395](https://github.com/kwad77/pincher/issues/1395) — this doc's
  umbrella
- [#1298](https://github.com/kwad77/pincher/issues/1298) — bench
  harness §2 (cross-comparator implementations)
- [#1398](https://github.com/kwad77/pincher/issues/1398) — composite-
  score rubric (the formula this doc's scenarios feed)
- [#1391](https://github.com/kwad77/pincher/issues/1391) — composite-
  tool roadmap (the claims being priced)
- [#638](https://github.com/kwad77/pincher/issues/638) — v1.0 launch
  checklist (credibility is load-bearing for v1.0)
