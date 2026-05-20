#!/usr/bin/env bash
# scripts/comparator-raw-readgrep.sh — FILE-B #1521 v0.86, §2 corpus #1298 v0.89.
#
# "Raw Read+Grep agent loop" comparator. Simulates a non-pincher agent
# obtaining the same information a pincher tool call returns, using only
# the primitives an agent without pincher has: grep, find, cat.
#
# For each canonical task it runs BOTH sides and records what would have
# entered the agent's context window:
#   1. The pincher tool call (HTTP gateway: search / context / query).
#   2. The simulated raw Read+Grep loop producing equivalent information.
#
# Per side we capture wall-clock ms and a token proxy in bytes — the
# pincher HTTP response size vs. what the raw loop pulls into context
# (grep's own output when only *locating*, whole-file `cat` when the
# agent must *read*).
#
# Output: JSON {schema_version, captured_at, corpus, tasks:[{task,
#   pincher_ms, comparator_ms, pincher_bytes, comparator_bytes,
#   bytes_ratio}]}.
#
# #1298 v0.89: the v0.86 runner never executed a single task — four
# bugs each aborted it before any measurement (see the inline notes at
# the server-setup block). This revision fixes them and grows the
# corpus from the 1 placeholder task to 3 canonical agent-loop shapes:
# find-symbol, read-with-context, find-callers.

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

CORPUS="${1:?path to corpus (a project directory to index) required: $0 <corpus> [out.json]}"
if [ ! -d "${CORPUS}" ]; then
  echo "::error::corpus '${CORPUS}' is not a directory" >&2
  exit 2
fi
OUT="${2:-out/comparator.json}"
mkdir -p "$(dirname "${OUT}")"

TARGET_SYMBOL="${TARGET_SYMBOL:-Open}"
# pincher derives a project's name from its directory basename; the
# HTTP tools scope by that name.
PROJECT="$(basename "${CORPUS}")"

# ── Server setup ─────────────────────────────────────────────────────
# Four bugs made the v0.86 runner a no-op; all four are fixed here:
#   1. The server was started as `pin_resp=$(pincher ... &)` — the `&`
#      backgrounds inside the command-substitution subshell, so `$!`
#      in this shell was unbound and `set -u` aborted immediately.
#   2. `pincher --http` also runs the MCP stdio loop; a backgrounded
#      process whose stdin EOFs at once triggers the graceful stdio
#      shutdown and the whole process (HTTP included) exits. `--no-stdio`
#      runs HTTP-only and survives.
#   3. `--http :0` binds all interfaces, which pincher default-denies
#      without --http-key (#199). `--http 127.0.0.1:0` is loopback-only
#      and needs no auth.
#   4. `pincher --data-dir D index PATH` puts `--data-dir` in argv[1],
#      so the `index` subcommand is never detected and pincher runs as
#      the MCP server, indexing nothing. The subcommand must lead:
#      `pincher index PATH --data-dir D`.
COMP_DATA="${COMP_DATA:-$(mktemp -d -t comparator-XXXXXX)}"
COMP_LOG="${COMP_DATA}/srv.log"
PIN_PID=""
cleanup() {
  if [ -n "${PIN_PID}" ]; then
    kill -TERM "${PIN_PID}" 2>/dev/null || true
    # Block until the gateway has actually exited and released its
    # SQLite handles, otherwise `rm` races the still-open DB file.
    wait "${PIN_PID}" 2>/dev/null || true
  fi
  rm -rf "${COMP_DATA}"
}
trap cleanup EXIT

# Index the corpus first, into a populated DB the gateway then reads.
"${PINCHER_BIN}" index "${CORPUS}" --data-dir "${COMP_DATA}" >/dev/null 2>&1

# Start the loopback HTTP-only gateway.
"${PINCHER_BIN}" --data-dir "${COMP_DATA}" --no-stdio --http 127.0.0.1:0 \
  >"${COMP_LOG}" 2>&1 &
PIN_PID=$!

addr=""
for _ in $(seq 1 50); do
  addr=$(grep -oE '127\.0\.0\.1:[0-9]+' "${COMP_LOG}" 2>/dev/null | head -1 || true)
  [ -n "${addr}" ] && break
  sleep 0.2
done
if [ -z "${addr}" ]; then
  echo "::error::pincher HTTP gateway did not bind" >&2
  cat "${COMP_LOG}" >&2 || true
  exit 1
fi
API="http://${addr}"

# ── helpers ──────────────────────────────────────────────────────────
# pin_call TOOL JSON → response body on stdout.
pin_call() {
  curl -fsSL -m 10 -H 'Content-Type: application/json' -d "$2" "${API}/v1/$1"
}

# byte_len → length in bytes of stdin.
byte_len() { wc -c | tr -d ' '; }

# sum_grep_file_bytes → reads `grep -rn` output on stdin, sums `wc -c`
# of each DISTINCT matched file (an agent Reads each file once).
sum_grep_file_bytes() {
  awk -F: 'NF > 1 && !seen[$1]++ { print $1 }' \
    | while IFS= read -r fp; do [ -f "${fp}" ] && wc -c < "${fp}"; done \
    | awk '{ s += $1 } END { print s + 0 }'
}

results='[]'

