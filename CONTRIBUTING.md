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
