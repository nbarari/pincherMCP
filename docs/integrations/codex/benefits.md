# Pincher + Codex: what changes

Codex reaches pincher over the MCP surface. The benefits mirror the other hosts — see [Claude Code benefits](../claude-code/benefits.md) for the full treatment. The Codex-specific notes:

- **Streamable-HTTP transport.** Codex can connect to pincher over the MCP Streamable-HTTP transport (`--mcp-http-path`), not just stdio — useful when pincher runs as a shared service. See [`docs/streamable-http.md`](../../streamable-http.md).
- **Ranked search over grep.** `search` returns BM25-ranked symbols with signatures; the agent skips the open-three-files-to-disambiguate step.
- **Read → context.** `context id=… lite=true` returns a symbol plus its imports — the part the agent needed, not the whole file.
- **Call graph in one call.** `trace` gives callers/callees with risk labels; `changes` maps a working-tree diff to affected symbols.
- **Real token accounting.** Every response carries `_meta.tokens_used` — savings are measured, not asserted.

## The numbers

Measured on pincher's own codebase (221 files, 3,769 symbols, 5,920 edges): cold index ~900 ms, incremental re-index <50 ms, `search` ~1 ms, `trace` depth-3 <5 ms.

## Verified

The canonical workflow is pinned in pincher's host-conformance corpus (`testdata/host-conformance/codex/`).

## Setup

[Codex tutorial](../../tutorials/codex.md).
