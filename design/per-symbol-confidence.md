# Design: Per-symbol confidence scoring

**Status:** draft for review
**Issue:** [#34](https://github.com/kwad77/pincherMCP/issues/34)
**Companions:** #24 (blocklist), #25 (bloat-trap), #32 (per-corpus FTS5, ✅ merged), #33 (snapshot tests, ✅ merged)

## TL;DR

Replace `extraction_confidence` (per-language constant: 1.0 / 0.85 / 0.70) with a
score composed of small, orthogonal signals computed at extraction time. Same
column, richer values. Existing per-language constant becomes one signal among
several — file-level penalties and content-shape penalties pull noisy symbols
below an opt-in threshold without removing them from the index.

The deliverable is a 4-PR sequence (substrate → composition → tool surface →
default flip) so each PR is reviewable in isolation, the snapshot diff at each
step is the rationale, and rollback is a single revert.

## Why now

Three things changed in the codebase that make this no-longer-deferrable:

1. **#33 landed** (snapshot tests w/ `avg_confidence_by_kind`). Before this,
   shipping confidence changes meant unfalsifiable smoke tests. Now every
   PR's confidence shift surfaces as a snapshot diff at PR time.
2. **#32 part 3 landed** (default corpus = `code`). The mixed-corpus
   default disguised "bad config symbols outranking real Go functions" by
   never showing them adjacent. With per-corpus search, suppressing
   lockfile noise inside the `config` corpus is now a discrete win
   without affecting code search.
3. **#50 landed** (bench regression gate). Composition adds work to the
   extract path. The gate makes its perf cost reviewable — ns/op +20%
   would surface at PR time.

Without #33/#32/#50, this PR was a leap of faith. With them, it's incremental.

## Current state (verified in code, not summary)

```
internal/ast/extractor.go:108       conf := e.Confidence()     // single value
internal/ast/extractor.go:110         result.Symbols[i].ExtractionConfidence = conf
```

The `Extractor` interface returns a single per-extractor `Confidence() float64`.
Every symbol from that extractor gets the same value, regardless of:
- file path (lockfile vs. real config)
- parent fan-out (a Setting in a 5-key map vs. a 5000-key map)
- identifier shape (`makeGreeter` vs. `_` vs. ` `)
- content shape (a leaf scalar vs. a structural mapping)

`internal/ast/blocklist.go` patches the worst case (lockfile/minified/source-map
files) at the file level — those files never get extracted at all. That's the
right guard for clear-cut noise. It's not the right surface for the gradient:
`README.md` headings, `vendor/` Go code, `*-generated.go` symbols. These are
real but lower-priority, and the binary blocklist forces a wrong choice
between "drop them entirely" and "treat them as first-class."

## Goals

1. **Replace the constant with a score.** Each symbol carries an
   `extraction_confidence` derived from observable per-symbol signals.
2. **Make signals orthogonal and bounded.** Each contributor is small,
   testable in isolation, clamped to `[0, 1]` after composition.
3. **Preserve the existing column.** No schema migration; the v6 column
   already stores a `REAL`. Just gets richer values.
4. **Make it tunable, not magical.** Path patterns and kind baselines
   are config (a Go file, not a hidden table); changing one is a one-line
   PR with snapshot diff.
5. **Surface it on every search.** `min_confidence` filter on `search` /
   `query` / `trace`; `_meta.confidence_distribution` on response envelopes.
6. **Demote the existing blocklist into a signal**, not delete it. #24's
   patterns become input to `path_pattern_penalty` rather than a
   separate yes/no decision. Strong patterns (lockfile) score below 0.4;
   weak patterns (`README.md`) score around 0.6.

## Non-goals

- **Not a content-quality classifier.** No NLP, no statistical inference,
  no per-codebase learning. Every signal is a deterministic function of
  (path, kind, name, structural depth/breadth).
- **Not a precision/recall tradeoff knob.** Threshold = 0.0 returns
  exactly today's behavior. Filtering is opt-in by callers.
- **Not a replacement for FTS5 ranking.** BM25 still drives ordering;
  confidence only filters. A high-BM25 + low-confidence hit is still
  legitimate (the agent asked for it).

## Design

### Signal composition

```go
// confidence.go (new)
type Signals struct {
    BaseExtractor   float64  // existing per-language number
    KindBaseline    float64  // Function 1.0, Setting 0.95, Section 0.80, ...
    PathPenalty     float64  // -0.40 lockfile, -0.20 README, -0.30 vendor, ...
    BreadthPenalty  float64  // -0.15 if parent has > 100 children
    LeafPenalty     float64  // -0.05 for scalar leaves with no children
    IdentBonus      float64  // +0.05 for clean idents, -0.10 for whitespace/empty
    GeneratedPen    float64  // -0.30 for `// Code generated` markers
}

