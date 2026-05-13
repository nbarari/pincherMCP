#!/usr/bin/env bash
# changelog-assemble.sh — collect CHANGELOG.d/*.md stubs into CHANGELOG.md (#681 Bucket C).
#
# Pre-Bucket-C, every PR added a bullet directly under
# `## [Unreleased] / ### Added` (or Fixed / Removed / Changed) in
# CHANGELOG.md. Two PRs touching the same section produced a Git merge
# conflict on the second-to-merge — manual edit per rebase, recurring
# cost across every release cycle.
#
# Per-PR stub files in CHANGELOG.d/ are purely additive — never conflict.
# This script assembles them at release-prep time.
#
# Convention:
#   CHANGELOG.d/<issue-or-pr-number>.<type>.md
#     <type> ∈ {added, changed, fixed, removed}
#   File body: one bullet (no leading dash; assembler adds it).
#
# Usage:
#   scripts/changelog-assemble.sh           # preview only — print the assembled section to stdout
#   scripts/changelog-assemble.sh --apply   # rewrite CHANGELOG.md + remove the stub files
#
# See CHANGELOG.d/README.md for the full convention.

set -euo pipefail

CHANGELOG="${CHANGELOG_FILE:-CHANGELOG.md}"
STUBS_DIR="${CHANGELOG_STUBS_DIR:-CHANGELOG.d}"
APPLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply)  APPLY=1; shift ;;
    -h|--help)
      sed -n '2,21p' "$0"
      exit 0
      ;;
    *)
      echo "unknown arg: $1" >&2
      exit 2
      ;;
  esac
done

# Group stub files by type. Order matters in output: Added → Changed →
# Fixed → Removed (Keep a Changelog convention). Within each group,
# sort by filename so PR-number ordering is stable.
declare -a TYPES=("added" "changed" "fixed" "removed")
declare -A SECTION_LABELS=(
  [added]="### Added"
  [changed]="### Changed"
  [fixed]="### Fixed"
  [removed]="### Removed"
)

# Build the assembled section into a temp file.
ASSEMBLED="$(mktemp)"
trap 'rm -f "$ASSEMBLED"' EXIT

STUB_COUNT=0
for type in "${TYPES[@]}"; do
  # Use glob with nullglob-on-the-fly so non-matching pattern collapses
  # to empty array instead of literal pattern (default shell behaviour).
  shopt -s nullglob
  files=("$STUBS_DIR"/*."$type".md)
  shopt -u nullglob
  if [[ ${#files[@]} -eq 0 ]]; then continue; fi
  # Sort by filename for stable PR-number ordering.
  IFS=$'\n' files=($(sort <<< "${files[*]}"))
  unset IFS
  echo "${SECTION_LABELS[$type]}" >> "$ASSEMBLED"
  for f in "${files[@]}"; do
    # Strip trailing newline; ensure single bullet prefix.
    body=$(awk 'NF' "$f")  # drop blank lines (collapsed for one-bullet case)
    if [[ -z "$body" ]]; then
      echo "::warning::empty stub $f — skipping" >&2
      continue
    fi
    # If the body already starts with -, take it as-is. Otherwise prepend "- ".
    if [[ "${body:0:1}" == "-" ]]; then
      echo "$body" >> "$ASSEMBLED"
    else
      echo "- $body" >> "$ASSEMBLED"
    fi
    STUB_COUNT=$((STUB_COUNT + 1))
  done
  echo "" >> "$ASSEMBLED"
done

if [[ $STUB_COUNT -eq 0 ]]; then
  echo "changelog-assemble: no stub files in $STUBS_DIR/ — nothing to assemble" >&2
  exit 0
fi

if [[ $APPLY -eq 0 ]]; then
  echo "=== assembled [Unreleased] content ($STUB_COUNT stub(s)) ==="
  cat "$ASSEMBLED"
  echo "=== end ==="
  echo ""
  echo "rerun with --apply to rewrite $CHANGELOG and remove the stubs"
  exit 0
fi

# --apply path: insert the assembled content under "## [Unreleased]" in
# CHANGELOG, then delete the stub files. The insertion preserves any
# pre-existing entries written directly into [Unreleased] (legacy path).
if [[ ! -f "$CHANGELOG" ]]; then
  echo "changelog-assemble: $CHANGELOG not found" >&2
  exit 1
fi

# awk: print everything; when we hit the [Unreleased] header, print it
# then drop in the assembled content immediately after the blank line
# that follows.
NEW="$(mktemp)"
awk -v assembled="$(cat "$ASSEMBLED")" '
  /^## \[Unreleased\]/ {
    print
    getline blank
    print blank
    print assembled
    next
  }
  { print }
' "$CHANGELOG" > "$NEW"
mv "$NEW" "$CHANGELOG"

# Remove only the .md stubs we consumed; leave README.md and .gitkeep.
for type in "${TYPES[@]}"; do
  rm -f "$STUBS_DIR"/*."$type".md
done

echo "changelog-assemble: applied $STUB_COUNT stub(s) to $CHANGELOG and cleared $STUBS_DIR/"
