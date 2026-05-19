# Pincher + Zed: what changes

Zed's assistant is fast; pincher makes the *retrieval* it does just as fast. Instead of the assistant reading files to understand a module, it asks pincher for the module's shape directly.

This page is the *why*. For setup, see the [Zed tutorial](../../tutorials/zed.md).

## How Zed reaches pincher

`pincher init --target=zed` registers pincher as an MCP server in Zed's `context_servers` config. Zed's assistant then has the full pincher tool surface available in-editor.

## What you get

**Orient on a file in one shot.** `onboard_module path=…` returns a file's role in the architecture — its public surface, what it depends on, what depends on it — without the assistant reading the file plus its neighbours to reconstruct that.

**Ranked search instead of fuzzy file-finding.** `search` is BM25 over the symbol index: the definition ranks first, with a signature and snippet. The assistant doesn't open three files to find the one it wanted.

**The call graph as a first-class answer.** `trace` walks callers/callees with risk labels — "what breaks if I change this" is one call, not a grep-and-eyeball.

## The numbers

Measured on pincher's own codebase (221 files, 3,769 symbols, 5,920 edges):

- Cold index: ~900 ms; incremental re-index after an edit: typically <50 ms.
- `search`: ~1 ms. `query` single-hop: ~2 ms. `trace` depth-3: <5 ms.
- `symbol` fetch never re-parses — one SQL row + one byte-offset seek.

## Verified

The canonical `initialize → tools/list → search → architecture` workflow is pinned in pincher's host-conformance corpus (`testdata/host-conformance/zed/`).

## Setup

[Zed tutorial](../../tutorials/zed.md).
