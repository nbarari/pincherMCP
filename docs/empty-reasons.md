# `empty_reason` catalog — failure modes and recovery

Every pincher tool that returns an empty result stamps `_meta.empty_reason` (a stable enum) alongside `_meta.diagnosis` (a free-text explanation). This doc enumerates every `EmptyReason*` constant, what causes it to fire, and the agent-actionable recovery path.

**Source of truth:** `internal/server/empty_reason.go`. Adding a new constant requires both:
1. A row below describing when it fires + the recovery action.
2. A positive test that exercises the new constant's firing condition.

The `why_empty` composite (Phase 4 #5, v0.85) consumes this catalog as its data source — when the agent calls `why_empty` after a previous empty result, the composite looks up the surfaced `empty_reason` and returns the matching recovery action without the agent having to read this doc.

## The 13 reason codes

### `no_project_indexed`

**Constant:** `EmptyReasonNoProjectIndexed`

**Fires when:** The `project` argument resolved to nothing on disk OR the MCP session has no project and the caller didn't pass one explicitly.

**What the agent sees:**
```json
{
  "_meta": {
    "empty_reason": "no_project_indexed",
    "diagnosis": "project arg 'myproj' did not resolve to any indexed path; ...",
    "next_steps": [
      {"tool": "list", "args": "{}", "why": "see what projects are indexed"},
      {"tool": "index", "args": "{\"path\":\"<absolute>\"}", "why": "index the project first"}
    ]
  }
}
```

**Recovery:** Call `list` to see indexed projects. If the intended project isn't there, call `index path=<absolute>` to add it.

### `stale_index`

**Constant:** `EmptyReasonStaleIndex`

**Fires when:** The running binary is newer than the project's `schema_version_at_index` OR the working tree drifted vs index without a re-index (file content hash differs from stored hash).

**Recovery:** Call `index force=true path=<project>` to refresh the index against the current binary + tree. The `health` tool's `binary_stale` field reports this state on every call so the agent can pre-empt the empty result.

### `unsupported_language`

**Constant:** `EmptyReasonUnsupportedLanguage`

**Fires when:** The file extension was detected but no extractor is registered for that language. Currently only Haskell (post-v0.63).

**Recovery:** This is a language-support gap, not a workflow bug. Either pick a different file or file an issue to add the extractor. The `architecture` tool's `languages` field shows which languages have edges.

### `low_confidence_extractor`

**Constant:** `EmptyReasonLowConfidenceExtractor`

**Fires when:** The extractor ran but every symbol fell below the `min_confidence` floor — the caller's threshold was too strict for this language tier.

**Recovery:** Lower the `min_confidence` argument. Defaults are tier-aware: AST extractors (Go, JS, Python, etc.) are 1.0 confidence; stable-regex (TS, Rust, Java) is 0.85; approximate-regex (Ruby, Scala, Lua) is 0.70.

```
search query="Login" min_confidence=0.85    # widens to stable-regex tier
search query="Login" min_confidence=0.70    # widens to approximate-regex tier
```

### `same_file_only`

**Constant:** `EmptyReasonSameFileOnly`

**Fires when:** The language has same-file CALLS edges but no cross-file resolver yet (every non-Go/Python extractor pre-#1177). `trace direction=out` or `neighborhood` returns empty when the caller asks for cross-file reach.

**Recovery:** Scope the trace to the same file (use `neighborhood` for that). Cross-file resolution is feature work — track via the language's tier-promotion issue.

### `cross_file_unavailable`

**Constant:** `EmptyReasonCrossFileUnavailable`

**Fires when:** The language has an extractor but emits zero edges of any kind (regex-tier languages pre-v0.62 for CALLS). Distinct from `same_file_only` because there's no edge graph at all, not just no cross-file resolution.

**Recovery:** Same shape as `same_file_only` — wait on extractor work, or use direct symbol search (`search`, `symbol`) which doesn't depend on the edge graph.

### `query_too_narrow`

**Constant:** `EmptyReasonQueryTooNarrow`

**Fires when:** The query is well-formed and the corpus is non-empty, but the combined filters (`kind` + `language` + `corpus` + `min_confidence` + `file_pattern`) excluded everything.

**Recovery:** Drop one filter at a time. `search`'s `verifyEmptySearchCause` helper already names which filter excluded the most candidates — the diagnosis text points at it.

```
search query="Login" kind=Class language=Go        # 0 results (no Go Classes)
search query="Login" kind=Class                    # try dropping language
search query="Login"                               # widest possible
```

### `no_results_in_corpus`

**Constant:** `EmptyReasonNoResultsInCorpus`

**Fires when:** The query is fine, the filters are fine, but the symbol genuinely doesn't appear in the indexed corpus. Distinct from `query_too_narrow` because no filter relaxation will rescue it.

**Recovery:**
1. Confirm the symbol name spelling — `search query="<partial>"` to widen.
2. Confirm the project — `list` to see if the right project is scoped.
3. If neither finds it, the symbol may live in a file that's not indexed (vendored, gitignored, in a sibling repo) — `index <other-path>` to bring it in.

### `cap_dropped_all`

**Constant:** `EmptyReasonCapDroppedAll`

**Fires when:** Every candidate match was dropped by a `max_hops`, `limit`, or `offset` cap. The canonical instance is "offset past end" — paginating past the last result page.

**Recovery:** Raise the cap, or paginate back. The diagnosis names the specific cap that fired:

```
search query="*" limit=10 offset=10000   # offset past end → cap_dropped_all
search query="*" limit=10 offset=0       # rewind to start
```

### `incremental_no_change`

**Constant:** `EmptyReasonIncrementalNoChange`

**Fires when:** `index` ran but every file was unchanged (hit the incremental fast path) OR every reprocessed file had symbol-neutral edits. Not a bug — distinct from `no_project_indexed` because the project IS indexed; this run just had nothing to do.

**Recovery:** Expected behaviour. If the agent is verifying a recent edit and sees `incremental_no_change`, that's confirmation the edit didn't change the symbol surface. If a re-extraction IS required (binary upgrade, extractor change), call `index force=true`.

### `all_files_blocked`

**Constant:** `EmptyReasonAllFilesBlocked`

**Fires when:** Every discovered file was filtered by `ast.ShouldSkip` (lockfiles, minified bundles, source maps, generated code). Expected for vendor-only or build-artifact-only directories.

**Recovery:** Either the path is wrong (you indexed a `node_modules/` or `dist/` directory) or the path genuinely has nothing pincher can extract. Check the path; if it's correct, pick a different directory.

### `extractor_emitted_nothing`

**Constant:** `EmptyReasonExtractorEmittedNothing`

**Fires when:** Files were processed and not blocked, but the extractor returned zero symbols. Usually a language-detection gap (extension not mapped to any extractor) or a malformed source file.

**Recovery:** Call `doctor` to see `extraction_failures` for the project — every parse/heuristic failure surfaces there with a reason code. If `extraction_failures` is empty too, the file extensions may not be mapped (e.g., `.txt` files in a `*.py`-only repo).

### `target_not_resolved`

**Constant:** `EmptyReasonTargetNotResolved`

**Fires when:** A composite handler (`plan_change`, `investigate_failure`, `context_for_task`) accepted input that LOOKED valid for its resolution heuristic — file extension, name, or symbol-id shape — but couldn't find a matching symbol in the index. Distinct from `no_results_in_corpus`: the data isn't missing, your input was the wrong shape.

**Recovery:** Re-issue with a more specific target, or run `search` first to confirm what shape resolves. `list` confirms the project scope.

## When to use this catalog

- **As an agent**, you don't read this directly — call `why_empty` and get the recovery action embedded in the response.
- **As a developer**, this is the contract for what each constant means. Adding a new constant means: (1) add it to `empty_reason.go` with a header comment, (2) add a row above, (3) ship a positive test.
- **As a reviewer**, an empty-response branch that stamps `empty_reason` without a recovery path in this doc is incomplete — gate at PR review.

## Related

- `internal/server/empty_reason.go` — source of truth
- `internal/server/why_empty.go` — the composite that consumes the catalog (Phase 4 #5, v0.85)
- `docs/integrations/meta-envelope-contract.md` — full `_meta` envelope shape
- `docs/integrations/composite-tool-roadmap.md` — composite roadmap including why_empty
