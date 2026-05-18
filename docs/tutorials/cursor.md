# Tutorial: pincher with Cursor

About 10 minutes. By the end you'll have a local pincher install, a Cursor rules file telling Cursor's agent to prefer pincher tools, and verified the agent reaches for pincher in chat.

This walkthrough assumes nothing about pincher's internals. For the long-form manual, see [`docs/REFERENCE.md`](../REFERENCE.md).

## What you need

- **Go 1.25+** on your `PATH` (or a [release binary](https://github.com/kwad77/pincher/releases/latest))
- **Cursor** installed
- A Git repository to point pincher at

## 1. Install pincher

```bash
go install github.com/kwad77/pincher/cmd/pinch@latest
pincher --version
# pincherMCP v0.79.0
```

## 2. Index your project

```bash
cd ~/code/your-project
pincher index
# indexed 42 files, 1238 symbols, 6711 edges in 187ms
```

## 3. Seed Cursor's rules file

Cursor reads project-scoped rules from `.cursor/rules/*.mdc` files (modern format) or `.cursorrules` (legacy). Pick the one your Cursor version uses:

```bash
# Modern Cursor: .cursor/rules/pincher.mdc with YAML frontmatter
pincher init --target=cursor
# pincher init [cursor]: wrote /home/you/code/your-project/.cursor/rules/pincher.mdc

# Legacy Cursor: ./.cursorrules plain text
pincher init --target=cursor-legacy
```

The modern target produces a file like:

```mdc
---
description: pincher MCP code-intelligence usage policy
globs:
  - "**/*"
alwaysApply: true
---

<!-- pincher:start -->
<!-- Managed by `pincher init`. Edit `pincher init` to change this block,
     or delete the markers below to opt out of future updates. -->

## Pincher Usage Policy
…
<!-- pincher:end -->
```

The frontmatter is yours to customise — change `globs` to scope the rule to a subset of the tree, set `alwaysApply: false` to make it on-demand only, etc. Re-running `pincher init --target=cursor` preserves your edits to the frontmatter and only touches the body inside the markers.

If you're not sure which Cursor format your install expects, run pincher's auto-detect — it writes only to targets whose marker file already exists:

```bash
pincher init --target=detect
```

## 4. Wire pincher into Cursor as an MCP server

Edit `~/.cursor/mcp.json` (create if missing):

```json
{
  "mcpServers": {
    "pincher": { "command": "pincher" }
  }
}
```

If `pincher` isn't on Cursor's `PATH`, use the absolute path from `which pincher` / `where.exe pincher`.

Restart Cursor so it re-reads the config.

## 5. Try it out

Open the chat panel and ask something graph-shaped:

> *"What calls `Open()` in `internal/db/db.go`? Show me the call sites."*

Cursor's agent now reaches for `mcp__pincher__trace` (BFS over the call graph with risk labels) instead of `Grep`. You'll see results land in a couple of milliseconds and the agent's context window stays small — every pincher response carries a `_meta` envelope with the token-savings receipt.

## 6. Watch the savings

```bash
pincher stats
```

```
┌────────────────────────────────────────────┐
│                  ALL-TIME                  │
│  Tool calls:         54                    │
│  Tokens used:      3,840                   │
│  Tokens saved:   217,600                   │
└────────────────────────────────────────────┘
```

## What to read next

- **[REFERENCE.md → MCP tools](../REFERENCE.md#the-24-mcp-tools)** — full tool catalogue
- **[REFERENCE.md → `pincher init`](../REFERENCE.md#pincher-init)** — every supported target (Claude Code, Cursor, Windsurf, Aider, Continue, …) and how the marker-block contract works
- **[Tutorial: Claude Code](claude-code.md)** — same flow with `~/.claude/CLAUDE.md`
- **[Tutorial: HTTP dashboard](http-dashboard.md)** — point any browser-friendly client at pincher's REST API
