# Host conformance

A per-host MCP transcript-replay gate that catches silent regressions in canonical user workflows on each supported host. Tracked by [#1532](https://github.com/kwad77/pincher/issues/1532) (FILE-M, v0.87 corpus) and [#1536](https://github.com/kwad77/pincher/issues/1536) (FILE-R, v0.91 promotion to release blocker).

## What it tests

For each host directory under `testdata/host-conformance/`, the runner:

1. Reads `workflow.jsonl` — the recorded MCP transcript (one JSON object per line, alternating `client→server` and `server→client`).
2. Builds a request stream from the client→server lines and pipes it to a freshly-built pincher in stdio mode.
3. Captures the response stream.
4. Validates the responses against `expectations.json`:
   - `tools_list_must_include` — every tool name in this list must appear in the host's `tools/list` response.
   - `search_response_shape.content_text_regex` — the search response must contain semantically useful content matching the regex (not just a well-formed empty envelope).
   - `error_envelope_forbidden_on_lines` — listed response IDs must not carry an error envelope (neither JSON-RPC `error` nor `result.isError: true`).

Failure on any of these breaks the host's canonical user-facing flow by definition.

## Phase: advisory → required

| Release | Workflow `continue-on-error` | Effect on merge / release |
|---|---|---|
| v0.87 | `true` (advisory) | Failure logs but does NOT block. |
| v0.91 | `false` (required, per FILE-R #1536) | Failure blocks release tag; listed in CLAUDE.md required-gates. |

The promotion happens by editing one line in `.github/workflows/host-conformance.yml`. The supporting corpus + scripting MUST be in place before the flip — that's why FILE-M and FILE-R are two separate gates.

## When to update transcripts

When you change an MCP-visible tool's request or response shape:

1. Either capture a new transcript from a live host (the host's developer-tools log) OR hand-author the JSON objects representing the new wire shape.
2. Update the relevant `workflow.jsonl`.
3. Update the relevant `expectations.json` if the shape change affects assertions.
4. Run `scripts/host-conformance.sh <host>` locally — must pass against the new pincher build.

A change that "doesn't break the recorded transcript" either means:

- The change is purely additive (good — semver 1.x allows this).
- The corpus doesn't actually exercise the changed path (gap — extend the corpus).

## Initial scope (v0.87)

v0.87 ships the conformance corpus with **one** host: `claude-code`. The remaining hosts (`cursor`, `codex`, `jetbrains`, `vscode-copilot`, `zed`) are filed as follow-up tasks to capture transcripts between v0.87 and v0.91 (when FILE-R promotes the gate to release-blocker).

The single-host start is intentional: the runner + corpus structure + expectations shape need a working reference before we expand. A corpus that exists at scale but has inconsistent shape is worse than a corpus that grows from one validated example.

## Why JSON-RPC replay (not "run a real host")

Three reasons:

1. **No host install required in CI.** Real hosts (Cursor, Claude Code Desktop) bundle proprietary binaries that are not easily installable in GitHub Actions.
2. **Deterministic.** Real-host behavior includes UI timing, async streaming, log noise. The recorded transcript captures the wire bytes that matter.
3. **Hand-authorable.** A maintainer can update a transcript without spinning up the host UI. Critical for the long tail of fix-the-corpus-then-fix-the-bug iterations.

The cost: the corpus is a recording, not a guarantee that the live host actually still sends those bytes. We accept that cost — when a real host changes its wire shape, the next user report sends us back to recapture, and the recapture itself is the gate signal.

## Related

- [#1532](https://github.com/kwad77/pincher/issues/1532) FILE-M — corpus scaffolding (this).
- [#1536](https://github.com/kwad77/pincher/issues/1536) FILE-R — release-blocker promotion at v0.91.
- `testdata/host-conformance/README.md` — corpus directory layout.
- `scripts/host-conformance.sh` — the replay runner.
- `internal/server/mcp_surface_split_test.go` — `expectedMCPTools` parity gate; the host-conformance corpus depends on the working-set tools it pins.
