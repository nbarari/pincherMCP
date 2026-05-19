# Pincher + Claude Code: what changes

Claude Code without pincher navigates code with `Read`, `Grep`, and `Glob` — it reads whole files into the context window to find one function, greps unranked text, and re-reads the same file every time it needs a different part. With pincher wired up, the same questions are answered by byte-offset symbol fetches and a BM25 index, and the PreToolUse hook nudges the agent there automatically.

This page is the *why*. For setup, see the [Claude Code tutorial](../../tutorials/claude-code.md).

## How Claude Code reaches pincher

Two surfaces, both installed by `pincher init --target=claude`:

1. **The MCP tool surface** — `search`, `symbol`, `context`, `trace`, `architecture`, `changes`, and the rest, callable directly.
2. **The PreToolUse hook** — `pincher hook-check` runs before every `Read` and `Grep`. Since v0.86 it is *advisory*: it never blocks the call, it adds a `systemMessage` nudge when an indexed file would be cheaper to reach via `context`. The agent keeps working; over a session it learns to reach for pincher first.

## What you get

**Read → context.** A 40 KB Go file read whole is ~10,000 tokens of context. The same file's main symbol via `context id=… lite=true` is the function plus its imports — a fraction of that, and it's the part the agent actually needed. The hook surfaces this on every large-indexed-file `Read`.

**Grep → search.** `Grep "processOrder"` returns every unranked textual hit — comments, strings, the definition, every call site, in file order. `search query="processOrder"` returns BM25-ranked symbols: the definition first, with a signature and a query-aware snippet. Often the one call answers the question with no follow-up.

**A new question pincher answers that grep can't:** *what calls this?* `trace name=processOrder direction=inbound` walks the call graph with risk labels — the blast radius of a change, in one call, instead of grepping the function name and manually filtering definitions from call sites.

## The numbers

Measured on pincher's own codebase (221 files, 3,769 symbols, 5,920 edges):

- Cold index: ~900 ms. Re-index after an edit (incremental, content-hash skip): typically <50 ms.
- `search` (FTS5 BM25): ~1 ms. Single-hop `query`: ~2 ms. `trace` depth-3 BFS: <5 ms.
- `symbol` fetch is one SQL row + one `os.File.Seek` + one `Read` — no re-parsing, ever.

Every tool response carries a real `_meta.tokens_used` count, so the savings are visible per call, not asserted.

## Verified

The `initialize → tools/list → search → architecture` workflow is pinned in pincher's host-conformance corpus (`testdata/host-conformance/claude-code/`) — a release that broke Claude Code's canonical flow fails CI.

## Setup

[Claude Code tutorial](../../tutorials/claude-code.md) — about 10 minutes, from `go install` to the agent reaching for pincher tools.
