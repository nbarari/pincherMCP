#!/usr/bin/env bash
# scripts/comparator-raw-readgrep.sh — FILE-B #1521 v0.86.
#
# "Raw Read+Grep agent loop" comparator. Simulates a non-pincher agent
# finding the same information that a pincher tool call returns, using
# only the primitives an agent without pincher has: grep, find, cat.
#
# For a given task ("find symbol X" / "find usages of Y" / "read context
# around Z"), we run BOTH:
#   1. The pincher tool call (search / symbol / context / trace).
#   2. The simulated raw Read+Grep loop that produces equivalent
#      information.
#
# We capture for each side: wall-clock, bytes read from disk, and a
# rough proxy for tokens that would have entered the agent's context
# (sum of `wc -c` over files cat'd, vs the size of pincher's response).
#
# Output: JSON per task with `{task, pincher_ms, comparator_ms, pincher_bytes, comparator_bytes, ratio}`.

set -euo pipefail

PINCHER_BIN="${PINCHER_BIN:-$(command -v pincher 2>/dev/null || echo "")}"
if [ -z "${PINCHER_BIN}" ] || [ ! -x "${PINCHER_BIN}" ]; then
  echo "::error::PINCHER_BIN not set and no pincher on PATH" >&2
  exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "::error::jq not on PATH — comparator needs jq" >&2
  exit 2
fi

CORPUS="${1:?path to corpus (existing indexed project) required: $0 <corpus>}"
OUT="${2:-out/comparator.json}"
mkdir -p "$(dirname "${OUT}")"

# Comparator tasks — each task pairs a pincher call with the equivalent
# raw-grep loop. The tasks were picked to cover the dominant agent-loop
# shapes: find-symbol, read-with-context, find-callers.
#
# A real comparator suite (post-v0.91) would cover 20+ task shapes; this
# scaffolds the runner with 3 representative tasks so the methodology
# can iterate before scaling.
TARGET_SYMBOL="${TARGET_SYMBOL:-Open}"

results='[]'

# ── Task 1: find-symbol-by-name ──────────────────────────────────────
# Pincher: search query:TARGET_SYMBOL → returns id + signature.
# Comparator: grep -rn "func TARGET_SYMBOL" → returns file:line, then
#             cat the line to confirm signature.
echo "── Task 1: find-symbol-by-name ($TARGET_SYMBOL) ──"

t0=$(date +%s%N)
pin_resp=$("${PINCHER_BIN}" --data-dir /tmp/comp-data --http :0 >/tmp/comp-srv.log 2>&1 &)
PIN_PID=$!
trap 'kill -TERM $PIN_PID 2>/dev/null || true; rm -rf /tmp/comp-data /tmp/comp-srv.log' EXIT
addr=""
for _ in $(seq 1 30); do
  addr=$(grep -oE '127\.0\.0\.1:[0-9]+' /tmp/comp-srv.log | head -1 || true)
  [ -n "${addr}" ] && break
  sleep 0.2
done
if [ -z "${addr}" ]; then
  echo "::error::pincher HTTP gateway did not bind"; exit 1
fi
# Index the corpus.
"${PINCHER_BIN}" --data-dir /tmp/comp-data index "${CORPUS}" >/dev/null 2>&1
pin_resp=$(curl -fsSL -m 5 -H 'Content-Type: application/json' -d "{\"query\":\"${TARGET_SYMBOL}\",\"limit\":5}" "http://${addr}/v1/search")
t1=$(date +%s%N)
pin_ms=$(( (t1 - t0) / 1000000 ))
pin_bytes=$(echo -n "${pin_resp}" | wc -c | tr -d ' ')

# Comparator side: grep + cat (no -n trick that would skip lines).
t0=$(date +%s%N)
comp_lines=$(grep -rn "func ${TARGET_SYMBOL}" "${CORPUS}" || true)
comp_bytes=0
if [ -n "${comp_lines}" ]; then
  # Sum file sizes for the matched files — the agent would `Read` each
  # to get full context.
  while IFS= read -r line; do
    fp=$(echo "${line}" | cut -d: -f1)
    [ -f "${fp}" ] || continue
    sz=$(wc -c < "${fp}" | tr -d ' ')
    comp_bytes=$(( comp_bytes + sz ))
  done <<< "${comp_lines}"
fi
t1=$(date +%s%N)
comp_ms=$(( (t1 - t0) / 1000000 ))

ratio=0
if [ "${comp_bytes}" -gt 0 ] && [ "${pin_bytes}" -gt 0 ]; then
  ratio=$(awk -v c="${comp_bytes}" -v p="${pin_bytes}" 'BEGIN { printf "%.2f", c/p }')
fi

echo "  pincher: ${pin_ms}ms, ${pin_bytes} bytes"
echo "  raw:     ${comp_ms}ms, ${comp_bytes} bytes"
echo "  ratio:   ${ratio}× (comparator/pincher)"

results=$(jq --arg task "find-symbol-by-name" \
  --argjson pm "${pin_ms}" --argjson cm "${comp_ms}" \
  --argjson pb "${pin_bytes}" --argjson cb "${comp_bytes}" \
  --arg r "${ratio}" \
  '. + [{task: $task, pincher_ms: $pm, comparator_ms: $cm, pincher_bytes: $pb, comparator_bytes: $cb, bytes_ratio: ($r|tonumber)}]' \
  <<< "${results}")

# Stop the pincher HTTP gateway.
kill -TERM $PIN_PID 2>/dev/null || true

jq --argjson r "${results}" --arg ts "$(date -u +%FT%TZ)" --arg corp "${CORPUS}" \
  '{schema_version: 1, captured_at: $ts, corpus: $corp, tasks: $r}' <<< '{}' > "${OUT}"

echo
echo "Wrote ${OUT}:"
cat "${OUT}"
echo
echo "::warning::comparator v0.86 ships with 1 task (find-symbol-by-name) as the working reference. Tasks 2-20 (read-with-context, find-callers, multi-symbol composites, etc.) land in v0.87-v0.90 follow-ups."
