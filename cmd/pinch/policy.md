## Pincher Usage Policy

This project uses [pincherMCP](https://github.com/kwad77/pincherMCP) for code intelligence. AI agents should prefer pincher tools over `Read`/`Grep`/`Glob` for any code-navigation task. Use the workflow below; fall back to direct file ops only when pincher returns no useful result or you specifically need to read a non-code file.

| Goal | Pincher tool | Notes |
|---|---|---|
| **Don't know which tool to call?** | `guide` | Pass a free-form task ("fix login retry bug"); returns 2-3 recommended next calls. |
| Orient in an unfamiliar area | `architecture` | Once at the start of unfamiliar work; entry points + hotspots + language breakdown. |
| Find code by symbol or keyword | `search` | Use **before** `Grep`. Supports kind/language/corpus filters and BM25 ranking. |
| Read one symbol's source | `symbol` | O(1) byte-offset seek, never re-parses. Use when you have the ID from `search`. |
| Read several symbols at once | `symbols` (batch) | Use **instead of** repeated `symbol` calls. One round-trip. |
| Inspect a function before editing | `context` | Returns the symbol plus its direct imports — minimal token cost. |
| See what calls a function | `trace` | BFS with risk labels. Use **before** grepping for callers. |
| Pre-edit / pre-commit safety check | `changes` | Maps git diff to symbols + blast-radius. Run **before** finalizing edits. |
| Persistent project memory | `adr` | Store decisions, conventions, gotchas; survives sessions. |
| Diagnose extraction quality | `health` | Per-language coverage, parser identity, slow queries. |

### Workflow shape

For new work:
1. `architecture` to orient.
2. `search` for the relevant symbol.
3. `context` (or `symbol`) to read it.
4. `trace` if behaviour change is risky.
5. Edit + verify.
6. `changes` before declaring done.

For bug-fix work, skip `architecture` if the area is familiar; otherwise start the same way.

### When to fall back to Read/Grep

- Pincher returns no result for the query (rare for code; common for raw text in non-code files).
- You need exact-byte file inspection (e.g., reviewing whitespace).
- The file isn't indexable (binary blobs, large lockfiles — pincher deliberately skips these).
- You're authoring a new file that doesn't exist yet.

If you're about to read a file end-to-end with `Read`, ask yourself: would `context` (symbol + its imports) answer my question with fewer tokens? Usually yes.
