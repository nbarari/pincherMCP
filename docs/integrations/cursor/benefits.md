# Pincher + Cursor: what changes

Cursor's agent is strong at editing but spends turns *locating* — opening files to find a symbol, asking you to clarify which `Handler` you meant, re-reading context it already saw. Pincher gives Cursor a ranked symbol index and a knowledge graph, and a rules file that steers the agent to them.

This page is the *why*. For setup, see the [Cursor tutorial](../../tutorials/cursor.md).

## How Cursor reaches pincher

`pincher init --target=cursor` writes `.cursor/rules/pincher.mdc` — a Cursor rules file with YAML frontmatter plus a Read/Grep "hook-check equivalent" preamble (the rules-file analogue of Claude Code's PreToolUse hook, since Cursor has no runtime hook). The MCP tools register through Cursor's MCP config. The rules file tells the agent: before `Read` an indexed file, reach for `context`; before grepping an identifier, reach for `search`.

## What you get

**The agent stops asking you to clarify.** "Which `Open` did you mean?" usually means the agent grepped, got 12 hits, and can't disambiguate. `search query="Open" kind=Function` returns them BM25-ranked with signatures and file paths — the agent picks the right one itself and keeps moving.

**Orientation before editing.** Dropped into an unfamiliar area, `architecture` returns entry points, hotspots, and the language breakdown in one call — cheaper than the agent reading a dozen files to build the same mental model.

**Change-impact before the edit lands.** `changes` maps the working-tree diff to affected symbols and blast radius; `trace` walks callers. Cursor can check what an edit touches before it makes it, not after the test suite tells it.

## The numbers

Measured on pincher's own codebase (221 files, 3,769 symbols, 5,920 edges):

- Cold index: ~900 ms; incremental re-index after an edit: typically <50 ms — the watcher keeps the index fresh as you work.
- `search`: ~1 ms. `query` single-hop: ~2 ms. `trace` depth-3: <5 ms.
- Every response carries a real `_meta.tokens_used` BPE count.

## Verified

The `initialize → tools/list → search → architecture` workflow is pinned in pincher's host-conformance corpus (`testdata/host-conformance/cursor/`) so a release can't silently break Cursor's canonical flow.

## Setup

[Cursor tutorial](../../tutorials/cursor.md).
