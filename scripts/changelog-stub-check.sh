#!/usr/bin/env bash
# changelog-stub-check.sh — CI gate: PRs touching code must add a CHANGELOG.d/ stub (#681 Bucket C).
#
# Pure-doc PRs (only *.md outside CHANGELOG.d/) are exempt. Otherwise,
# any PR with .go/.yml/.yaml/.sh/.ps1 changes must add at least one
# CHANGELOG.d/<num>.<type>.md stub.
#
# Usage:
#   scripts/changelog-stub-check.sh [base-ref]
#     base-ref defaults to origin/master
#
# Exit codes:
#   0 — clean (stub present, or PR is doc-only and exempt)
#   1 — code changes without a stub
#   2 — usage / git error

set -euo pipefail

BASE="${1:-origin/master}"

# Compute the diff vs the base ref. --diff-filter=AM = Added or Modified
# (we don't care about deletions; deleting code shouldn't require a
# new CHANGELOG entry).
if ! git rev-parse --quiet --verify "$BASE" >/dev/null 2>&1; then
  echo "changelog-stub-check: base ref '$BASE' not found" >&2
  exit 2
fi

CHANGED=$(git diff --name-only --diff-filter=AM "$BASE"...HEAD 2>/dev/null || true)
if [[ -z "$CHANGED" ]]; then
  echo "changelog-stub-check: no changes vs $BASE — clean"
  exit 0
fi

# Categorize changes.
HAS_CODE=0
HAS_NONEXEMPT_DOC=0
HAS_STUB=0
while IFS= read -r f; do
  case "$f" in
    CHANGELOG.d/*.added.md|CHANGELOG.d/*.changed.md|CHANGELOG.d/*.fixed.md|CHANGELOG.d/*.removed.md)
      HAS_STUB=1
      ;;
    CHANGELOG.d/*)
      ;; # README.md, .gitkeep — neither stub nor exemption-disqualifier
    *.go|*.yml|*.yaml|*.sh|*.ps1|Makefile|go.mod|go.sum|Dockerfile)
      HAS_CODE=1
      ;;
    *.md|docs/*|README.md|CLAUDE.md|CHANGELOG.md)
      HAS_NONEXEMPT_DOC=1  # docs *outside* CHANGELOG.d are exempt-eligible
      ;;
    *)
      HAS_CODE=1  # default: treat unknown extensions as code
      ;;
  esac
done <<< "$CHANGED"

# Exemption: PR touches docs only AND no code.
if [[ $HAS_CODE -eq 0 ]]; then
  echo "changelog-stub-check: doc-only PR — stub not required"
  exit 0
fi

if [[ $HAS_STUB -eq 1 ]]; then
  echo "changelog-stub-check: stub present — clean"
  exit 0
fi

cat >&2 <<EOF
changelog-stub-check: PR has code changes but no CHANGELOG.d/<num>.<type>.md stub.

Add one stub file in CHANGELOG.d/ following the convention:

  CHANGELOG.d/<issue-or-pr-number>.<type>.md
    <type> ∈ {added, changed, fixed, removed}

Example:

  echo '**Short headline ([#NNN](https://github.com/kwad77/pincher/issues/NNN)).** body' \\
    > CHANGELOG.d/\$PR_NUMBER.added.md

See CHANGELOG.d/README.md for the full convention.

(Doc-only PRs are exempt. Touch only *.md outside CHANGELOG.d/ to skip this gate.)
EOF
exit 1
