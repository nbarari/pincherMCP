# Integrations

How pincher fits into the tools you already use, and the rationale behind its design.

## Per-host benefits

What pincher concretely changes for each supported MCP host — the *why*, paired with the [tutorials](../tutorials/) which are the *how*:

- [Claude Code](claude-code/benefits.md) — PreToolUse hook + the full tool surface
- [Cursor](cursor/benefits.md) — rules-file steering + ranked symbol search
- [Zed](zed/benefits.md) — in-editor MCP server + module orientation
- [JetBrains AI Assistant](jetbrains/benefits.md) — `.junie/guidelines.md` steering + the queryable graph
- [Codex](codex/benefits.md) — stdio or Streamable-HTTP transport
- [VSCode Copilot](vscode-copilot/benefits.md) — MCP registration for agent mode

Every host's canonical workflow is pinned in the [host-conformance corpus](../../testdata/host-conformance/) — a release that breaks a host's `initialize → tools/list → search → architecture` flow fails CI.

## Design rationale

- [loop-leverage-layers.md](loop-leverage-layers.md) — the three-layer agent-leverage frame.
- [meta-envelope-contract.md](meta-envelope-contract.md) — the `_meta` planning-loop input surface, field by field.
- [composite-tool-roadmap.md](composite-tool-roadmap.md) — the composite tools and their contract invariants.
- [use-case-pricing.md](use-case-pricing.md) — serious-use-case scenarios and the citation policy.
