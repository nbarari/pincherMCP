# Tutorial: pincher with JetBrains AI Assistant (Junie)

About 10 minutes. Wires pincher into JetBrains IDEs (IntelliJ IDEA,
PyCharm, GoLand, WebStorm, RubyMine, etc.) so the AI Assistant /
Junie agent reaches for pincher tools when navigating code.

JetBrains AI Assistant reads project-level rules from
`.idea/.junie/guidelines.md`. `pincher init --target=jetbrains` writes
the pincher policy block to that file.

For the long-form manual see [`docs/REFERENCE.md`](../REFERENCE.md).

## What you need

- **A JetBrains IDE** with **AI Assistant** enabled (Settings →
  Tools → AI Assistant). The assistant must be at a version that
  reads `.junie/guidelines.md` (2024.3+).
- **Go 1.25+** on your `PATH` (or a release binary from
  [releases](https://github.com/kwad77/pincher/releases/latest))
- A Git repository opened in the IDE

## 1. Install pincher

```bash
go install github.com/kwad77/pincher/cmd/pinch@latest
pincher --version
# pincherMCP v0.78.0
```

## 2. Index your project

From the IDE's terminal (View → Tool Windows → Terminal), or any
shell at the project root:

```bash
pincher index
# indexed 42 files, 1238 symbols, 6711 edges in 187ms
```

## 3. Wire pincher's policy into the IDE

```bash
pincher init --target=jetbrains
# pincher init [jetbrains]: wrote /home/you/code/your-project/.idea/.junie/guidelines.md
```

The block lands inside `<!-- pincher:start --> ... <!-- pincher:end -->`
markers — re-runs update the block in place. Add your own project
rules above or below the markers; pincher leaves them untouched.

The policy tells the AI Assistant:
- Prefer pincher tools for code navigation
- Workflow shape: `search` → `context`/`symbol` → `trace` if behavior
  change → edit → `changes` before declaring done
- When to fall back to file reads (binary blobs, files being
  authored, exact-byte audits)

## 4. Configure JetBrains MCP server access

JetBrains AI Assistant currently surfaces MCP servers through Junie's
agent mode (built into 2024.3+). To register pincher:

1. Settings → Tools → AI Assistant → MCP Servers (or Junie → Servers
   depending on the AI Assistant version).
2. Add a new stdio server:
   - **Name:** `pincher`
   - **Command:** `pincher`
   - **Args:** (none)
3. Save and reload the assistant.

If `pincher` isn't on the IDE's `PATH` (JetBrains processes don't
inherit shell aliases), set the absolute path from `which pincher`.

## 5. Try it out

In the AI Assistant chat panel:

> *"Find every function that calls `processPayment` and show me what it does."*

The assistant calls `mcp__pincher__search` and `mcp__pincher__symbol`
instead of grepping or reading the project tree directly. Token
savings show in the `_meta` envelope on every pincher response.

## 6. Watch the savings add up

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

## Notes for JetBrains users specifically

- **`.idea/` is per-project.** `pincher init --target=jetbrains`
  writes to the current project. To enable pincher across every
  JetBrains project, drop the same policy block into the IDE's
  global instructions (Settings → Tools → AI Assistant → Custom
  Instructions).
- **`.idea/` is sometimes in `.gitignore`.** If your team
  `.gitignore`s the whole `.idea/` directory, the `.junie/guidelines.md`
  file won't be shared with teammates. Add an exception:
  `!.idea/.junie/` (and `!.idea/.junie/**`) in `.gitignore`.
- **`notifications/tools/list_changed` support varies by AI Assistant
  version.** Most recent JetBrains releases honor it; older versions
  require an IDE restart to surface tools added by a mid-session
  binary swap.
- **AI Assistant vs Junie.** Earlier JetBrains AI Assistant builds
  used `.idea/.aiagent/*` paths; v2024.3+ unified under `.junie/`.
  `pincher init --target=jetbrains` writes the current convention.

## What to read next

- [REFERENCE.md → MCP tools](../REFERENCE.md#the-23-mcp-tools) — every tool, every parameter
- [Tutorial: Claude Code](claude-code.md) — same flow for the Claude Code CLI
- [Tutorial: VS Code Copilot](vscode-copilot.md) — same flow for VS Code Copilot
- [`docs/integrations/loop-leverage-layers.md`](../integrations/loop-leverage-layers.md) — the three-layer agent-leverage frame

---

_Last reviewed: v0.75 (#1334)._
