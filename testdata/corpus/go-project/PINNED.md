# Pinned corpus: `go-project`

A small synthetic Go project used by the corpus-snapshot tooling (#33) to lock
in pincher's behaviour on a known input. Hand-crafted rather than vendored
from a real repo so:

- The expected snapshot stays stable across pincher upgrades (no upstream
  drift to absorb).
- Symbol counts are small enough to eyeball-verify when the snapshot diff
  reports a change.
- Cross-file CALLS / IMPORTS shapes are deliberate — every edge in
  `<name>.snapshot.json` traces back to a specific construction here.

If pincher learns a new Go feature (generics support, build-tag awareness,
etc.) and the symbol count for this corpus changes, regenerate the snapshot
via `make corpus-snapshot-update` AND review the diff in the same PR — the
review IS the rationale for the change.

## Layout

- `go.mod` — establishes `example.com/demo` so the Go extractor can rewrite
  intra-module IMPORTS.
- `cmd/cli/main.go` — has `main` and a helper `Greet`. Calls into `internal/auth`.
- `internal/auth/auth.go` — package-level `Open` function + a struct with
  one method. Cross-file CALLS edge from `cmd/cli` to `internal/auth.Open`
  is the canonical regression test for PR #27's deferred resolution.
