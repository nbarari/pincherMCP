#!/usr/bin/env bash
# release-channel.sh — print the release channel for a tag (#642).
#
# Channels:
#   stable — minor version divisible by 10 (v0.60, v0.70, ..., v1.0, v1.10).
#            Promoted via Homebrew formula bump + Docker `latest`/`stable` tags.
#   dev    — every other plain semver tag (v0.53, v0.54, ..., v0.59, v0.61).
#            Published via Docker `dev` tag; never bumps Homebrew.
#   beta   — pre-release with -beta.N suffix.   Docker `beta` tag.
#   alpha  — pre-release with -alpha.N suffix.  Docker `alpha` tag.
#   rc     — pre-release with -rc.N suffix.     Docker `rc` tag.
#
# Patch releases (vX.Y.Z where Z > 0) inherit their minor's channel.
# v0.60.1 = stable; v0.53.1 = dev.
#
# This script is the canonical channel-detection implementation.
# release.yml shells out to this script directly (post-#689) — the
# inline-divergence detector in cmd/workflow-lint (#690 Bucket 2)
# enforces that workflows never reimplement this logic inline.
# scripts/release-channel_test.sh exercises this directly so the rule
# is CI-tested locally without needing to push tags.
#
# Usage: scripts/release-channel.sh v0.60.0 → "stable"

set -euo pipefail

REF="${1:?usage: release-channel.sh <tag>}"

# Pre-release suffixes always win over the minor-modulo rule.
case "$REF" in
  *-beta.*)   echo "beta"; exit 0 ;;
  *-alpha.*)  echo "alpha"; exit 0 ;;
  *-rc.*)     echo "rc"; exit 0 ;;
  *-*)        echo "dev"; exit 0 ;;  # any other -suffix → dev
esac

# Plain semver. Strip leading v, parse the minor.
VER="${REF#v}"
MINOR="$(echo "$VER" | awk -F. '{print $2}')"

if ! [[ "$MINOR" =~ ^[0-9]+$ ]]; then
  echo "::error::could not parse minor version from '$REF'" >&2
  exit 1
fi

if (( MINOR % 10 == 0 )); then
  echo "stable"
else
  echo "dev"
fi
