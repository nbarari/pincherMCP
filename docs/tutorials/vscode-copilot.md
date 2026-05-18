# Tutorial: pincher with VS Code Copilot Chat

About 10 minutes. By the end you'll have a local pincher install, GitHub Copilot's instructions file telling Copilot Chat to prefer pincher tools, pincher registered as an MCP server in VS Code, and verified Copilot Chat reaches for pincher in a real chat.

This walkthrough assumes nothing about pincher's internals. For the long-form manual, see [`docs/REFERENCE.md`](../REFERENCE.md).

## What you need

- **Go 1.25+** on your `PATH` (or a [release binary](https://github.com/kwad77/pincher/releases/latest))
- **VS Code** with the **GitHub Copilot** and **GitHub Copilot Chat** extensions
- A Git repository to point pincher at

VS Code 1.95+ ships the MCP integration for Copilot Chat under `.vscode/mcp.json`. Older versions can still use the rules file from step 3, just without the MCP registration in step 4.

## 1. Install pincher

```bash
go install github.com/kwad77/pincher/cmd/pinch@latest
pincher --version
```

## 2. Index your project

```bash
cd ~/code/your-project
pincher index
# indexed 42 files, 1238 symbols, 6711 edges in 187ms
```

## 3. Seed Copilot's instructions file

GitHub Copilot reads `.github/copilot-instructions.md` at the repo root and folds it into every chat/completion request as system context. The convention is shared across VS Code Copilot Chat, Codespaces, JetBrains Copilot, and github.com — one file, one writer.

```bash
pincher init --target=vscode
# pincher init [vscode]: wrote /home/you/code/your-project/.github/copilot-instructions.md
```

Re-running the command is idempotent — pincher only touches the body inside its `<!-- pincher:start -->` / `<!-- pincher:end -->` markers, so your other instructions stay untouched.

## 4. Register pincher as an MCP server

VS Code Copilot Chat picks up MCP servers from `./.vscode/mcp.json` at the workspace root.

```bash
pincher init --target=vscode-mcp
# pincher init [vscode-mcp]: wrote /home/you/code/your-project/.vscode/mcp.json
```

The generated file looks like:

```json
{
  "servers": {
    "pincher": {
      "type": "stdio",
      "command": "/abs/path/to/pincher",
      "args": ["supervised"],
      "env": {
        "PINCHER_DATA_DIR": "/home/you/.local/share/pincher-vscode"
      }
    }
  }
}
```

The per-target `PINCHER_DATA_DIR` keeps VS Code's pincher session isolated from Claude Code's or Cursor's — so a `pincher stats` run inside one editor doesn't conflate token-savings counters with another.

If you already have other MCP servers configured in `.vscode/mcp.json`, pincher merges into the existing `servers` map and leaves the rest alone. If the file is malformed JSON, pincher refuses to write rather than nuke your config.

Reload the VS Code window (Cmd/Ctrl+Shift+P → **Developer: Reload Window**) so Copilot Chat picks up the new server.

## 5. Try it out

Open Copilot Chat (Cmd/Ctrl+Alt+I) and ask something graph-shaped:

> *"What calls `Open()` in `internal/db/db.go`? Show me the call sites."*

Copilot Chat reaches for `mcp__pincher__trace` (BFS over the call graph with risk labels) instead of grepping the file tree. You'll see results land in a couple of milliseconds, and the agent's context window stays small — every pincher response carries a `_meta` envelope with the token-savings receipt.

If Copilot doesn't pick pincher up on its own, ask explicitly: *"Use the pincher MCP server to trace callers of Open."*

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

- **[REFERENCE.md → MCP tools](../REFERENCE.md#the-23-mcp-tools)** — full tool catalogue
- **[REFERENCE.md → `pincher init`](../REFERENCE.md#pincher-init)** — every supported target and how the marker-block contract works
- **[Tutorial: Claude Code](claude-code.md)** — same flow with `~/.claude/CLAUDE.md`
- **[Tutorial: Cursor](cursor.md)** — same flow with `.cursor/rules/*.mdc`
