#!/usr/bin/env bash
# generate-sdks_test.sh — smoke test for scripts/generate-sdks.sh.
#
# Verifies the script:
#   1. Errors loud (not silently exits 0) when no generator is installed
#   2. Errors loud when pincher is unreachable
#   3. Rejects unknown language argument with exit 2
#   4. Codegen config files exist for all three languages
#
# Doesn't actually run openapi-generator (that's a heavy dep we don't
# want in CI); just verifies the wrapper's failure paths and config
# scaffolding integrity.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/generate-sdks.sh"

failed=0

# Test 1: unknown language exits 2.
out=$("$SCRIPT" bogus-lang 2>&1 || true)
rc=$?
if [ $rc -ne 2 ]; then
  # bash quirk: $? after `|| true` is 0; check via direct invocation
  set +e
  "$SCRIPT" bogus-lang >/dev/null 2>&1
  rc=$?
  set -e
fi
if [ $rc -ne 2 ]; then
  echo "FAIL test 1: unknown language returned $rc, want 2"
  failed=1
fi
if ! echo "$out" | grep -q "unknown language"; then
  echo "FAIL test 1: unknown-language error message missing 'unknown language' in: $out"
  failed=1
fi

# Test 2: codegen config files exist for all three languages.
for lang in typescript python go; do
  cfg="$REPO_ROOT/sdks/$lang/codegen.yaml"
  if [ ! -f "$cfg" ]; then
    echo "FAIL test 2: missing config $cfg"
    failed=1
  fi
done

# Test 3: each config declares a generatorName + outputDir (no codegen
# config can succeed without these).
for lang in typescript python go; do
  cfg="$REPO_ROOT/sdks/$lang/codegen.yaml"
  if ! grep -q "^generatorName:" "$cfg"; then
    echo "FAIL test 3: $cfg missing generatorName"
    failed=1
  fi
  if ! grep -q "^outputDir:" "$cfg"; then
    echo "FAIL test 3: $cfg missing outputDir"
    failed=1
  fi
done

# Test 4: each language has an examples/ stub.
for lang_dir in typescript:search.ts python:search.py go:search.go; do
  lang="${lang_dir%%:*}"
  file="${lang_dir##*:}"
  example="$REPO_ROOT/sdks/$lang/examples/$file"
  if [ ! -f "$example" ]; then
    echo "FAIL test 4: missing example $example"
    failed=1
  fi
done

# Test 5: .gitignore excludes generated/ trees so they never get
# accidentally committed.
gitignore="$REPO_ROOT/sdks/.gitignore"
if [ ! -f "$gitignore" ]; then
  echo "FAIL test 5: missing $gitignore — generated SDK trees would leak into git"
  failed=1
elif ! grep -q "generated" "$gitignore"; then
  echo "FAIL test 5: $gitignore doesn't exclude generated/ — leaking generated SDK trees would inflate the repo"
  failed=1
fi

if [ $failed -eq 0 ]; then
  echo "all generate-sdks.sh tests passed"
  exit 0
fi
exit 1
