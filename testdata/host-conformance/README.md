# Host conformance corpus

Each subdirectory is one supported MCP host. Inside:

- `workflow.jsonl` — recorded MCP transcript: ordered JSON-RPC 2.0
  request/response pairs the host actually sends + receives during
  a "canonical workflow." One JSON object per line, alternating
  `direction: "client→server"` and `direction: "server→client"`.
- `expectations.json` — assertions over the response side: required
  fields, value-shape constraints, and at least one regex over the
  payload that proves the response is semantically useful (not just
  syntactically well-formed).

The conformance runner (`scripts/host-conformance.sh`) replays each
`workflow.jsonl` against a freshly-built pincher (no real host needed)
and validates responses against `expectations.json`. Drift caught here
fails CI as a release blocker.

Tracked by [#1532](https://github.com/kwad77/pincher/issues/1532)
(FILE-M, v0.87, the corpus) and
[#1536](https://github.com/kwad77/pincher/issues/1536)
(FILE-R, v0.91, the gate).

## When to update

When you change an MCP-visible tool's request or response shape:

1. Capture a new transcript from the live host that broke (the host's
   developer tools log the wire bytes).
2. Update `workflow.jsonl` for that host.
3. Update `expectations.json` to reflect the new shape.
4. Re-run `scripts/host-conformance.sh` locally to confirm parity.

A response-shape change that doesn't break the recorded transcript
either means (a) the change is purely additive (good — semver 1.x
allows this), or (b) the corpus doesn't actually exercise the changed
path (gap — file an issue to extend the corpus).

## Initial scope (v0.87)

This directory ships in the FILE-M PR with **one** host (`claude-code`)
as a working reference. FILE-M acceptance criterion is "one canonical
workflow script per host" — the remaining hosts (cursor, codex,
jetbrains, vscode-copilot, zed) are filed as follow-up tasks that ship
their workflow recordings between v0.87 and v0.91 (when FILE-R promotes
the gate to release-blocker per #1536).
