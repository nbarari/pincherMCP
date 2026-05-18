# Tutorial: pincher with Zed

About 10 minutes. Wires pincher into Zed as an MCP server and writes
a Zed-friendly rules file so the Zed assistant reaches for pincher
tools instead of raw file reads.

For the long-form manual see [`docs/REFERENCE.md`](../REFERENCE.md).

## What you need

- **Zed** installed ([zed.dev](https://zed.dev))
- **Go 1.25+** on your `PATH` (or a release binary from
  [releases](https://github.com/kwad77/pincher/releases/latest))
- A Git repository to point pincher at

## 1. Install pincher

```bash
go install github.com/kwad77/pincher/cmd/pinch@latest
pincher --version
# pincherMCP v0.75.0
```

## 2. Index your project

```bash
cd ~/code/your-project
pincher index
# indexed 42 files, 1238 symbols, 6711 edges in 187ms
```

Incremental on every subsequent run — only changed files are re-parsed.

## 3. Wire pincher into Zed

`pincher init --target=zed` writes the MCP wiring into
`.zed/settings.json` at the project root:

```bash
pincher init --target=zed
# pincher init [zed]: wrote /home/you/code/your-project/.zed/settings.json
```

The block lands under `"context_servers"` (Zed's MCP server slot):

```json
{
  "context_servers": {
    "pincher": {
      "command": {
        "path": "pincher",
        "args": []
      }
    }
  }
}
```

If `pincher` isn't on Zed's `PATH`, replace `"pincher"` with the
absolute path from `which pincher`.

Restart Zed so it re-reads the settings.

## 4. Try it out

In Zed's assistant panel, ask:

> *"Find every function that calls `processPayment` and show me what it does."*

Behind the scenes Zed now calls `mcp__pincher__search` (BM25 over the
FTS5 index) and `mcp__pincher__symbol` (O(1) byte-offset fetch)
instead of the assistant's built-in file-reading tools.

Each response carries a `_meta` envelope:

```json
"_meta": {
  "tokens_used":  312,
  "tokens_saved": 14500,
  "latency_ms":   2
}
```

## 5. Watch the savings add up

```bash
pincher stats
```

```
┌────────────────────────────────────────────┐
│                  ALL-TIME                  │
│  Tool calls:         128                   │
│  Tokens used:      9,120                   │
│  Tokens saved:   612,400                   │
└────────────────────────────────────────────┘
```

Persistent across restarts, reconnects, and binary upgrades.

## Notes for Zed users specifically

- **`notifications/tools/list_changed` IS honored.** When pincher
  auto-restarts onto a fresh binary mid-session (drift detection,
  #1374), Zed picks up newly-added tools live — no Zed restart
  needed. Claude Code requires a fresh session for new-tool discovery.
- **Slash commands.** Zed's slash-command surface for pincher
  composites is tracked in #1336 for v0.77.
- **Per-project vs global settings.** `pincher init --target=zed`
  writes to the project's `.zed/settings.json`. To enable pincher for
  every project Zed opens, set the same `context_servers.pincher`
  block in `~/.config/zed/settings.json` instead.

## What to read next

- [REFERENCE.md → MCP tools](../REFERENCE.md#the-23-mcp-tools) — every tool, every parameter
- [Tutorial: Claude Code](claude-code.md) — same flow with stdio-mcp
- [Tutorial: HTTP dashboard](http-dashboard.md) — live dashboard view
- [`docs/integrations/loop-leverage-layers.md`](../integrations/loop-leverage-layers.md) — the three-layer agent-leverage frame

---

_Last reviewed: v0.75 (#1334)._
