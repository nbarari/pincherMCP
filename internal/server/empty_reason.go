package server

// #1252: stable machine-readable taxonomy for empty-response branches.
//
// Pincher already stamps `_meta.diagnosis` (free-text) on every empty result.
// Diagnosis is for humans; agents and routers had to regex-match prose to
// decide whether to retry, widen, or give up.
//
// `_meta.empty_reason` is the stable code that lives alongside diagnosis.
// Callers consume the enum; diagnosis stays as the rendered explanation.
//
// Add codes here only — never inline string literals at stamp sites — so a
// renamed code surfaces as a build break and the taxonomy stays auditable.
const (
	// no_project_indexed: the project arg resolved to nothing on disk OR
	// the session has no project and none was passed. Recovery: `index <path>`.
	EmptyReasonNoProjectIndexed = "no_project_indexed"

	// stale_index: the running binary is newer than the project's
	// schema_version_at_index OR the working tree drifted vs index without
	// a re-index. Recovery: `index force=true`.
	EmptyReasonStaleIndex = "stale_index"

	// unsupported_language: the file extension was detected but no
	// extractor is registered for that language (Haskell, post-v0.63).
	EmptyReasonUnsupportedLanguage = "unsupported_language"

	// low_confidence_extractor: the extractor ran but every symbol fell
	// below the min_confidence floor — caller's threshold was too strict
	// for this language tier (regex 0.85 / approximate 0.70).
	EmptyReasonLowConfidenceExtractor = "low_confidence_extractor"

	// same_file_only: the language has same-file CALLS edges but no
	// cross-file resolver (every non-Go/Python extractor pre-#1177).
	// Trace/neighborhood return empty when the caller asks for cross-file
	// reach. Recovery: scope to same-file or wait on cross-file work.
	EmptyReasonSameFileOnly = "same_file_only"

	// cross_file_unavailable: the language has an extractor but emits zero
	// edges (regex-tier languages pre-v0.62 for CALLS). Distinct from
	// same_file_only because there's no edge graph at all, not just no
	// cross-file resolution.
	EmptyReasonCrossFileUnavailable = "cross_file_unavailable"

	// query_too_narrow: the query is well-formed and the corpus is
	// non-empty, but the combined filters (kind + language + corpus +
	// min_confidence) excluded everything. Recovery: drop one filter at
	// a time — verifyEmptySearchCause already names which one.
	EmptyReasonQueryTooNarrow = "query_too_narrow"

	// no_results_in_corpus: the query is fine, the filters are fine, but
	// the symbol genuinely doesn't appear in the indexed corpus. Distinct
	// from query_too_narrow because no filter relaxation rescues it.
	EmptyReasonNoResultsInCorpus = "no_results_in_corpus"

	// cap_dropped_all: every candidate match was dropped by a max_hops /
	// limit / offset cap. Recovery: raise the cap or paginate. #1033's
	// "offset past end" case is the canonical instance.
	EmptyReasonCapDroppedAll = "cap_dropped_all"

	// incremental_no_change: index ran but every file was unchanged
	// (incremental fast path) OR every reprocessed file had symbol-neutral
	// edits. Not a bug — distinct from no_project_indexed because the
	// project IS indexed; this run just had nothing to do. #425 split this
	// out of the generic empty-extractor diagnosis.
	EmptyReasonIncrementalNoChange = "incremental_no_change"

	// all_files_blocked: every discovered file was filtered by
	// ast.ShouldSkip (lockfiles, minified bundles, source maps). Expected
	// for vendor-only or build-artifact-only directories.
	EmptyReasonAllFilesBlocked = "all_files_blocked"

	// extractor_emitted_nothing: files were processed and not blocked, but
	// the extractor returned zero symbols. Usually a language-detection
	// gap (extension not mapped) or a malformed source file.
	EmptyReasonExtractorEmittedNothing = "extractor_emitted_nothing"
)

// stampEmpty sets the machine-readable empty_reason code and the
// human-readable diagnosis on the meta map in one shot. Use this at every
// empty-response branch instead of writing meta["diagnosis"] directly so
// the enum stays paired with the prose.
//
// Callers that need to set next_steps, hints, or warnings stamp those
// separately — empty_reason + diagnosis is the minimum pair.
func stampEmpty(meta map[string]any, reason, humanDiagnosis string) {
	if meta == nil {
		return
	}
	meta["empty_reason"] = reason
	meta["diagnosis"] = humanDiagnosis
}
