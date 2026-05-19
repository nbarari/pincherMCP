#!/usr/bin/env bash
# scripts/host-conformance.sh — FILE-M #1532 v0.87 corpus runner.
#
# Replays the JSON-RPC transcripts in testdata/host-conformance/<host>/
# against a freshly-built pincher (no real host needed) and validates
# responses against the host's expectations.json.
#
# Drift caught here would break the listed host's canonical workflow.
# v0.87: advisory (informational). v0.91 (#1536 FILE-R): release-blocker.
#
# Usage:
#   PINCHER_BIN=/abs/path scripts/host-conformance.sh [host_name|all]
#
# Defaults:
#   PINCHER_BIN     — derived from `command -v pincher` if unset.
#   host_name       — "all" (run every host in testdata/host-conformance/).

set -euo pipefail

PINCHER_BIN="${PINCHER_BIN:-$(command -v pincher 2>/dev/null || echo "")}"
if [ -z "${PINCHER_BIN}" ] || [ ! -x "${PINCHER_BIN}" ]; then
  echo "::error::PINCHER_BIN not set and no pincher on PATH" >&2
  exit 2
fi

# Tooling check: jq is required for response parsing.
if ! command -v jq >/dev/null 2>&1; then
  echo "::error::jq not on PATH — host conformance gate requires jq for response parsing." >&2
  exit 2
fi

HOST_ARG="${1:-all}"
CORPUS_ROOT="$(cd "$(dirname "$0")/.." && pwd)/testdata/host-conformance"

if [ ! -d "${CORPUS_ROOT}" ]; then
  echo "::error::No host-conformance corpus at ${CORPUS_ROOT}" >&2
  exit 2
fi

# Resolve which hosts to run.
hosts=()
if [ "${HOST_ARG}" = "all" ]; then
  for d in "${CORPUS_ROOT}"/*/; do
    [ -d "$d" ] || continue
    hosts+=("$(basename "$d")")
  done
else
  hosts=("${HOST_ARG}")
fi

if [ ${#hosts[@]} -eq 0 ]; then
  echo "::warning::No hosts found under ${CORPUS_ROOT}" >&2
  exit 0
fi

fail_count=0
for host in "${hosts[@]}"; do
  host_dir="${CORPUS_ROOT}/${host}"
  if [ ! -d "${host_dir}" ]; then
    echo "::error::unknown host '${host}' (no directory at ${host_dir})" >&2
    fail_count=$(( fail_count + 1 ))
    continue
  fi

  workflow="${host_dir}/workflow.jsonl"
  expectations="${host_dir}/expectations.json"
  if [ ! -f "${workflow}" ] || [ ! -f "${expectations}" ]; then
    echo "::error::host '${host}' missing workflow.jsonl or expectations.json" >&2
    fail_count=$(( fail_count + 1 ))
    continue
  fi

  echo "── host: ${host} ──────────────────────────────"

  # Build the input stream: every line whose direction is client→server
  # becomes one JSON-RPC line piped to pincher's stdin. Server→client
  # lines are expectations against the responses we capture.
  work=$(mktemp -d -t hostconf-XXXXXX)
  trap 'rm -rf "$work"' EXIT
  in_stream="${work}/in.jsonl"
  out_stream="${work}/out.jsonl"

  jq -c 'select(.direction == "client→server") | .payload' "${workflow}" > "${in_stream}"
  if [ ! -s "${in_stream}" ]; then
    echo "::warning::host '${host}' workflow.jsonl has no client→server lines" >&2
    fail_count=$(( fail_count + 1 ))
    continue
  fi

  # Spin up pincher in stdio mode, feed the request stream, capture the
  # response stream. The replay does not require an indexed project
  # because the canonical flow's first call is the MCP initialize +
  # tools/list — both work against an empty server.
  pin_data="${work}/data"
  mkdir -p "${pin_data}"
  "${PINCHER_BIN}" --data-dir "${pin_data}" < "${in_stream}" > "${out_stream}" 2>"${work}/stderr.log" || true

  if [ ! -s "${out_stream}" ]; then
    echo "::error::host '${host}': pincher produced no responses (stderr below)" >&2
    cat "${work}/stderr.log" >&2 | tail -20
    fail_count=$(( fail_count + 1 ))
    continue
  fi

  # Required-tools assertion (single jq pass over the tools/list
  # response). The expectations.json lists names every host depends on;
  # missing any of them is a release blocker by definition.
  required_tools=$(jq -r '.assertions.tools_list_must_include[]' "${expectations}")
  tools_response=$(jq -c 'select(.id == 2)' "${out_stream}")
  if [ -z "${tools_response}" ]; then
    echo "::error::host '${host}': no tools/list response (id=2) in capture" >&2
    fail_count=$(( fail_count + 1 ))
    continue
  fi

  missing=0
  for tool in ${required_tools}; do
    if ! jq -e --arg t "${tool}" '.result.tools[] | select(.name == $t)' <<< "${tools_response}" >/dev/null; then
      echo "::error::host '${host}': required tool '${tool}' missing from tools/list response" >&2
      missing=$(( missing + 1 ))
    fi
  done
  if [ "${missing}" -gt 0 ]; then
    fail_count=$(( fail_count + 1 ))
    continue
  fi

  # Search response: regex must match somewhere in result.content[0].text.
  search_response=$(jq -c 'select(.id == 3)' "${out_stream}")
  if [ -z "${search_response}" ]; then
    echo "::error::host '${host}': no search response (id=3)" >&2
    fail_count=$(( fail_count + 1 ))
    continue
  fi
  search_regex=$(jq -r '.assertions.search_response_shape.content_text_regex' "${expectations}")
  search_text=$(jq -r '.result.content[0].text // empty' <<< "${search_response}")
  if ! grep -qiE "${search_regex}" <<< "${search_text}"; then
    echo "::error::host '${host}': search response text did not match required regex /${search_regex}/" >&2
    echo "  got: ${search_text:0:200}" >&2
    fail_count=$(( fail_count + 1 ))
    continue
  fi

  # Error-envelope forbidden lines — id=1, id=2, id=3, id=4. Any of them
  # carrying an error means the canonical flow does not work.
  for id in 1 2 3 4; do
    if jq -e --argjson id "${id}" 'select(.id == $id) | (.error // (.result | objects.isError == true))' "${out_stream}" >/dev/null 2>&1; then
      echo "::error::host '${host}': line id=${id} returned an error envelope" >&2
      fail_count=$(( fail_count + 1 ))
      break
    fi
  done

  if [ "${fail_count}" -eq 0 ]; then
    echo "host '${host}': PASS"
  fi
done

if [ "${fail_count}" -gt 0 ]; then
  echo
  echo "::error::host-conformance: ${fail_count} host(s) failed"
  exit 1
fi
echo
echo "host-conformance: all hosts pass"