func (s Signals) Compose() float64 {
    base := (s.BaseExtractor + s.KindBaseline) / 2.0
    score := base + s.PathPenalty + s.BreadthPenalty + s.LeafPenalty +
             s.IdentBonus + s.GeneratedPen
    return clamp(score, 0.0, 1.0)
}
```

Each field is independently computed and tested. `Compose()` is a pure
function — given identical inputs, identical output, byte-for-byte, on
any platform.

### Lookup tables (live in code, reviewable)

```go
// kind_baseline.go (new)
var kindBaseline = map[string]float64{
    "Function": 1.00, "Method": 1.00, "Class": 1.00, "Interface": 1.00,
    "Setting":  0.95, "Variable": 0.95, "Resource": 0.95,
    "Section":  0.80, "Heading":  0.80, "Block":    0.85,
    "Document": 0.70, "CodeSnippet": 0.70,
}

// path_patterns.go (new)
var pathPatterns = []struct {
    Glob    string
    Penalty float64
    Reason  string  // surfaced on the snapshot for review
}{
    {"**/package-lock.json", -0.40, "npm lockfile"},
    {"**/yarn.lock",         -0.40, "yarn lockfile"},
    {"**/Gemfile.lock",      -0.40, "bundler lockfile"},
    {"**/Cargo.lock",        -0.40, "cargo lockfile"},
    {"**/Pipfile.lock",      -0.40, "pipenv lockfile"},
    {"**/go.sum",            -0.40, "go module checksums"},
    {"**/*.min.js",          -0.40, "minified JS"},
    {"**/*.min.css",         -0.40, "minified CSS"},
    {"**/*.map",             -0.40, "source map"},
    {"vendor/**",            -0.30, "vendored third-party code"},
    {"node_modules/**",      -0.30, "node third-party"},
    {"**/dist/**",           -0.20, "build output"},
    {"**/build/**",          -0.20, "build output"},
    {"README.md",            -0.20, "project README (low-priority docs)"},
    {"**/CHANGELOG.md",      -0.20, "changelog"},
}
```

These tables are the **review surface**. Adding/changing a pattern is a one-
line PR. Snapshot diffs surface the impact across all four pinned corpora
in `avg_confidence_by_kind`.

### What changes downstream

| Surface | Today | After |
|---|---|---|
| `extraction_confidence` column | per-language constant | per-symbol score |
| `search` tool | no filter | `min_confidence` (default 0.0) |
| `query` tool | no filter | `min_confidence` (default 0.0) |
| `trace` tool | no filter | `min_confidence` (default 0.0) |
| `_meta` envelope | basic | adds `confidence_distribution` (histogram bins) |
| `health` tool | per-language coverage | per-(language, kind) p10/p50/avg |
| Snapshot file | `avg_confidence_by_kind` | adds per-kind p10 (regression gate for over-suppression) |
| Blocklist (#24) | hard yes/no | one input to `path_pattern_penalty` |

## Phased delivery

Each phase is one PR. Each PR's snapshot diff is its rationale.

### Phase 1: Substrate (no behavioral change)

- Add `internal/ast/confidence.go` with `Signals` struct + `Compose()`
- Wire `ExtractWithModule` to build `Signals` per symbol but keep
  `BaseExtractor + KindBaseline = 2.0` and all penalties at 0.0 — net result
  identical to today's per-language constant
- Snapshot diff: **zero**. Numbers don't move.
- Tests: composition orthogonality (apply signals in random order,
  same result), boundedness (worst-case min/max inputs stay in [0,1]),
  determinism (same input → byte-identical output across runs)
- ~150 LOC + tests

### Phase 2: Path patterns + content signals

- Populate `pathPatterns` with the lockfile/minified/vendor list
- Populate `kindBaseline`
- Wire `BreadthPenalty`, `LeafPenalty`, `IdentBonus`, `GeneratedPen`
- Snapshot diff: `avg_confidence_by_kind` for `node-monorepo` drops
  meaningfully (the lockfile-shaped JSON in `package.json` will not be
  affected since #24's blocklist already filters `package-lock.json`,
  but the new path penalty applies to anything #24 doesn't already
  block — vendor dirs, generated files inside the corpus, etc.)
- New negative-assertion section in snapshot files: `confidence_floor_p10`
  per-kind. Lockfiles' Settings p10 < 0.5 becomes the gate.
- ~100 LOC + ~150 LOC tests (one positive + one negative case per signal)

### Phase 3: Tool surface

- Add `min_confidence` parameter to `search`, `query`, `trace`
- Default 0.0 (no behavior change for existing callers)
- Add `_meta.confidence_distribution` histogram to response envelopes
- Update `health` tool's per-language coverage to per-(language, kind)
  with avg/p10/p50
- Snapshot diff: zero (default 0.0 = today's behavior)
- ~200 LOC + tests covering the threshold-filtering correctness gate
  (boundary inclusion, opt-in semantics, default-equivalence)

### Phase 4: Default flip (separate PR, not part of #34's MVP)

- Default `min_confidence` from 0.0 → 0.7 for `search`
- README + tool docs updated
- Single-PR rollback if real-world signal is lost
- Snapshot diff: now non-zero, intentional, with the change in
  `search_relevance` top-hit ranks documented as the rationale

Phase 4 is intentionally separate. Phases 1-3 are mechanical and
reviewable as one or three PRs. Phase 4 is a behavior change with
meaningful blast radius and warrants standalone review.

## Test strategy

Three layers, each enforced at PR time.

### Unit tests (per signal, both directions)

For each signal in `Signals`:
- **Positive:** the signal fires when its trigger pattern is present
  (a `package-lock.json` path produces `PathPenalty == -0.40`).
- **Negative:** the signal does NOT fire on near-misses (a
  `package.json` produces `PathPenalty == 0.0`).

This is the orthogonality gate from the issue's negative-tests section.

### Composition tests (the property gates)

- **Order-independence:** apply signals in 10 random orderings; same
  final score every time.
- **Boundedness:** stress test with worst-case inputs (every penalty
  active, every bonus active); score stays in `[0, 1]`.
- **Determinism:** run identical inputs 100 times; every output bit-
  identical.

### Pinned-corpus assertions (the system gate)

In `testdata/corpus/<name>.snapshot.json`, add:

```json
"confidence_floor_p10": {
    "Setting": 0.5,    // p10 of all Setting symbols' confidence
    "Function": 0.85,
    "Section": 0.6
},
"confidence_negative_assertions": {
    "lockfile_settings_below_0.5": true,
    "vendor_functions_below_threshold": true,
    "real_helm_settings_above_0.9": true
}
```

These are committed expectations. Phase 2's snapshot diff is the rationale
when these change. The `negative_assertions` block follows the same policy
as #33's `extraction_failures_by_reason`: NOT auto-regenerated by
`make corpus-snapshot-update`, requires explicit code-review approval to
flip.

## Risks and tradeoffs

### Risk: Signals interact in surprising ways

A `vendor/lib/foo.go` Go function gets `BaseExtractor=1.0`, `KindBaseline=1.0`,
`PathPenalty=-0.30`. Final score = `1.0 - 0.30 = 0.70`. Right at the default
threshold, slightly off on the wrong side at `min_confidence=0.71`.

**Mitigation:** the threshold is opt-in (Phase 4). Until then, the score
is informational, not gating. The snapshot test catches over-suppression
via the `Function` p10 floor (must stay >= 0.85 for `go-project` corpus,
no vendor in that corpus). When Phase 4 lands, an evaluation across all
four corpora documents the actual distribution.

### Risk: Performance cost on the extract path

Each symbol now runs through 7 lookups + a clamp. On 50k symbols this is
~350k extra ops per cold index — measurable but small. The bench regression
gate (#50) makes this visible at PR time. If `Compose` shows >10ns/symbol
in `BenchmarkIndex_Cold_K8sOps`, we know to optimize before Phase 2 lands.

### Risk: Path patterns become a maintenance treadmill

Every new project shape adds a pattern. **Mitigation:** the patterns are
deliberately limited to clear-cut signals (lockfile shapes, vendored
code conventions, build output). Project-specific patterns belong in
project config, not the global table. The threshold gives end users a
recourse without needing to add patterns.

### Risk: Default-flip in Phase 4 surprises agents

Some agent integration may rely on every symbol surfacing in `search`
results. Defaulting `min_confidence=0.7` filters silently.

**Mitigation:** Phase 4 lands separately, with a snapshot diff showing
exactly which symbols disappear from `search_relevance` top hits. If
the diff is large, we keep default at 0.0 and document `min_confidence`
as opt-in.

## Out of scope

- **Per-symbol confidence learning** (per-codebase ML). Out of scope
  permanently — pincher is single-binary and stateless across runs.
- **Cross-extractor signal sharing** (e.g. "this Go file imports
  many YAML configs, so the YAML files near it score higher"). Adds
  inter-file coupling that breaks per-file extraction's parallel
  goroutine model.
- **Confidence on edges.** Edges already have `confidence` from the
  resolver (`#21`-era work). Different signal model; out of scope.

## Open questions for review

1. **Should `KindBaseline` be per-corpus or global?** A Markdown
   `Section` baseline of 0.80 makes sense in `docs-site`; in a Go
   project it might be 0.60 (most Go markdown is README, not API
   docs). Probably global for Phase 2; revisit if `health` shows
   per-corpus skew.
2. **Should `path_pattern_penalty` accept project-relative or
   absolute paths?** Probably project-relative (matches blocklist).
3. **Negative assertions in `<corpus>.snapshot.json` vs.
   `<corpus>.confidence-assertions.json`?** Splitting keeps the
   primary snapshot reviewable; combining keeps "everything about
   this corpus" in one file. Soft preference for splitting.
4. **Should #24's blocklist stay as a hard pre-filter, or fully
   migrate into the scoring system?** Recommend keep — extracting
   a 5MB minified bundle just to score it 0.0 is wasted work. The
   blocklist is the perf optimization; scoring is the gradient.

---

If you have feedback on shape/sequencing, reply on the PR.
Once approved, Phase 1 lands as a separate code PR; this design doc
gets archived as `design/per-symbol-confidence.md` (committed as
historical artifact, not a living spec).
