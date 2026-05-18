# Tutorial: pincher with Codex CLI

About 10 minutes. Wires pincher into the OpenAI Codex CLI as an MCP
server. By the end the Codex CLI reaches for pincher tools instead of
its built-in file-reading sub-tools when navigating code.

For the long-form manual see [`docs/REFERENCE.md`](../REFERENCE.md).

## What you need

- **Codex CLI** installed (`pip install openai-codex` or follow the
  [Codex install docs](https://github.com/openai/codex))
- **Go 1.25+** on your `PATH` (or a release binary from
  [releases](https://github.com/kwad77/pincher/releases/latest))
- A Git repository to point pincher at

## 1. Install pincher

```bash
go install github.com/kwad77/pincher/cmd/pinch@latest
pincher --version
# pincherMCP v0.78.0
```

## 2. Index your project

```bash
cd ~/code/your-project
pincher index
# indexed 42 files, 1238 symbols, 6711 edges in 187ms
```

## 3. Wire pincher into Codex

`pincher init --target=codex` writes a `[mcp.pincher]` block to
`~/.codex/config.toml`:

```bash
pincher init --target=codex
# pincher init [codex]: wrote /home/you/.codex/config.toml
```

The block lands at the user-config scope (not project) since Codex
reads MCP servers from `~/.codex/config.toml`:

```toml
[mcp.pincher]
command = "pincher"
args = []
```

If `pincher` isn't on Codex's `PATH`, set the absolute path from
`which pincher`.

Restart Codex so it re-reads the config.

## 4. Try it out

```bash
codex "Find every function that calls processPayment and show me what it does."
```

Codex calls `mcp__pincher__search` to locate seeds, then
`mcp__pincher__symbol` and `mcp__pincher__trace` to assemble the
answer. Each response carries a `_meta` envelope:

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

## Notes for Codex users specifically

- **`notifications/tools/list_changed` IS honored.** Mid-session
  pincher binary swaps surface new tools live in Codex without a
  CLI restart.
- **Roots auto-discovery.** Codex advertises the `roots` capability
  in its MCP `initialize` handshake. Pincher's session-root detection
  picks the first advertised root automatically (see
  `internal/server/server.go::detectRoot`). Multi-root indexing
  rolls forward in v0.76 (#1081).
- **Stdio vs HTTP.** Codex prefers the stdio MCP transport for local
  development. To run pincher as a network-reachable HTTP-MCP server
  for Codex, set `PINCHER_MCP_HTTP_PATH=/mcp` and use the
  `streamable_http` transport — see [`docs/streamable-http.md`](../streamable-http.md).

## What to read next

- [REFERENCE.md → MCP tools](../REFERENCE.md#the-23-mcp-tools) — every tool, every parameter
- [Tutorial: Claude Code](claude-code.md) — same flow with CLAUDE.md rules
- [Tutorial: Cursor](cursor.md) — same flow with rules-file format
- [`docs/integrations/loop-leverage-layers.md`](../integrations/loop-leverage-layers.md) — the three-layer agent-leverage frame

---

_Last reviewed: v0.75 (#1334)._
