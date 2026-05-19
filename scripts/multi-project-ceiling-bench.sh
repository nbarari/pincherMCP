#!/usr/bin/env bash
# scripts/multi-project-ceiling-bench.sh — FILE-J #1529 v0.86.
#
# Measures the wall-clock cost of a single watcher poll cycle across
# N indexed projects (synthetic). Output anchors the "recommended max
# concurrent indexed projects" budget published in REFERENCE.md.
#
# Each tier:
#   1. Generates N small synthetic projects (50 files each — keeps
#      per-project work small so the SIGNAL is N, not per-project size).
#   2. Indexes each (one-time cost, not counted in the budget).
#   3. Times one watcher poll cycle against the full set.
#
# Output: JSON with per-tier {project_count, poll_cycle_ms}.

set -euo pipefail

PINCHER_BIN="${PINCHER_BIN:-$(command -v pincher 2>/dev/null || echo "")}"
if [ -z "${PINCHER_BIN}" ] || [ ! -x "${PINCHER_BIN}" ]; then
  echo "::error::PINCHER_BIN not set and no pincher on PATH" >&2
  exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "::error::jq not on PATH" >&2
  exit 2
fi

TIERS="${1:-10,50}"   # 200-tier available via TIERS=10,50,200 on dispatch
OUT="${2:-out/multi-project-ceiling.json}"
PER_PROJECT_FILES="${PER_PROJECT_FILES:-50}"

mkdir -p "$(dirname "${OUT}")"

results='[]'
IFS=',' read -ra TIER_LIST <<< "${TIERS}"

for tier in "${TIER_LIST[@]}"; do
  echo "── tier: ${tier} projects × ${PER_PROJECT_FILES} files each ──────"

  work=$(mktemp -d -t mpcb-XXXXXX)
  trap 'rm -rf "$work"' EXIT
  pin_data="${work}/data"
  mkdir -p "${pin_data}"

  # Generate + index each project. One-shot per project; we want the
  # tier post-condition (N indexed projects in the DB), not the
  # initial-index cost which is FILE-I's territory.
  for p in $(seq 1 "${tier}"); do
    proj_dir="${work}/p${p}"
    bash scripts/generate-synthetic-corpus.sh "${PER_PROJECT_FILES}" "${proj_dir}" >/dev/null
    "${PINCHER_BIN}" --data-dir "${pin_data}" index "${proj_dir}" >/dev/null 2>&1
  done

  # Time a single watcher poll cycle by running `pincher list` with
  # check-on-disk enabled — that traverses every indexed project,
  # which is what the watcher does per tick. Three runs, take median
  # to filter cold-cache outliers.
  samples=()
  for _ in 1 2 3; do
    t0=$(date +%s%N)
    "${PINCHER_BIN}" --data-dir "${pin_data}" list --json >/dev/null 2>&1 || true
    t1=$(date +%s%N)
    ms=$(( (t1 - t0) / 1000000 ))
    samples+=("${ms}")
  done

  sorted=$(printf '%s\n' "${samples[@]}" | sort -n)
  median=$(echo "${sorted}" | sed -n 2p)

  echo "  poll_cycle_ms (median of 3): ${median}"

  results=$(jq --arg t "${tier}" --arg m "${median}" --arg pf "${PER_PROJECT_FILES}" \
    '. + [{project_count: ($t|tonumber), per_project_files: ($pf|tonumber), poll_cycle_ms_median: ($m|tonumber)}]' \
    <<< "${results}")

  rm -rf "${work}"
  trap - EXIT
done

jq --argjson r "${results}" --arg ts "$(date -u +%FT%TZ)" \
  '{schema_version: 1, captured_at: $ts, tiers: $r}' <<< '{}' > "${OUT}"

echo
echo "Wrote ${OUT}:"
cat "${OUT}"
