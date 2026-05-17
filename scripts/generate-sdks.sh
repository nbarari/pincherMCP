#!/usr/bin/env bash
# generate-sdks.sh — regenerate the typed client SDKs (#1262).
#
# Pulls /v1/openapi.json from a running pincher (defaults to
# http://localhost:8080; override via PINCHER_HTTP_URL) and feeds it
# to openapi-generator-cli with the per-language config under
# sdks/<lang>/codegen.yaml. Generated output lands in
# sdks/<lang>/generated/ — gitignored, regenerable.
#
# Usage:
#   ./scripts/generate-sdks.sh                    # all three languages
#   ./scripts/generate-sdks.sh typescript         # one specific language
#   PINCHER_HTTP_URL=http://host:9000 ./scripts/generate-sdks.sh
#
# Generator binary lookup order:
#   1. openapi-generator-cli on PATH
#   2. openapi-generator on PATH (brew formula name)
#   3. docker pull openapitools/openapi-generator-cli (fallback)
#
# Exit codes: 0 = all requested generators ran; 1 = pincher unreachable
# or generator unavailable; 2 = unknown language argument.

set -euo pipefail

PINCHER_HTTP_URL="${PINCHER_HTTP_URL:-http://localhost:8080}"
LANGS=("typescript" "python" "go")

if [ $# -gt 0 ]; then
  case "$1" in
    typescript|python|go) LANGS=("$1") ;;
    *) echo "unknown language: $1 (expected: typescript | python | go)" >&2; exit 2 ;;
  esac
fi

# Locate generator binary or fall back to docker.
GEN=""
if command -v openapi-generator-cli >/dev/null 2>&1; then
  GEN="openapi-generator-cli"
elif command -v openapi-generator >/dev/null 2>&1; then
  GEN="openapi-generator"
elif command -v docker >/dev/null 2>&1; then
  GEN="docker run --rm -v $(pwd):/local openapitools/openapi-generator-cli"
  echo "INFO: using docker openapitools/openapi-generator-cli (no local binary found)"
else
  cat >&2 <<EOF
ERROR: no openapi-generator available. Install one of:
  brew install openapi-generator                              # macOS
  npm install -g @openapitools/openapi-generator-cli          # any platform
  docker pull openapitools/openapi-generator-cli              # any platform
EOF
  exit 1
fi

# Pull the spec from a running pincher.
SPEC_FILE="$(mktemp -t pincher-openapi.XXXXXX.json)"
trap "rm -f \"$SPEC_FILE\"" EXIT

if ! curl --fail --silent --show-error "$PINCHER_HTTP_URL/v1/openapi.json" -o "$SPEC_FILE"; then
  cat >&2 <<EOF
ERROR: failed to fetch $PINCHER_HTTP_URL/v1/openapi.json.

Is pincher running with --http?
  pincher --http :8080

Override the URL via PINCHER_HTTP_URL if your binding differs.
EOF
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for lang in "${LANGS[@]}"; do
  echo "──────────── generating $lang SDK ────────────"
  config="$REPO_ROOT/sdks/$lang/codegen.yaml"
  out="$REPO_ROOT/sdks/$lang/generated"
  if [ ! -f "$config" ]; then
    echo "ERROR: missing config $config" >&2
    exit 1
  fi
  mkdir -p "$out"
  # shellcheck disable=SC2086
  $GEN generate \
    --input-spec "$SPEC_FILE" \
    --config "$config" \
    --output "$out" \
    --skip-validate-spec   # already gated by openapi_spec_validity_test.go
  echo "ok: $out"
done

echo ""
echo "All requested SDKs generated. Outputs (gitignored):"
for lang in "${LANGS[@]}"; do
  echo "  sdks/$lang/generated/"
done
