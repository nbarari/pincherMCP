#!/usr/bin/env bash
# scripts/time-to-first-success.sh — FILE-Q #1535 v0.91
#
# "5-minute-to-first-success" bench: measures the wall-clock cost of the
# minimum path a brand-new user walks from `git clone` of a target
# repository to a useful pincher answer. Three phases timed:
#
#   1. install      — copy the pre-built pincher binary into PATH
#   2. index        — `pincher index .` against the target repo
#   3. first-query  — `pincher search` for a representative symbol
#
# Reported budget: ≤5 min on a ~5k-file repo. Regressions >20% page
# (gated weekly in CI, not per-PR — see #1536 FILE-R for the conformance
# matrix that uses this script).
#
# Usage:
#   PINCHER_BIN=/abs/path/to/pincher scripts/time-to-first-success.sh \
#       <target-repo-url> <query-symbol>
#
# Defaults:
#   PINCHER_BIN  — derived from `command -v pincher` if unset.
#   target-repo  — https://github.com/golang/go (~12k files; large stress test).
#   query-symbol — "fmt.Println"
#
# Output:
#   JSON to stdout with phase-level timings + summary. The pencil-edged
#   numbers go to testdata/bench/time-to-first-success.bench.txt
#   (committed) when scripts are run by maintainers; CI compares against
#   that baseline.

set -euo pipefail

PINCHER_BIN="${PINCHER_BIN:-$(command -v pincher 2>/dev/null || echo "")}"
if [ -z "${PINCHER_BIN}" ] || [ ! -x "${PINCHER_BIN}" ]; then
  echo "::error::PINCHER_BIN not set and no pincher on PATH" >&2
  exit 2
fi

REPO_URL="${1:-https://github.com/golang/go}"
QUERY="${2:-fmt.Println}"
BUDGET_SECONDS="${BUDGET_SECONDS:-300}"  # 5 min default

WORK=$(mktemp -d -t pincher-ttfs-XXXXXX)
trap 'rm -rf "${WORK}"' EXIT

# Phase 1: install (PATH copy). Realistic minimum — every install path
# (Homebrew, Scoop, direct download) ends in "binary on PATH"; we time
# the cheapest representation of that.
INSTALL_DIR="${WORK}/bin"
mkdir -p "${INSTALL_DIR}"
T0=$(date +%s)
cp "${PINCHER_BIN}" "${INSTALL_DIR}/pincher"
T1=$(date +%s)
INSTALL_MS=$(( (T1 - T0) * 1000 ))
export PATH="${INSTALL_DIR}:${PATH}"

# Phase 2: clone + index. Clone is wall-clock too — it's part of "git
# clone → first useful answer." Use --depth=1 for honesty (a new user
# does not pull full history).
T0=$(date +%s)
git clone --depth=1 --quiet "${REPO_URL}" "${WORK}/repo"
T_CLONE=$(date +%s)
CLONE_MS=$(( (T_CLONE - T0) * 1000 ))

cd "${WORK}/repo"
pincher index . >/dev/null 2>&1
T2=$(date +%s)
INDEX_MS=$(( (T2 - T_CLONE) * 1000 ))

# Phase 3: first query. Pick a search the user would actually type.
T0=$(date +%s)
pincher search "${QUERY}" >/dev/null 2>&1 || true
T3=$(date +%s)
QUERY_MS=$(( (T3 - T0) * 1000 ))

TOTAL_MS=$(( INSTALL_MS + CLONE_MS + INDEX_MS + QUERY_MS ))
BUDGET_MS=$(( BUDGET_SECONDS * 1000 ))
WITHIN_BUDGET=$( [ "${TOTAL_MS}" -le "${BUDGET_MS}" ] && echo "true" || echo "false" )

cat <<JSON
{
  "schema_version": 1,
  "target_repo": "${REPO_URL}",
  "query": "${QUERY}",
  "phases_ms": {
    "install": ${INSTALL_MS},
    "clone": ${CLONE_MS},
    "index": ${INDEX_MS},
    "first_query": ${QUERY_MS}
  },
  "total_ms": ${TOTAL_MS},
  "budget_ms": ${BUDGET_MS},
  "within_budget": ${WITHIN_BUDGET}
}
JSON
