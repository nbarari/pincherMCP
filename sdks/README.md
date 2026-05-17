# pincher typed SDKs

Typed client SDKs for TypeScript, Python, and Go — generated from the
`/v1/openapi.json` spec that every running pincher serves. Lets you
script pincher from outside the MCP transport without hand-rolling an
HTTP client.

## Status

**Codegen scaffolding shipped (v0.71).** The config files and the
`scripts/generate-sdks.sh` wrapper live in this directory; running the
script produces ready-to-use SDKs under `sdks/<lang>/generated/`.

**Publish workflow deferred.** The release tag → npm / PyPI / pkg.go.dev
auto-publish workflow needs registry credentials (NPM_TOKEN, PYPI_TOKEN,
plus the Go SDK's own module repo) that aren't yet wired into the
release pipeline. Tracked under [#1262](https://github.com/kwad77/pincher/issues/1262).
Until then, generate locally and either vendor the output or publish
under your own namespace.

## Generating locally

```bash
# Prereqs: pincher running on localhost (any of stdio / --http :8080),
# plus openapi-generator-cli on PATH.
brew install openapi-generator           # macOS
# or: npm install -g @openapitools/openapi-generator-cli
# or: docker pull openapitools/openapi-generator-cli (script auto-detects)

# Generate all three SDKs into sdks/{typescript,python,go}/generated/
./scripts/generate-sdks.sh

# Generate just one:
./scripts/generate-sdks.sh typescript
```

The script grabs `/v1/openapi.json` from a running pincher (defaults
to `http://localhost:8080`; override via `PINCHER_HTTP_URL`) and feeds
it to openapi-generator with the per-language config under
`sdks/<lang>/codegen.yaml`. Generated output is gitignored — these
SDKs are *thin clients*, regenerated whenever the spec changes.

## Per-language usage

Each language directory has an `examples/` folder with a "hello world"
that calls `pincher search` and prints the first result. See:

- [`sdks/typescript/examples/search.ts`](typescript/examples/search.ts)
- [`sdks/python/examples/search.py`](python/examples/search.py)
- [`sdks/go/examples/search.go`](go/examples/search.go)

## Spec contract

The `/v1/openapi.json` endpoint emits OpenAPI 3.1.0 and is gated by
`internal/server/openapi_spec_validity_test.go` — every PR runs the
validity check so the spec stays codegen-clean.

For the inverse contract (every advertised MCP tool also appears in
the OpenAPI spec, and vice versa), see
`internal/server/openapi_parity_test.go`. Together those two gates
ensure regenerated SDKs cover every tool the binary exposes.
