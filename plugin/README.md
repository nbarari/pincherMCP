# pincher (Claude Code plugin)

Wraps the [pincherMCP](https://github.com/kwad77/pincherMCP) codebase-intelligence server as a Claude Code plugin. Installs the matching binary locally on first launch and wires it up as an MCP stdio server. No separate install step, no PATH editing.

## What gets added to your session

- 16 MCP tools for codebase search, graph queries, and token-efficient symbol retrieval. See the main [REFERENCE](https://github.com/kwad77/pincherMCP/blob/master/docs/REFERENCE.md#the-16-mcp-tools) for the full list.
- A SessionStart hook that runs `pincher index --hook` after install, injecting a "pincher is ready" additionalContext envelope so agents are primed to use pincher tools instead of defaulting to Read/Grep ([#138](https://github.com/kwad77/pincherMCP/issues/138)).
- A `_meta` envelope on every tool response with real BPE token counts, latency, and cost avoided.
- Persistent per-project session stats and all-time savings totals in SQLite.

## Install

```
/plugin marketplace add kwad77/pincherMCP
/plugin install pincher@pincherMCP
```

Start a new Claude Code session after the install so the SessionStart hook runs and downloads the binary (one-time per version, ~8 MB).

## Upgrade

When a new plugin version ships, `/plugin update pincher` bumps `plugin.json`. On the next SessionStart the install script sees the version mismatch and fetches the new binary automatically. No manual binary swap.

Versions are **pinned** — the binary is always the exact release matching `plugin.json.version`, never a `latest` tag. Stability over convenience.

## What the install script does

Short audit notes for anyone reviewing the plugin:

- Reads the version from `.claude-plugin/plugin.json`.
- Fast-exits if `bin/pincher` is already the right version, or if `pincher` is on PATH at the right version (in which case it's symlinked/copied rather than re-downloaded).
- Detects OS and arch, constructs a download URL against `github.com/kwad77/pincherMCP/releases`.
- Fetches the archive + `SHA256SUMS` from the release.
- Verifies the archive against the expected checksum. Refuses to install on mismatch.
- Extracts to a temp directory, moves the binary into `${CLAUDE_PLUGIN_ROOT}/bin/`, sets the executable bit on POSIX.

Network access is required on the first run per version. Subsequent sessions with no version change do no network I/O.

## Files

| Path | Purpose |
|---|---|
| `.claude-plugin/plugin.json` | Plugin metadata — name, version, author, license |
| `.mcp.json` | MCP server registration. Points at the locally-installed binary |
| `hooks/hooks.json` | SessionStart hook that runs the install script |
| `scripts/install.js` | Cross-platform dispatcher (uses Node, which Claude Code already ships with) |
| `scripts/install.sh` | POSIX installer (macOS, Linux) — downloads + verifies + extracts |
| `scripts/install.ps1` | Windows installer — same flow, PowerShell idioms |

## Uninstall

```
/plugin uninstall pincher
```

This removes the plugin and its `bin/` directory. The pincher database at your platform data dir (`~/.local/share/pincherMCP/pincher.db`, `~/Library/Application Support/pincherMCP/pincher.db`, or `%APPDATA%\pincherMCP\pincher.db`) is **not** touched — your index and session stats survive reinstalls.

## Privacy

The plugin downloads the pincher binary from `github.com/kwad77/pincherMCP/releases` on first use. Nothing else leaves your machine — pincher itself is entirely local and has no telemetry, no auto-update check, no cloud component. See the [main repo](https://github.com/kwad77/pincherMCP) for the source of everything that runs.
