# CHANGELOG.d/ — per-PR CHANGELOG stub files

> Eliminates merge conflicts on `CHANGELOG.md`'s `[Unreleased]` section (#681 Bucket C). Pattern adapted from [`towncrier`](https://towncrier.readthedocs.io/) but kept dependency-free — pure bash assembly script.

## Why

Pre-Bucket-C, every PR added a bullet under `## [Unreleased] / ### Added` (or Fixed / Removed / Changed). Two PRs touching the same section produced a Git merge conflict on the second-to-merge PR — manual edit per rebase, recurring cost across every release cycle (saw ~5 conflicts in v0.53/v0.54 alone).

Per-PR stub files are purely additive — two PRs touching different files in `CHANGELOG.d/` never conflict.

## File-naming convention

```
CHANGELOG.d/<issue-or-pr-number>.<type>.md
```

- `<issue-or-pr-number>`: digits only, the issue or PR this entry tracks (no `#` prefix).
- `<type>`: one of `added` / `changed` / `fixed` / `removed` — maps to the [Keep a Changelog](https://keepachangelog.com/) section it lands under.
- `.md`: required so editors syntax-highlight the bullet.

Examples:

```
CHANGELOG.d/691.fixed.md       # PR #691 → goes into ### Fixed
CHANGELOG.d/692.removed.md     # PR #692 → goes into ### Removed
CHANGELOG.d/693.added.md       # PR #693 → goes into ### Added
```

## File contents

One bullet per file, no leading `-` (the assembler adds it):

```markdown
**Short headline ([#NNN](https://github.com/kwad77/pincher/issues/NNN)).** Longer body explaining the change, its `_why_`, any flags or migration notes. Multi-paragraph allowed.
```

## Assembly (release-prep time)

```bash
# Preview — prints what the [Unreleased] section will look like
scripts/changelog-assemble.sh

# Apply — rewrites CHANGELOG.md inserting the assembled section under
# [Unreleased] and removes the stub files. Run this in your release-prep PR.
scripts/changelog-assemble.sh --apply
```

The `--apply` flow is what the [release-prep checklist](../CLAUDE.md#release-prep-checklist-every-release-no-skipping) item references. Always run on a clean working tree so the diff is reviewable.

## CI gate

`changelog-stub-check` (CI job) runs on every PR. If a PR touches `.go` / `.yml` / `.sh` files but adds no new `CHANGELOG.d/*.md` stub, the job fails with a hint to add one. Pure-doc PRs (only `*.md` files outside `CHANGELOG.d/` itself) are exempt.

## Migration / fallback

The pre-Bucket-C pattern (writing directly into `CHANGELOG.md`'s `[Unreleased]`) **still works** — the assembler is additive, not exclusive. If a PR finds it easier to edit `CHANGELOG.md` directly, that's fine; the stub files are the conflict-free path, not a hard requirement.

## See also

- [#681](https://github.com/kwad77/pincher/issues/681) — v0.55 CI hardening umbrella (parent)
- [`scripts/changelog-assemble.sh`](../scripts/changelog-assemble.sh) — the assembler
- [`scripts/changelog-stub-check.sh`](../scripts/changelog-stub-check.sh) — the CI gate
