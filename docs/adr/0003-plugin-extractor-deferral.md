# ADR-0003: Plugin-extractor surface — defer to v1.x

**Status:** Accepted
**Date:** 2026-05-18
**Decision-maker:** kwad77 (sole maintainer through v0.x)
**Issue:** [#1540](https://github.com/kwad77/pincher/issues/1540) (FILE-V design-review gate); supersedes the v0.88 scheduling of [#1333](https://github.com/kwad77/pincher/issues/1333)
**Supersedes:** scheduling line in `.planning-roadmap-to-v1.md`

## Context

[#1333](https://github.com/kwad77/pincher/issues/1333) proposes a stable
plugin point for **language extractors only**: external Go modules that
register an `internal/ast.Extractor` via `pincher --plugin-dir=<path>` and
ship as `buildmode=plugin` `.so` / `.dylib` files. The original
scheduling was v0.74. The v1.0 plan refresh ([`.planning-roadmap-to-v1.md`])
moved the decision gate to v0.88 (FILE-V) with criterion: *"can the
plugin API be frozen with v1.0 quality?"* Defer to v1.x otherwise.

This ADR records the v0.88 design-review outcome.

## Forces

### Interface still hardening

The `Extractor` interface has changed materially across recent releases:

- **v0.73** — universal regex-tier promotion: Swift / Kotlin / C# / PHP /
  C / C++ all moved from approx-0.70 to stable-0.85, requiring new
  modifier/attribute coverage on the shared `regexExtractor` framework
  (#1450 / #1457 / #1459 / #1461 / #1463).
- **v0.76** — `USES_VAR` edge category emitted by extractors as a new
  edge kind alongside CALLS / WRITES (drift between extractors caught
  during the v0.78.1 USES_VAR diag sweep, #1479).
- **v0.67** — `scopeRE` framework added to `regexExtractor` to support
  Rust `impl` / Swift `extension` syntactic-grouping containers (#1183).
- **v0.20** — JS/JSX dispatcher upgrade from 0.85 → 1.0 contingent on
  AST extractor parsing successfully (#266).

Each of these required either a new method, a new field on
`ExtractedSymbol`, or new edge categories. A v1.0 freeze with
additive-only evolution would have prevented every one of them without
a v2.0 break.

### `buildmode=plugin` fragility

Go's plugin support is the binding constraint, not pincher's API design:

- **Linux / macOS only.** No Windows support. Windows is currently
  pincher's largest dev-machine population (per dogfood). A
  Windows-excluded plugin point ships a permanently second-class
  experience to >40% of users.
- **Exact host-version match required.** A plugin built against
  pincher 1.0.0 will refuse to load into 1.0.1 unless rebuilt against
  exact-matching pincher source. Documented Go-runtime constraint, not
  pincher's choice. Practical effect: every patch release breaks every
  third-party plugin.
- **No cross-compile.** A plugin built on linux/amd64 cannot be loaded
  on darwin/arm64 or vice versa. Plugin authors must publish six
  artifacts (the binary matrix from `release.yml`) — a non-trivial
  operational ask for community contributors.
- **`-trimpath` interactions.** The release binaries are built with
  `-trimpath` (`.github/workflows/release.yml`). Plugins that import
  pincher packages must be built with matching flags or the runtime
  hash check fails. Easy to get wrong; reproducer is path-dependent.

### No demand signal

#1333 has been open since v0.74 scheduling. There has been zero external
request — no issues, no thread comments, no discussion — asking for a
plugin slot for an unsupported language. The current 22-extractor
coverage spans the languages every dogfood corpus has encountered.
Shipping a plugin point ahead of demand means inheriting the API-freeze
risk without offsetting it against real use.

### Critical-path resource concentration

v0.81 → v1.0 has [22 FILE-X v1.0-blocker issues](`.planning-roadmap-to-v1.md`)
plus the Phase 4 composite suite, supply-chain hardening, latency
budgets, and migration rehearsal. Adding a plugin-API freeze to the
v0.88 slice spends scarce design-review attention on a surface with no
demand signal, displacing items that DO have a v1.0 acceptance criterion.

## Decision

**Defer #1333 to v1.x. Close #1540 as accepted-deferral.**

Codification:

1. **#1333 milestone change:** moved from `v0.88.0` → `v1.1.0`. The
   issue stays open as a tracked-for-v1.x item; closing it would lose
   the design context already captured in the thread.
2. **`.planning-roadmap-to-v1.md`:** v0.88 row's FILE-V line annotated
   *"Decision: defer per ADR-0003."* The v0.88 milestone keeps its
   other items unchanged.
3. **ADR-0002 (v1.0 frozen surface, #1547):** plugin API is NOT included
   in the v1.0 frozen-surface declaration. Status remains `experimental`
   in the surface-status table.
4. **No code shipped this release** beyond this ADR.

## Consequences

**Positive:**

- Frees v0.88 design-review attention for the FILE-X items that DO have
  v1.0 acceptance criteria.
- Pincher 1.0 ships with a smaller, more honestly-frozen public surface.
  Every API in the v1.0 freeze is one whose evolution path we have
  thought through; the plugin API is not.
- Allows the extractor interface to keep evolving through v0.89 →
  v1.0 without v2.0-break risk. Recent releases prove the interface is
  still finding its shape.
- Avoids the operational tax of supporting third-party plugins on a
  fragile substrate (`buildmode=plugin`) for a v1.0 stability promise.

**Negative:**

- Community contributors who want to add tail-language coverage must
  upstream extractors via PR rather than ship out-of-tree plugins.
  Pre-v1.0 this has not been an issue; the in-tree extractor count
  reached 22 with this workflow.
- Pincher cannot claim "plugin-extensible" on the v1.0 marketing
  surface. The honest position is "22 in-tree extractors at well-defined
  confidence tiers; plugins post-v1.0."

**Mitigations:**

- Extractor contributors can still ship languages via in-tree PRs. The
  registry pattern in `ast/registry.go` makes a new extractor a single
  file plus snapshot updates — well-trodden ground.
- The deferral is reversible. v1.x can promote #1333 back into a
  numbered minor without ADR rework — this ADR explicitly anticipates
  re-evaluation.

## Re-evaluation triggers

Reopen this decision when ANY of the following hold:

1. **External demand signal:** ≥3 distinct issues / discussions from
   different users requesting a plugin slot for a specific language
   pincher does not support.
2. **`buildmode=plugin` constraints relax:** Go publishes Windows
   support OR drops the exact-version-match requirement. Either change
   materially shifts the supply-chain math.
3. **In-tree extractor cadence stalls:** the in-tree contribution rate
   for new languages drops to zero across two consecutive minors
   despite open issues, indicating the in-tree path is the bottleneck
   rather than the plugin path being the missing affordance.
4. **The extractor interface stabilises:** two consecutive minors with
   no non-additive `Extractor` interface change. Currently we have not
   gone more than one minor without one (per the v0.67 / v0.73 / v0.76
   evolution history).

## Related

- [#1540](https://github.com/kwad77/pincher/issues/1540) FILE-V — this ADR closes it.
- [#1333](https://github.com/kwad77/pincher/issues/1333) Plugin point — deferred, milestone moved to v1.1.0.
- [#1547](https://github.com/kwad77/pincher/pull/1547) ADR-0002 v1.0 frozen surface — plugin API listed as `experimental`.
- `internal/ast/registry.go` — current in-tree self-registration pattern (Extractor interface).
- `.github/workflows/release.yml` — release binary matrix that would need to grow if plugin shipping went live.
