# ADR-0002: v1.0 frozen surface — what we promise, what we don't

**Status:** Accepted
**Date:** 2026-05-18
**Decision-maker:** kwad77 (sole maintainer through v0.x)
**Issues:** [#1530 (FILE-K)](https://github.com/kwad77/pincher/issues/1530) · [#1531 (FILE-L)](https://github.com/kwad77/pincher/issues/1531)
**Supersedes:** none
**Related:** [`.planning-roadmap-to-v1.md`](../../.planning-roadmap-to-v1.md) Phase 4 v0.84 API freeze checkpoint

## Context

v1.0 is the first release that explicitly promises a stable surface. Without an explicit declaration of *which* surface elements are frozen, every minor 1.x release becomes a backwards-compatibility negotiation — and consumers (agents, dashboards, HTTP clients, MCP hosts) can't reliably plan against pincher's API.

This ADR enumerates each surface element and assigns it one of three statuses:

| Status | Meaning |
|---|---|
| `frozen` | Stable across all 1.x. A breaking change requires a 2.0 release. |
| `evolving` | Stable enough to use; additions are welcome in any minor, removals require a deprecation cycle (at least one minor of `Deprecated:` documentation + log warning before removal in a later minor). |
| `experimental` | No compatibility promise. Behavior can change in any minor. Explicitly marked in the docs / response shape. |

The freeze checkpoint is **v0.84.0**. After v0.84.0, every PR that changes a `frozen` surface element either (a) is reverted before merging, or (b) is rejected and re-shaped to a `evolving` / `experimental` alternative.

## The v1.0 surface

### Tool names (frozen)

The 26 MCP tool names registered in `internal/server/server.go registerTools()` as of v0.83. Adding a new tool in any 1.x minor is non-breaking. **Renaming** or **removing** a tool requires a 2.0 release.

```
adr          architecture     audit_unused     changes
context      context_for_task dead_code        doctor
fetch        guide            health           index
init         investigate_failure              list
neighborhood plan_change      query            rebuild_fts
schema       search           self_test        stats
symbol       symbols          trace
```

The split between MCP-visible and operator-only tools is documented in `internal/server/mcp_surface_split_test.go`'s `expectedMCPTools`. That map is frozen at v0.84.0 too — flipping a tool from "MCP-visible" to "operator-only" or vice-versa is a breaking change.

### Tool input + output shapes (frozen)

Every tool's `InputSchema` JSON Schema declared in `registerTools()` and every tool's output schema declared in `internal/server/openapi_output_schemas.go` `outputSchemas` are frozen at v0.84.0:

- **Adding optional input fields** in a 1.x minor is non-breaking (the contract test `TestToolContract_GoldenFile` catches accidental drift).
- **Adding optional output fields** in a 1.x minor is non-breaking (consumers MUST ignore unknown fields per the `_meta` envelope contract, [`docs/integrations/meta-envelope-contract.md`](../integrations/meta-envelope-contract.md)).
- **Removing fields, renaming fields, or changing field types** requires a 2.0 release.
- **Making an optional field required** requires a 2.0 release.
- **Making a required field optional** is non-breaking in 1.x (consumers that always set it keep working).
- **Changing enum values** of a field requires a 2.0 release.

The contract test in `internal/server/tool_contract_golden_test.go` is the canary — `testdata/tool-contract.json` is the source of truth for 1.x compatibility; CI fails on any breaking change.

### `_meta` envelope (frozen with named additive extension points)

The shape declared in [`docs/integrations/meta-envelope-contract.md`](../integrations/meta-envelope-contract.md) is frozen. Fields keyed `_v2` (e.g. `warnings_v2`, `diagnosis_v2`, `error_v2`) are the documented extension points for breaking changes that ship as side-by-side new fields; the original `warnings` / `diagnosis` / `error` keys stay populated for backwards compatibility within 1.x.

- Adding new fields to `_meta` is non-breaking (consumers ignore unknown keys).
- Removing or renaming existing `_meta` fields requires a 2.0 release.
- Bumping to a `_v3` shape is non-breaking — the existing `_v2` shape stays populated.

### HTTP gateway routes (frozen)

`GET /v1/openapi.json`, `POST /v1/<tool>`, `GET /v1/hook-stats`, `GET /v1/architecture/<aspect>`, `GET /v1/tool-payload-stats`, `GET /metrics` (Prometheus, when wired per FILE-G), `GET /v1/icons/<tool>` (when wired per #1079 if it lands), and any other public route declared in `docs/REFERENCE.md` under "HTTP REST gateway" or "Streamable HTTP" are frozen.

- Adding new routes in 1.x is non-breaking.
- Changing the URL structure or response shape of an existing route requires a 2.0 release.

The OpenAPI spec served at `/v1/openapi.json` is itself the wire contract; any deviation between the spec and the actual route response is a bug.

### CLI subcommands + flags (frozen)

The 15 subcommands enumerated in `internal/server/reference_md_cli_subcommand_parity_test.go expectedCLISubcommands` are frozen:

```
bench        doctor       health-check hook-stats   index
init         project      rebuild-fts  self-test    stats
supervised   update       vacuum       verify       web
```

- Adding new subcommands is non-breaking.
- Removing or renaming subcommands requires a 2.0 release.
- Adding new flags to an existing subcommand is non-breaking.
- Removing flags or changing flag semantics requires a 2.0 release (one deprecation-cycle minor required before removal in a later minor).

### Symbol ID format (frozen)

`{file_path}::{qualified_name}#{kind}` per `internal/db/db.go MakeSymbolID()`. The format is frozen.

- Built-in kinds in `internal/ast/registry.go` are frozen at v0.84.0.
- Adding new kinds (e.g. a new language extractor's specific kind) is non-breaking.
- Renaming or removing kinds requires a 2.0 release.

### Database schema (evolving)

The SQLite schema is **evolving**, not frozen — but the migration story is part of the contract:

- Every schema migration in `schemaMigrations` (`internal/db/db.go`) carries an `invalidates` classification (`invalidatesAll` / `invalidatesNothing` / `Languages`).
- Pincher only ever migrates forward. Down-migrations are out of scope.
- A 1.x → 2.x upgrade may require a full reindex (`pincher index --force <path>`) — documented in the migration guide.
- The wire shapes (tool I/O, HTTP responses, CLI output) never expose schema version numbers as part of their compatibility contract.

### pinchQL grammar (evolving)

The query language (`internal/cypher`) is **evolving**. Existing keywords + syntax stay supported through 1.x; new keywords + clauses are welcome in any minor. Removal of a keyword requires a deprecation cycle.

### Resource URIs (experimental for v1.0)

The MCP `resources` capability (#1083) is **experimental** through v1.0 if it ships. The URI shape `pincher://<project>/<kind>/<id>` (#1369 per-tool anchor convention) is reserved but its compatibility status is not promised until a 1.x minor explicitly graduates it to `evolving`.

### Plugin extractor API (experimental, conditional per FILE-V)

If #1333 (plugin surface for custom language extractors) ships in v0.88 per FILE-V, the plugin API is **experimental** for v1.0. Promotion to `evolving` or `frozen` requires a follow-up ADR after at least one external plugin author has implemented against the API and reported back.

## What's explicitly NOT frozen

The following are explicitly not part of the v1.0 surface contract — pincher can change them in any minor without a deprecation cycle:

- **Internal extraction confidence values** (the 0.0 / 0.70 / 0.85 / 1.0 tiers per [`docs/REFERENCE.md`](../REFERENCE.md#language-support)). Promotion of a language from tier N to tier N+1 is a feature, not a breaking change.
- **Internal blocklist of paths** (`internal/ast/blocklist.go`). Adding or removing entries is non-breaking.
- **Doctor advisory message wording** — the advisory codes (`blast_radius_high`, `ghost_extraction_signature`, etc.) are part of the `_meta.warnings_v2` contract; the human-readable message strings are not.
- **Slog field names + log levels** — observability output is best-effort, not a compatibility contract.
- **Dashboard HTML / CSS** — internal UI; snapshot tests pin the current shape but the shape itself can change.

## Risks

- **Over-freezing.** Declaring 26 tool I/O shapes frozen at v0.84 means a real bug in a tool's output schema requires a 2.0 release. Mitigation: the `_v2` extension-point pattern means breaking changes can ship as side-by-side new fields without a major. Already proven by `warnings_v2` / `diagnosis_v2` / `error_v2` in the existing `_meta` envelope.
- **Under-freezing.** Putting too many surfaces in `evolving` makes "1.x" promise weak. Mitigation: the bias above is "freeze the agent-callable surface, evolve the operator surface" — agents are the dominant consumer, operators can absorb migration cost via release notes.
- **Schema migrations during 1.x.** If a schema change requires a forced reindex (e.g. v18→v19 `pending_edges`), that's user-visible cost even though we don't promise schema compatibility. Mitigation: the `invalidates` classification + force-reindex gate (#1497) is now part of the contract — `invalidatesNothing` migrations never trigger reindex; `invalidatesAll` ones do; the user sees the cost in advance via `doctor`'s `startup_migrations` field.

## Acceptance

- [x] ADR drafted (this file).
- [ ] CONTRIBUTING.md updated with the matching semver rules (#1531 FILE-L, this PR's second commit).
- [ ] CI gates: contract tests on every frozen surface element exist (`TestToolContract_GoldenFile`, `TestRegisterTools_Annotations_EveryToolHasOne`, `TestMCPSurface_AllRegisteredToolsAgentCallable`, `TestReferenceMD_EveryCLISubcommandHasSection`).
- [ ] PR-template checkbox added: "Does this PR change a frozen surface element per ADR-0002?"
- [ ] CLAUDE.md release-prep checklist updated to reference this ADR.
- [ ] Acceptance audit at v0.84.0 tag: every surface element listed above has either (a) a contract test in CI or (b) a documented exemption.

## References

- [`docs/integrations/meta-envelope-contract.md`](../integrations/meta-envelope-contract.md) — `_meta` envelope wire shape.
- [`internal/server/openapi_output_schemas.go`](../../internal/server/openapi_output_schemas.go) — frozen per-tool output schemas.
- [`internal/server/testdata/tool-contract.json`](../../internal/server/testdata/tool-contract.json) — frozen tool-contract golden.
- [`internal/server/mcp_surface_split_test.go`](../../internal/server/mcp_surface_split_test.go) — frozen MCP-visible split.
- [`internal/server/reference_md_cli_subcommand_parity_test.go`](../../internal/server/reference_md_cli_subcommand_parity_test.go) — frozen CLI subcommand list.
- [`internal/db/db.go MakeSymbolID`](../../internal/db/db.go) — frozen symbol-ID format.
