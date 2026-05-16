#!/usr/bin/env bash
# Self-test for pr-issue-consistency.sh.
# Run via: bash scripts/pr-issue-consistency_test.sh
# Exit 0 = all cases passed; non-zero = at least one assertion failed.

set -u

SCRIPT="$(dirname "$0")/pr-issue-consistency.sh"
PASS=0
FAIL=0

assert_exit() {
  local want="$1"; shift
  local desc="$1"; shift
  local title="$1"; shift
  local body="$1"; shift
  bash "$SCRIPT" "$title" "$body" >/dev/null 2>&1
  local got="$?"
  if [[ "$got" == "$want" ]]; then
    echo "PASS: $desc (exit $got)"
    PASS=$((PASS+1))
  else
    echo "FAIL: $desc (want exit $want, got $got)"
    echo "  title: $title"
    echo "  body:  $body"
    FAIL=$((FAIL+1))
  fi
}

# Happy paths — exit 0.
assert_exit 0 "title+body match same number" "fix: foo (#1135)" "Closes #1135."
assert_exit 0 "no title (#N) → skip" "chore: bump deps" "Closes #999."
assert_exit 0 "no body close → skip" "fix: foo (#1)" "No close ref here."
assert_exit 0 "multi-close with title number present" "fix: foo (#1)" "Closes #1
Closes #2"
assert_exit 0 "case-insensitive close" "fix: foo (#5)" "CLOSES #5"
assert_exit 0 "fixes keyword" "fix: bar (#7)" "Fixes #7."
assert_exit 0 "resolves keyword" "fix: baz (#9)" "Resolves #9."

# The actual #1103 bug shapes — exit 1.
assert_exit 1 "PR #1092 shape: title cites #1063 but body closes #1075" \
  "fix(init): surface skipped (#1063)" \
  "Same family as #1063/#1064/#1065. Closes #1075."
assert_exit 1 "PR #1095 shape: title cites #1093 but body closes #1094" \
  "fix(dead_code): all-unknown filter (#1093)" \
  "Companion to #1093. Closes #1094."
assert_exit 1 "title #100 body Fixes #200 (disagreement)" \
  "fix: thing (#100)" "Fixes #200"

# Usage error — exit 2. Test via direct invocation (assert_exit always
# passes two args, so we exercise the wrong-arg-count path directly).
bash "$SCRIPT" >/dev/null 2>&1
GOT="$?"
if [[ "$GOT" == "2" ]]; then
  echo "PASS: no args → exit 2 (usage error)"
  PASS=$((PASS+1))
else
  echo "FAIL: no args expected exit 2, got $GOT"
  FAIL=$((FAIL+1))
fi

echo ""
echo "tests: $PASS passed, $FAIL failed"
exit $FAIL
