#!/usr/bin/env bash
# Run the bench gate N times, capture each iteration's raw output for
# variance analysis. Output goes to scripts/.bench-variance/run-NN.txt.
# Each iteration takes ~30-60s at -benchtime=2s, so 20 iterations runs
# 10-20 minutes.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/scripts/.bench-variance"
ITERATIONS="${ITERATIONS:-20}"
BENCHTIME="${BENCHTIME:-2s}"

mkdir -p "${OUT_DIR}"
rm -f "${OUT_DIR}/run-"*.txt

cd "${REPO_ROOT}"
echo "Running ${ITERATIONS} iterations at -benchtime=${BENCHTIME}; output → ${OUT_DIR}"

for i in $(seq -f '%02g' 1 "${ITERATIONS}"); do
    echo "==> iteration ${i}/${ITERATIONS}"
    out="${OUT_DIR}/run-${i}.txt"
    {
        echo "## index"
        go test ./internal/index/ -run='^$' -bench=. -benchtime="${BENCHTIME}" -benchmem 2>&1 || true
        echo ""
        echo "## server"
        go test ./internal/server/ -run='^$' -bench=. -benchtime="${BENCHTIME}" -benchmem 2>&1 || true
    } > "${out}"
done

echo "All ${ITERATIONS} iterations complete."
echo "Raw output: ${OUT_DIR}"
