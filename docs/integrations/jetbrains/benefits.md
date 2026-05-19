# Pincher + JetBrains AI Assistant: what changes

JetBrains AI Assistant (and Junie) already have the IDE's own index. Pincher adds a *cross-language, agent-shaped* graph: ranked symbol search, a call graph, and change-impact — exposed as MCP tools and steered by a guidelines file.

This page is the *why*. For setup, see the [JetBrains tutorial](../../tutorials/jetbrains.md).

## How JetBrains reaches pincher

`pincher init --target=jetbrains` writes `.idea/.junie/guidelines.md` — the steering file Junie reads — and registers pincher's MCP server. The guidelines tell the assistant when to prefer pincher's tools over re-deriving the same information.

## What you get

**A graph that survives outside the IDE's model.** Pincher's symbol graph is a queryable SQLite store — `query` runs pinchQL (a Cypher-shaped subset) over it: "every Function with no inbound CALLS edge," "classes in this file," multi-hop call chains. That's an audit surface the assistant can drive directly.

**Change-impact pairs with Junie's project knowledge.** Junie knows the project; pincher's `changes` + `trace` tell it the blast radius of a specific diff — affected symbols, callers, recent-change overlap.

**Cross-language, one index.** One pincher index spans Go, Python, TS/JS, Java, and ~20 more — the assistant gets the same `search`/`trace`/`context` surface regardless of the file's language.

## The numbers

Measured on pincher's own codebase (221 files, 3,769 symbols, 5,920 edges):

- Cold index: ~900 ms; incremental re-index after an edit: typically <50 ms.
- `search`: ~1 ms. `query` single-hop: ~2 ms. `trace` depth-3: <5 ms.

## Verified

The canonical `initialize → tools/list → search → architecture` workflow is pinned in pincher's host-conformance corpus (`testdata/host-conformance/jetbrains/`).

## Setup

[JetBrains tutorial](../../tutorials/jetbrains.md).