# record_task NAME PIN_MS COMP_MS PIN_BYTES COMP_BYTES
record_task() {
  local name="$1" pm="$2" cm="$3" pb="$4" cb="$5" ratio=0
  if [ "${cb}" -gt 0 ] && [ "${pb}" -gt 0 ]; then
    ratio=$(awk -v c="${cb}" -v p="${pb}" 'BEGIN { printf "%.2f", c / p }')
  fi
  echo "  pincher: ${pm}ms, ${pb} bytes"
  echo "  raw:     ${cm}ms, ${cb} bytes"
  echo "  ratio:   ${ratio}× (comparator/pincher)"
  results=$(jq \
    --arg task "${name}" \
    --argjson pm "${pm}" --argjson cm "${cm}" \
    --argjson pb "${pb}" --argjson cb "${cb}" \
    --arg r "${ratio}" \
    '. + [{task: $task, pincher_ms: $pm, comparator_ms: $cm,
           pincher_bytes: $pb, comparator_bytes: $cb,
           bytes_ratio: ($r | tonumber)}]' \
    <<< "${results}")
}

# now_ms → wall clock in milliseconds.
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }

# ── Task 1: find-symbol-by-name ──────────────────────────────────────
# Pincher: search → id + signature, one targeted response.
# Comparator: `grep -rn "func TARGET"` — what enters context is grep's
# own output (the agent only needs to LOCATE the symbol here).
echo "── Task 1: find-symbol-by-name (${TARGET_SYMBOL}) ──"
t0=$(now_ms)
search_resp=$(pin_call search \
  "{\"query\":\"${TARGET_SYMBOL}\",\"project\":\"${PROJECT}\",\"limit\":5}")
t1=$(now_ms)
pin_bytes=$(printf '%s' "${search_resp}" | byte_len)

t0c=$(now_ms)
grep_out=$(grep -rn "func ${TARGET_SYMBOL}" "${CORPUS}" || true)
t1c=$(now_ms)
comp_bytes=$(printf '%s' "${grep_out}" | byte_len)
record_task "find-symbol-by-name" "$((t1 - t0))" "$((t1c - t0c))" \
  "${pin_bytes}" "${comp_bytes}"

# ── Task 2: read-with-context ────────────────────────────────────────
# Pincher: context → the symbol's source plus its imports/callees in
# one response.
# Comparator: the agent has located the symbol; to READ it with context
# it must `cat` the whole file(s) — no way to extract just the function
# and its imports with grep/cat. Context = full file bytes.
echo "── Task 2: read-with-context (${TARGET_SYMBOL}) ──"
sym_id=$(printf '%s' "${search_resp}" | jq -r '.results[0].id // empty')
if [ -n "${sym_id}" ]; then
  t0=$(now_ms)
  ctx_resp=$(pin_call context \
    "{\"id\":\"${sym_id}\",\"project\":\"${PROJECT}\"}")
  t1=$(now_ms)
  pin_bytes=$(printf '%s' "${ctx_resp}" | byte_len)

  t0c=$(now_ms)
  decl_grep=$(grep -rn "func ${TARGET_SYMBOL}" "${CORPUS}" || true)
  t1c=$(now_ms)
  comp_bytes=$(printf '%s' "${decl_grep}" | sum_grep_file_bytes)
  record_task "read-with-context" "$((t1 - t0))" "$((t1c - t0c))" \
    "${pin_bytes}" "${comp_bytes}"
else
  echo "::warning::task read-with-context skipped — search returned no id for ${TARGET_SYMBOL}" >&2
fi

# ── Task 3: find-callers ─────────────────────────────────────────────
# Pincher: query (pinchQL) → the resolved CALLS graph, callers only.
# Comparator: `grep -rn "TARGET("` finds candidate call sites, but a
# bare grep can't tell a real call from a comment / string / shadowed
# name — the agent must `cat` each matched file to confirm. Context =
# full file bytes of every file with a candidate match.
echo "── Task 3: find-callers (${TARGET_SYMBOL}) ──"
t0=$(now_ms)
query_resp=$(pin_call query \
  "{\"pinchql\":\"MATCH (caller)-[:CALLS]->(callee) WHERE callee.name = \\\"${TARGET_SYMBOL}\\\" RETURN caller.qualified_name, caller.file\",\"project\":\"${PROJECT}\"}")
t1=$(now_ms)
pin_bytes=$(printf '%s' "${query_resp}" | byte_len)

t0c=$(now_ms)
call_grep=$(grep -rn "${TARGET_SYMBOL}(" "${CORPUS}" || true)
t1c=$(now_ms)
comp_bytes=$(printf '%s' "${call_grep}" | sum_grep_file_bytes)
record_task "find-callers" "$((t1 - t0))" "$((t1c - t0c))" \
  "${pin_bytes}" "${comp_bytes}"

# ── Emit ─────────────────────────────────────────────────────────────
jq \
  --argjson r "${results}" \
  --arg ts "$(date -u +%FT%TZ)" \
  --arg corp "${CORPUS}" \
  '{schema_version: 2, captured_at: $ts, corpus: $corp, tasks: $r}' \
  <<< '{}' > "${OUT}"

echo
echo "Wrote ${OUT}:"
cat "${OUT}"
