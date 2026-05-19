# Contributing to pincher

Pincher is a local code-intelligence MCP server. Contributions of all sizes are welcome — bug fixes, new language extractors, dashboard polish, documentation improvements. This doc covers the dev loop for human contributors. For Claude-Code-facing guidance see [`CLAUDE.md`](CLAUDE.md).

## Dev loop in three commands

```bash
git clone https://github.com/kwad77/pincher.git
cd pincher
go test ./...        # full suite, ~90s on a developer laptop
make build PINCHER_BIN=./pincher.exe   # or `go build -o pincher ./cmd/pinch/` on Linux/macOS
```

If the test sweep is green, you can iterate.

## Branch + PR shape

- **Branch from `master`.** Always cut new branches from `master`, never from another in-flight branch — tangled ancestry causes phantom conflicts on GitHub.
- **One PR per logical change.** Small PRs merge faster and review better than mega-PRs.
- **Assign to a milestone.** Every PR is assigned to a milestone at create-time. Pick the current target from [`/milestones`](https://github.com/kwad77/pincher/milestones); default to the next minor if uncertain.
- **CHANGELOG stub.** Drop a `CHANGELOG.d/<num>.<type>.md` file with one bullet (no leading dash). `<type>` is one of `added`, `changed`, `fixed`, `removed`. Stubs get assembled into `CHANGELOG.md` at release time by `bash scripts/changelog-assemble.sh --apply`.

## What CI gates require

Required checks on every PR (skipped on doc-only PRs where noted):

| Gate | What it checks |
|---|---|
| `Test (mac/ubuntu/windows)` | Full `go test ./...` on three platforms. |
| `Coverage` | Combined coverage doesn't drop. |
| `Corpus snapshot` | Per-corpus snapshot in `testdata/corpus/*.snapshot.json` matches. Bump via `make corpus-snapshot-update` if you intentionally changed extraction. |
| `Benchmark smoke` | Bench targets compile + run a short pass. |
| `Release channel rule` | Release-PR titles follow the convention. |
| `Workflow isolation lint` | GitHub workflows don't duplicate inline logic that has a canonical script. |
| `CHANGELOG stub check` | A `CHANGELOG.d/<num>.<type>.md` stub is present (skipped on doc-only PRs). |

## Test conventions

Every fix ships with **positive + negative + control + cross-check** assertions. Pattern:

```go
// Positive: feature behaves as designed on the happy path.
// Negative: feature correctly rejects / clamps / warns on edge inputs.
// Control: unrelated paths are unaffected by the change.
// Cross-check: an adjacent invariant the change could have broken still holds.
```

Specific gates that fail when changes elsewhere don't update them in lockstep:

- **New exported `*Store` method (`internal/db/db.go`):** classify in `readerRoutedStoreMethods` or `writerRoutedStoreMethods` (`internal/db/db_test.go`), or `TestStore_AllExportedMethodsClassified` fails.
- **Schema migration changes:** bump `schema_version` in 5 corpus snapshot files. `make corpus-snapshot-update` regenerates them; on Windows where `make` may be unavailable, `sed -i 's/"schema_version": N/"schema_version": N+1/' testdata/corpus/*.snapshot.json`.
- **Tool-contract changes (descriptions, InputSchema):** regenerate via `go test ./internal/server -run TestToolContract -update-tool-contract`.
- **Dashboard HTML/CSS changes:** regenerate via `go test ./internal/server -run 'TestDashboardHTMLSnapshot|TestDashboardCSS' -count=1 -update-dashboard-snapshot -update-dashboard-css-snapshot`.
- **New language extractor:** update `internal/ast/registry.go` self-registration AND `internal/db/corpus.go` `ClassifyCorpus` AND the v9 SQL trigger WHERE clauses. `TestClassifyCorpus_MatchesSQLTriggerRouting` is the gate.

## JSON response invariants

Two invariants that recur:

- **All slice fields in tool responses must be allocated as `[]T{}`, never `var x []T`.** A nil slice marshals to `null`; consumers iterating without a null-check break. The grep canary: `grep -n "var \w\+ \[\]map\[string\]" internal/server/` should return nothing once a handler is response-stable.
- **Empty-response branches stamp `_meta.empty_reason`** (a stable enum from `internal/server/empty_reason.go`) alongside the prose `_meta.diagnosis`. The enum is the machine-readable signal; diagnosis is the human-readable one.

## Idioms

- **Logging:** `slog` everywhere. `log.Printf` silences under bench `TestMain` and corrupts baselines.
- **Reader pool:** pure SELECT methods use `s.ro.Query` / `s.ro.QueryContext`; writes use `s.db.Exec`. Routing is enforced by the classification gate above.
- **Symbol IDs:** always build via `db.MakeSymbolID(file, qn, kind)`. Never string-concat.

## Release process

- **Minor (`0.X.0`):** features, schema migrations, new CLI surface.
- **Patch (`0.X.Y`):** bug fixes only. No features, no schema changes.

## Semver in pincher 1.x ([ADR-0002](docs/adr/0002-v1-frozen-surface.md))

Starting at v1.0, pincher promises a specific surface. The rules below say exactly what triggers each release type during 1.x. Pre-1.0 these are aspirational; v0.84.0 is the freeze checkpoint after which they bind.

### What is a breaking change?

A change that breaks something in [ADR-0002's frozen surface](docs/adr/0002-v1-frozen-surface.md):

- Renaming or removing an MCP tool listed in `internal/server/mcp_surface_split_test.go expectedMCPTools`.
- Changing a tool's input/output JSON Schema in a way that's not strictly additive (removing a field, renaming a field, changing a field's type, making an optional field required, changing an enum value).
- Removing or renaming a `_meta` envelope field (additive `_v2` / `_v3` extension points are NOT breaking — they ship alongside the original).
- Renaming or removing an HTTP gateway route, or changing an existing route's response shape.
- Renaming or removing a CLI subcommand listed in `internal/server/reference_md_cli_subcommand_parity_test.go expectedCLISubcommands`, or removing a flag from one.
- Changing the symbol ID format produced by `internal/db/db.go MakeSymbolID`.

A breaking change requires a **2.0 release**. There is no in-1.x deprecation cycle for breakage of frozen surface elements — the deprecation cycle is the one-minor warning window required before *removal* of a flag or subcommand, but the removal itself is what bumps to 2.0.

### What is a minor (1.X.0)?

Any non-breaking, non-trivial change:

- Adding a new MCP tool, CLI subcommand, or HTTP route (additive — non-breaking by ADR-0002 rules).
- Adding an optional input field, or a new output field, to an existing tool.
- Adding a `_v2` / `_v3` extension to the `_meta` envelope.
- A schema migration (forward-only — pincher never migrates back).
- A new language extractor or a tier promotion (0.85 → 1.0).
- A perf or memory characterization that crosses a published claim threshold.

### What is a patch (1.X.Y)?

Bug fixes that don't change the published surface:

- Wrong behavior in an existing tool that doesn't change the contract (e.g. ranking-order bug, off-by-one in a heuristic).
- Internal performance fix.
- A wrong / misleading advisory message.
- A bug in the dashboard rendering or CSS.
- A fix to a CHANGELOG / README typo or stale claim.

Patch releases NEVER introduce schema migrations, new MCP tools, new CLI subcommands, or new HTTP routes. If a fix requires any of those, ship it in the next minor.

### Deprecation cycle for removal

Removing a CLI flag, an HTTP route's response field, or an MCP tool output field — anything not already a breaking change but still user-visible — requires:

1. One full minor of `Deprecated:` doc warnings + runtime `slog.Warn` on the deprecated path.
2. Removal in the next minor (or later).

The deprecation window gives users a chance to migrate before the field disappears. Skipping the window is a breaking change requiring a 2.0 release.

### PR-template rule

Every PR touching the frozen surface (per ADR-0002) checks the PR-template box:

> [ ] This PR changes a frozen surface element per [ADR-0002](docs/adr/0002-v1-frozen-surface.md). If yes, the change is either (a) additive, or (b) targeted for the 2.x branch.

Reviewers verify the checkbox claim before merging. CI gates (`TestToolContract_GoldenFile`, the contract tests on every frozen surface element) catch accidental breakage.

Release-prep PR (the one before tagging) MUST touch all five:

1. **`CHANGELOG.md`** — assemble stubs via `bash scripts/changelog-assemble.sh --apply`, then promote `[Unreleased]` to a versioned heading with the release's theme one-liner.
2. **`README.md` roadmap table** — bump prior `🚧 in flight` row to `✅ shipped`; add the next `🚧 in flight` row.
3. **`README.md` Known limitations** — rewrite items whose fix lands this release into past tense.
4. **Version-sensitive claims in README leading paragraph** — tool count, schema version, coverage badge if it moved meaningfully (>1%).
5. **`docs/REFERENCE.md` leading metadata line** — `**Schema version:** vN · **MCP tools:** N · **Languages detected:** ~N`. Bump every release that moves any of those numbers.

Tag pushes trigger the auto-bump workflow for the Homebrew formula and Docker image — those don't go in the release-prep PR.

## Where to look next

- [`CLAUDE.md`](CLAUDE.md) — full dev guidance + architecture notes (longer than this file).
- [`docs/REFERENCE.md`](docs/REFERENCE.md) — every tool, every flag, every endpoint, schema history, performance numbers.
- [`docs/troubleshooting.md`](docs/troubleshooting.md) — top recurring friction items with remediation.
- [`internal/server/empty_reason.go`](internal/server/empty_reason.go) — the empty-response taxonomy enum.

## Reporting bugs

File at https://github.com/kwad77/pincher/issues with:

- pincher version (`pincher --version`).
- Schema version (`pincher health` → `schema_version` field).
- Output of `pincher doctor` (sanitize project paths if sensitive).
- Minimum repro: the tool call + args + the unexpected behaviour.

For confirmed bugs, an `*_test.go` repro alongside the report makes the fix much faster.
