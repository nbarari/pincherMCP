# Tutorial: pincher with Claude Code

About 10 minutes. By the end you'll have a local pincher install indexing one of your repositories and Claude Code reaching for pincher tools instead of `Read` / `Grep` / `Glob` for code navigation.

This walkthrough assumes nothing about pincher's internals. For the long-form manual, see [`docs/REFERENCE.md`](../REFERENCE.md).

## What you need

- **Go 1.25+** on your `PATH` (or a release binary downloaded from [releases](https://github.com/kwad77/pincher/releases/latest))
- **Claude Code** installed (`claude.ai/code`)
- A Git repository to point pincher at — anything from a 100-file side project to a multi-thousand-file monorepo works

## 1. Install pincher

```bash
go install github.com/kwad77/pincher/cmd/pinch@latest
# or:
#   wget https://github.com/kwad77/pincher/releases/latest/download/pincher-linux-amd64
#   chmod +x pincher-linux-amd64 && mv pincher-linux-amd64 ~/.local/bin/pincher
```

Confirm it's on your `PATH`:

```bash
pincher --version
# pincherMCP v0.78.1
```

## 2. Index your project

From inside the repository:

```bash
cd ~/code/your-project
pincher index
```

You'll see something like:

```
indexed 42 files, 1238 symbols, 6711 edges in 187ms
```

The index lives in pincher's data directory (`~/.local/share/pincher/pincher.db` on Linux, `~/Library/Application Support/pincher/` on macOS, `%LOCALAPPDATA%\pincher\` on Windows). Re-running `pincher index` is incremental — only changed files are re-parsed.

## 3. Drop the policy block into `CLAUDE.md`

`pincher init` writes a policy block into your project's `CLAUDE.md` that tells Claude Code to prefer pincher tools over `Read` / `Grep` / `Glob`:

```bash
pincher init
# pincher init [claude]: wrote /home/you/code/your-project/CLAUDE.md
#
# Next steps:
#   1. Run `pincher index` from this directory to build the symbol graph.
#   2. Connect your MCP client (Claude Code, Cursor, etc.) to `pincher`.
#   3. Or open the dashboard: `pincher web`
```

The block is wrapped in `<!-- pincher:start --> ... <!-- pincher:end -->` markers so re-running `pincher init` updates the block in place rather than duplicating it. Feel free to add your own project guidance above or below the markers — pincher leaves it untouched.

If you want pincher's policy applied to *every* project Claude Code touches, write it to your global config instead:

```bash
pincher init --global    # ~/.claude/CLAUDE.md
```

## 4. Wire pincher into Claude Code

Edit `~/.claude/mcp.json` (create if missing):

```json
{
  "mcpServers": {
    "pincher": { "type": "stdio", "command": "pincher" }
  }
}
```

If `pincher` isn't on Claude Code's `PATH` (Claude Code may not see your shell aliases), use the absolute path you got from `which pincher` or `where.exe pincher`.

Restart Claude Code so it re-reads the MCP config.

## 5. Try it out

In Claude Code, ask something that exercises pincher:

> *"Find every function that calls `processPayment` and show me what it does."*

Behind the scenes Claude Code now calls `mcp__pincher__search` (BM25 over the FTS5 index) instead of `Grep`-ing the whole tree. With a result in hand it follows up with `mcp__pincher__symbol` for the O(1) byte-offset fetch. You'll see the chat is faster and uses dramatically fewer tokens — every pincher response carries a `_meta` envelope:

```json
"_meta": {
  "tokens_used":  312,
  "tokens_saved": 14500,
  "latency_ms":   2
}
```

## 6. Watch the savings add up

```bash
pincher stats
```

Output:

```
┌────────────────────────────────────────────┐
│                  ALL-TIME                  │
│  Tool calls:         128                   │
│  Tokens used:      9,120                   │
│  Tokens saved:   612,400                   │
└────────────────────────────────────────────┘

PROJECT                          FILES       SYMBOLS     EDGES       PATH
your-project                     42          1238        6711        /home/you/code/your-project
```

Savings persist in SQLite across restarts, reconnects, and binary upgrades.

## What to read next

- **[REFERENCE.md → MCP tools](../REFERENCE.md#the-23-mcp-tools)** — every tool, every parameter, every `_meta` field
- **[REFERENCE.md → CLI subcommands](../REFERENCE.md#cli-subcommands)** — `pincher doctor`, `pincher self-test`, `pincher rebuild-fts`, etc.
- **[Tutorial: Cursor](cursor.md)** — same flow with Cursor's rules-file format
- **[Tutorial: HTTP dashboard](http-dashboard.md)** — open the live dashboard for any browser-friendly client
- **[Project roadmap](https://github.com/kwad77/pincher/issues/193)** — what's coming in v0.6.0 → v1.0
