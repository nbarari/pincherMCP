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

## Host coverage

| Host | Shipped | Source |
|---|---|---|
| `claude-code` | v0.87 (FILE-M #1532) | working reference |
| `cursor` | v0.88 (#1562) | canonical wire-shape |
| `codex` | v0.88 (#1563) | canonical wire-shape |
| `jetbrains` | v0.88 (#1564) | canonical wire-shape |
| `vscode-copilot` | v0.88 (#1565) | canonical wire-shape |
| `zed` | v0.88 (#1566) | canonical wire-shape |

All six hosts now have a workflow corpus. The five v0.88 hosts ship as
**canonical wire-shape** transcripts — they replicate the claude-code
reference's `initialize → tools/list → search → architecture` sequence
with the host's own `clientInfo.name`, exercising pincher's protocol
surface rather than a byte-exact capture of each host build. Replacing
any of them with a live capture (the host's developer tools log the
wire bytes) is a strict improvement — follow the steps under *When to
update* — and stays compatible with the runner + the v0.91 release-gate
(#1536, FILE-R) because the assertion schema is identical.
