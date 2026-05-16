#!/usr/bin/env bash
# pr-issue-consistency.sh — CI gate: PR title's (#N) suffix must match
# any `Closes #M` reference in the PR body when both are present (#1103).
#
# Failure mode this catches: PR title uses the conventional-commit suffix
# (#1135) which GitHub does NOT auto-interpret as a close reference, and
# the body has an explicit `Closes #M` line with a different N — the
# wrong issue gets closed. Observed twice in 24h on this repo (#1075 +
# #1094 closed by unrelated PRs #1092 / #1095).
#
# Usage:
#   scripts/pr-issue-consistency.sh "<title>" "<body>"
#
# The CI workflow invokes this with the event payload's title + body.
#
# Exit codes:
#   0 — consistent (or no issue numbers present in either field — nothing to check)
#   1 — title (#N) and body Closes #M disagree
#   2 — usage error

set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  echo "usage: $0 <pr-title> <pr-body>" >&2
  exit 2
fi

TITLE="$1"
BODY="$2"

# Extract the LAST (#N) from the title — conventional-commit suffix
# convention puts the issue number at the end, after the subject.
# Greedy match captures only the trailing reference, not earlier #refs
# that may appear inside the subject for context.
TITLE_NUM=$(printf '%s\n' "$TITLE" | grep -oE '\(#([0-9]+)\)[^(]*$' | grep -oE '[0-9]+' | head -1 || true)

if [[ -z "$TITLE_NUM" ]]; then
  echo "pr-issue-consistency: title has no (#N) suffix — nothing to check"
  exit 0
fi

# Extract every Closes/Fixes/Resolves #M reference from the body.
# GitHub recognises these (case-insensitive) as auto-close keywords:
# close / closes / closed / fix / fixes / fixed / resolve / resolves / resolved.
# We accept any of them.
BODY_NUMS=$(printf '%s\n' "$BODY" | grep -oEi '\<(close[sd]?|fix(e[sd])?|resolve[sd]?)\s*:?\s*#([0-9]+)' | grep -oE '#[0-9]+' | tr -d '#' | sort -u || true)

if [[ -z "$BODY_NUMS" ]]; then
  echo "pr-issue-consistency: body has no Closes/Fixes/Resolves #M reference — nothing to check"
  exit 0
fi

# Body has one or more close-refs. They should all agree with the title's
# trailing (#N) — or the title (#N) should appear among them (chained-PR
# case where the title cites issue A but the PR also closes A's parent B).
FOUND=0
for n in $BODY_NUMS; do
  if [[ "$n" == "$TITLE_NUM" ]]; then
    FOUND=1
    break
  fi
done

if [[ "$FOUND" -eq 0 ]]; then
  echo "pr-issue-consistency: MISMATCH" >&2
  echo "  title cites: #$TITLE_NUM" >&2
  echo "  body closes: $(printf '#%s ' $BODY_NUMS)" >&2
  echo "" >&2
  echo "The title's (#N) suffix and the body's Closes/Fixes #M references disagree." >&2
  echo "GitHub will auto-close the body's #M, not the title's #N — which can close" >&2
  echo "an unrelated issue (this happened twice in 24h on this repo, see #1103)." >&2
  echo "" >&2
  echo "Fix one of:" >&2
  echo "  - Title suffix: change \"(#$TITLE_NUM)\" to match the body's close target." >&2
  echo "  - Body close: change \"Closes #...\" to match the title's #$TITLE_NUM." >&2
  echo "  - If the PR genuinely chains (title cites one issue, body closes another)," >&2
  echo "    add a Closes #$TITLE_NUM line so the title's issue is also in the close set." >&2
  exit 1
fi

echo "pr-issue-consistency: title #$TITLE_NUM matches body close-refs ($(printf '#%s ' $BODY_NUMS)) — clean"
exit 0
