# Pincher + VSCode Copilot: what changes

VSCode Copilot reaches pincher over the MCP surface. The benefits mirror the other hosts — see [Claude Code benefits](../claude-code/benefits.md) for the full treatment. The VSCode-Copilot-specific notes:

- **MCP server registration.** `pincher init --target=vscode-copilot` registers pincher in VSCode's MCP config; Copilot's agent mode then has the full tool surface.
- **Ranked search over grep.** `search` returns BM25-ranked symbols with signatures — the definition first, not an unranked text scan.
- **Read → context.** `context id=… lite=true` returns a symbol plus its imports; the agent skips reading whole files to find one function.
- **Call graph in one call.** `trace` gives callers/callees with risk labels; `architecture` orients on an unfamiliar area in a single call.
- **Real token accounting.** Every response carries `_meta.tokens_used` — measured, not asserted.

## The numbers

Measured on pincher's own codebase (221 files, 3,769 symbols, 5,920 edges): cold index ~900 ms, incremental re-index <50 ms, `search` ~1 ms, `trace` depth-3 <5 ms.

## Verified

The canonical workflow is pinned in pincher's host-conformance corpus (`testdata/host-conformance/vscode-copilot/`).

## Setup

[VSCode Copilot tutorial](../../tutorials/vscode-copilot.md).
