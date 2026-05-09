# Releasing pincherMCP

This document describes how releases are cut and what guarantees they make.
The contract here is what a downstream packager (Homebrew, the Claude plugin,
Docker users) can rely on.

## Versioning

pincherMCP follows [SemVer](https://semver.org/) with the following promises:

- **MAJOR** — breaking schema changes that aren't safe via additive migration,
  removal of MCP tools, or changes that require downstream callers to adapt.
- **MINOR** — additive schema migrations, new MCP tools, new fields on
  existing tool responses, deprecations.
- **PATCH** — bug fixes, performance improvements, internal refactors with
  no observable surface change.

### Schema-freeze policy (post-1.0)

Once 1.0 is tagged, the SQLite schema becomes part of the public contract:

- The `1.x` line ships only **additive** migrations: new tables, new columns
  with defaults, new triggers, new FTS5 indexes. A 1.x build can read every
  prior 1.x DB after running `migrate()` on Open.
- Breaking schema changes (column type changes, dropped tables, renamed
  columns) require a `2.0` major bump and a documented migration path.
- The `pincher` binary refuses to open a DB at a higher schema version than
  it understands (#10 — downgrade-safety guard, shipped in v0.2.1). This
  is load-bearing for the multi-binary scenarios (plugin + Homebrew + stray
  download all sharing one `pincher.db`).

### Tool-contract policy (post-1.0)

The 15 MCP tools (`index`, `symbol`, `symbols`, `context`, `search`, `query`,
`trace`, `changes`, `architecture`, `schema`, `list`, `adr`, `health`, `stats`,
`fetch`) and their JSON Schemas are pinned by golden-file tests in
`internal/server/`. After 1.0:

- **Adding** a new tool, or new fields on an existing tool's input or output,
  is a **MINOR** bump.
- **Removing** a tool, or removing/renaming a field, is a **MAJOR** bump.
- **Behaviour changes** that callers can observe (default value flips,
  filter additions like `min_confidence`'s default move from 0.0 to 0.7)
  are MINOR if the change is opt-out (callers can pass the old default
  explicitly), MAJOR if it isn't.

A failing golden-file diff is the gate — reviewers see exactly what changed
and whether the version bump matches.

## Release procedure

This is the manual procedure for cutting a release. Once we set up automated
release notes from CHANGELOG.md, the human steps shrink to "tag and push".

### 1. Pre-flight (master)

- All in-flight PRs that should ship in this release are merged.
- `go test ./...` is green on master.
- `make corpus-test` is green (pinned-corpus snapshots match).
- `make corpus-bench` (advisory) — surface any regressions for review;
  not a blocker pre-1.0.
- CHANGELOG.md `[Unreleased]` section is populated and ready to be
  promoted to a versioned section.

### 2. Update CHANGELOG.md

- Move `[Unreleased]` content under a new versioned heading with the
  release date.
- Add the new version at the bottom of the link-reference table.
- Recreate an empty `[Unreleased]` section at the top.
- Commit with message `release: prep CHANGELOG for vX.Y.Z`.

### 3. Tag

```bash
# Annotated tag with release notes inline
git tag -a vX.Y.Z -m "$(awk '/^## \[vX.Y.Z\]/,/^## \[/' CHANGELOG.md | head -n -1)"
git push origin vX.Y.Z
```

The `release` GitHub Actions workflow picks up the tag, builds binaries
for `linux/darwin/windows × amd64/arm64`, builds the multi-arch Docker
image, and publishes the GitHub Release with auto-generated artifacts.

### 4. Verify artifacts

- GitHub Releases page shows the new version with binaries + SHA256SUMS.
- `ghcr.io/kwad77/pinchermcp:X.Y.Z` and `:latest` resolve.
- Homebrew formula update PR auto-opens (the `homebrew-auto-bump`
  workflow runs on tag push).

### 5. Verify the Homebrew bump

After the formula PR opens, run the local smoke test:

```bash
brew uninstall pincher
brew untap kwad77/pincher
brew tap kwad77/pincher
brew install pincher
pincher --version
```

Once verified, merge the formula PR.

## Branch policy

### Pre-1.0 (current)

Direct merges to `master` are allowed. PRs are still preferred for non-trivial
changes and remain the historical record (CHANGELOG.md links by PR number).

### Post-1.0

Master is protected:

- All work happens on `feat/*`, `fix/*`, `chore/*`, `docs/*`, `test/*`,
  `perf/*`, `release/*` branches.
- PR + green CI required to merge.
- Squash-merge by default; merge-commit only for cross-cutting refactors
  where the per-commit history is informative.
- Tags only from `master`.
- Force-push to master is forbidden.

## Hotfix procedure

If a critical bug ships in `vX.Y.Z` and master has unrelated work in flight:

1. Branch `hotfix/vX.Y.(Z+1)` from `vX.Y.Z` (not master).
2. Fix the bug, add a regression test, get PR + green CI.
3. Merge to master (or a `release/X.Y` branch if master has diverged).
4. Cherry-pick the fix back to master if needed.
5. Tag `vX.Y.(Z+1)` from the hotfix point and follow the regular release
   procedure from step 2.

For pre-1.0, hotfixes are usually just a regular patch release off master
since master moves slowly enough.
