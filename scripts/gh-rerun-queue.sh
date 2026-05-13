#!/usr/bin/env bash
# gh-rerun-queue.sh — wait for a CI run to be rerunnable, then trigger rerun (#681 Bucket E2).
#
# Pain point this fixes:
#
#   $ gh run rerun 12345 --failed
#   run 12345 cannot be rerun; This workflow is already running
#
# Hit during v0.53 PR #679 — wanted to immediately re-run a failed
# Windows test, got blocked because GitHub considers a run "still in
# progress" until every sibling job (including continue-on-error advisory
# jobs that already failed) reaches terminal state. The standard advice
# ("just wait and click Re-run failed") puts the burden on the operator;
# this script automates the wait + dispatch.
#
# Usage:
#   scripts/gh-rerun-queue.sh <run-id> [--failed|--all]
#     <run-id>: the run to rerun (gh run list to find it)
#     --failed: rerun only failed jobs (default; matches `gh run rerun --failed`)
#     --all:    rerun all jobs in the workflow
#
# Polls every 30s up to 30 min for the run to enter `rerunnable=true` state,
# then triggers rerun. Exits 0 on successful trigger; 1 on timeout or error.

set -euo pipefail

if [[ $# -lt 1 ]]; then
  sed -n '2,21p' "$0"
  exit 2
fi

RUN_ID="$1"
SCOPE="--failed"
if [[ $# -ge 2 ]]; then
  case "$2" in
    --failed) SCOPE="--failed" ;;
    --all)    SCOPE="" ;;
    *) echo "unknown scope: $2" >&2; exit 2 ;;
  esac
fi

DEADLINE=$(($(date +%s) + 1800))  # 30 min budget

while true; do
  # `gh run view --json status` returns the workflow status. Once it's
  # `completed`, the rerun endpoint is dispatchable.
  STATUS=$(gh run view "$RUN_ID" --json status --jq '.status' 2>/dev/null || echo "error")

  case "$STATUS" in
    completed)
      echo "gh-rerun-queue: run $RUN_ID is completed; triggering rerun ($SCOPE)" >&2
      # shellcheck disable=SC2086  # SCOPE intentionally splits
      gh run rerun "$RUN_ID" $SCOPE
      exit 0
      ;;
    in_progress|queued|waiting|requested|pending)
      ;; # keep polling
    error)
      echo "gh-rerun-queue: run $RUN_ID not found (gh error)" >&2
      exit 1
      ;;
    *)
      echo "gh-rerun-queue: run $RUN_ID has unexpected status '$STATUS' — bailing" >&2
      exit 1
      ;;
  esac

  if [[ $(date +%s) -ge $DEADLINE ]]; then
    echo "gh-rerun-queue: timed out after 30 min waiting for run $RUN_ID to complete" >&2
    exit 1
  fi

  echo "gh-rerun-queue: run $RUN_ID status=$STATUS; waiting 30s..." >&2
  sleep 30
done
