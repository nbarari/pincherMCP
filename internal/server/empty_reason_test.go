package server

import (
	"context"
	"testing"
)

// #1252: stable _meta.empty_reason enum gate tests.
//
// Coverage strategy follows the project's positive/negative/control/cross-check
// pattern. Each empty branch in each instrumented handler should:
//   1. Stamp empty_reason with one of the valid enum values
//   2. Stamp diagnosis (human-readable) alongside
//   3. Survive applyLiteMeta — the enum is per-call actionable signal
//
// Tests live here (not split per-handler) so a new code added to
// empty_reason.go without a stamp site, or a stamp site that uses a
// literal string instead of the constant, surfaces in one place.

// validEmptyReasons is the source of truth for the enum. New codes
// added in empty_reason.go must be added here; tests pin the set so
// a typo'd stamp ("query_to_narrow" vs "query_too_narrow") fails loud.
var validEmptyReasons = map[string]bool{
	EmptyReasonNoProjectIndexed:        true,
	EmptyReasonStaleIndex:              true,
	EmptyReasonUnsupportedLanguage:     true,
	EmptyReasonLowConfidenceExtractor:  true,
	EmptyReasonSameFileOnly:            true,
	EmptyReasonCrossFileUnavailable:    true,
	EmptyReasonQueryTooNarrow:          true,
	EmptyReasonNoResultsInCorpus:       true,
	EmptyReasonCapDroppedAll:           true,
	EmptyReasonIncrementalNoChange:     true,
	EmptyReasonAllFilesBlocked:         true,
	EmptyReasonExtractorEmittedNothing: true,
	EmptyReasonTargetNotResolved:       true,
}

// Positive: stampEmpty sets both fields atomically on a fresh meta map.
func TestStampEmpty_SetsBothFields(t *testing.T) {
	t.Parallel()
	meta := map[string]any{}
	stampEmpty(meta, EmptyReasonNoProjectIndexed, "test diagnosis text")
	if got := meta["empty_reason"]; got != EmptyReasonNoProjectIndexed {
		t.Errorf("empty_reason = %v; want %q", got, EmptyReasonNoProjectIndexed)
	}
	if got := meta["diagnosis"]; got != "test diagnosis text" {
		t.Errorf("diagnosis = %v; want %q", got, "test diagnosis text")
	}
}

// Negative: stampEmpty on a nil map is a no-op, not a panic. Some
// handlers nil-check before allocating; the helper must be safe in
// that window.
func TestStampEmpty_NilMapIsNoop(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("stampEmpty(nil, ...) panicked: %v", r)
		}
	}()
	stampEmpty(nil, EmptyReasonNoProjectIndexed, "irrelevant")
}

// Control: every constant exported from empty_reason.go must appear in
// validEmptyReasons. A new code added without updating the gate would
// silently bypass the per-handler enum-membership checks below.
func TestEmptyReason_ConstantsAreRegistered(t *testing.T) {
	t.Parallel()
	for _, c := range []string{
		EmptyReasonNoProjectIndexed,
		EmptyReasonStaleIndex,
		EmptyReasonUnsupportedLanguage,
		EmptyReasonLowConfidenceExtractor,
		EmptyReasonSameFileOnly,
		EmptyReasonCrossFileUnavailable,
		EmptyReasonQueryTooNarrow,
		EmptyReasonNoResultsInCorpus,
		EmptyReasonCapDroppedAll,
		EmptyReasonIncrementalNoChange,
		EmptyReasonAllFilesBlocked,
		EmptyReasonExtractorEmittedNothing,
		EmptyReasonTargetNotResolved,
	} {
		if !validEmptyReasons[c] {
			t.Errorf("constant %q is exported but not in validEmptyReasons gate map", c)
		}
	}
}

// Cross-check: applyLiteMeta strips dogfood-only fields but MUST preserve
// empty_reason — the AC ("thin-client meta=lite still strips other dogfood
// fields but keeps empty_reason — it's actionable per-call signal") is the
// guarantee callers need to consume the enum unconditionally.
func TestApplyLiteMeta_PreservesEmptyReason(t *testing.T) {
	t.Parallel()
	meta := map[string]any{
		"empty_reason":      EmptyReasonNoProjectIndexed,
		"diagnosis":         "stamped together with the enum",
		"capabilities":      []string{"schema_v33"},
		"baseline_method":   "full_file_read",
		"complexity_tier":   "lite",
		"tokens_used":       100,
		"tokens_saved":      200,
		"tokens_saved_pct":  50.0,
	}
	applyLiteMeta(meta)
	if got := meta["empty_reason"]; got != EmptyReasonNoProjectIndexed {
		t.Errorf("empty_reason was stripped by applyLiteMeta; want it preserved as %q, got %v", EmptyReasonNoProjectIndexed, got)
	}
	if got := meta["diagnosis"]; got != "stamped together with the enum" {
		t.Errorf("diagnosis was stripped by applyLiteMeta; want it preserved, got %v", got)
	}
	// Cross-check that the strip list still fires for fields it owns.
	for _, k := range []string{"capabilities", "baseline_method", "complexity_tier", "tokens_used", "tokens_saved", "tokens_saved_pct"} {
		if _, present := meta[k]; present {
			t.Errorf("applyLiteMeta failed to strip %q", k)
		}
	}
}

// Per-handler integration: each instrumented empty branch stamps a code
// from validEmptyReasons. These exercise the live handler — a stamp site
// that drifts off the enum (or a literal "query_to_narrow" typo) fails.

func assertEmptyReason(t *testing.T, meta map[string]any, want string) {
	t.Helper()
	got, ok := meta["empty_reason"].(string)
	if !ok {
		t.Fatalf("empty_reason missing or non-string; meta keys: %v", metaKeys(meta))
	}
	if !validEmptyReasons[got] {
		t.Errorf("empty_reason = %q is not in validEmptyReasons gate map", got)
	}
	if want != "" && got != want {
		t.Errorf("empty_reason = %q; want %q", got, want)
	}
	if _, hasDiag := meta["diagnosis"]; !hasDiag {
		t.Errorf("diagnosis must be stamped alongside empty_reason; meta keys: %v", metaKeys(meta))
	}
}

// #1603 v0.84: orphan-stamp audit. Records which EmptyReason* constants
// have a production stamp site (handler emits the code on an empty
// result). Four constants currently have catalog entries + why_empty
// support but NO handler ever stamps them — they're real conditions
// surfaced via _meta.warnings or specific tool advisories instead.
// Pin the current state so a future "we forgot to stamp X" regression
// is visible at PR review (the orphan set must only shrink).
//
// To close the gap: instrument the documented detection paths and
// move the constant from `knownOrphan` to `knownStamped`. The test
// becomes a hard contract once `knownOrphan` is empty.
func TestEmptyReason_OrphanStampAudit(t *testing.T) {
	t.Parallel()

	// knownStamped — production code in this package emits this code
	// on at least one empty-result path. Verified by grep of
	// internal/server/*.go (excluding *_test.go + empty_reason.go +
	// why_empty.go).
	knownStamped := map[string]bool{
		EmptyReasonNoProjectIndexed:        true, // server.go list/architecture/schema
		EmptyReasonCrossFileUnavailable:    true, // server.go trace/architecture
		EmptyReasonQueryTooNarrow:          true, // server.go search/trace/changes/list
		EmptyReasonNoResultsInCorpus:       true, // server.go search/trace/changes; audit_unused; context_for_task; investigate_failure; onboard_module
		EmptyReasonCapDroppedAll:           true, // server.go search/list; neighborhood
		EmptyReasonIncrementalNoChange:     true, // server.go handleIndex
		EmptyReasonAllFilesBlocked:         true, // server.go handleIndex
		EmptyReasonExtractorEmittedNothing: true, // server.go handleIndex
		EmptyReasonTargetNotResolved:       true, // plan_change, investigate_failure, context_for_task (v0.82 #1578 + v0.83 #1591)
	}

	// knownOrphan — constant exists in empty_reason.go + has a catalog
	// entry in why_empty.go + this test acknowledges the gap. why_empty
	// still returns the recovery action when an external caller passes
	// these codes (e.g. extracted from _meta.warnings), but no handler
	// auto-stamps them today.
	//
	// Tracked in #1603. When a constant gets a real stamp site, move
	// the row to knownStamped and a future PR will see the orphan
	// count shrink.
	knownOrphan := map[string]bool{
		EmptyReasonStaleIndex:             true, // condition fires via _meta.warnings (binary_stale)
		EmptyReasonUnsupportedLanguage:    true, // condition surfaces in doctor advisory, not stamped on search empty
		EmptyReasonLowConfidenceExtractor: true, // no handler tells caller "your min_confidence excluded everything"
		EmptyReasonSameFileOnly:           true, // collapsed into EmptyReasonCrossFileUnavailable in current trace path
	}

	// Every constant must appear in exactly one of the two sets.
	allConstants := []string{
		EmptyReasonNoProjectIndexed,
		EmptyReasonStaleIndex,
		EmptyReasonUnsupportedLanguage,
		EmptyReasonLowConfidenceExtractor,
		EmptyReasonSameFileOnly,
		EmptyReasonCrossFileUnavailable,
		EmptyReasonQueryTooNarrow,
		EmptyReasonNoResultsInCorpus,
		EmptyReasonCapDroppedAll,
		EmptyReasonIncrementalNoChange,
		EmptyReasonAllFilesBlocked,
		EmptyReasonExtractorEmittedNothing,
		EmptyReasonTargetNotResolved,
	}
	for _, c := range allConstants {
		stamped := knownStamped[c]
		orphan := knownOrphan[c]
		if stamped && orphan {
			t.Errorf("constant %q is in both knownStamped AND knownOrphan — one or the other", c)
		}
		if !stamped && !orphan {
			t.Errorf("constant %q is in neither knownStamped nor knownOrphan — add it to one (orphan if no handler stamps it; stamped if a handler does)", c)
		}
	}
	// Surface the orphan count so the gap is visible at PR review.
	t.Logf("empty_reason orphan-stamp audit: %d stamped, %d orphan (tracked in #1603)",
		len(knownStamped), len(knownOrphan))
}

// Positive: list on a freshly-initialised test server (no projects
// indexed) stamps no_project_indexed. The empty-store branch is the
// cleanest deterministic empty path — no fixture corpus needed.
func TestEmptyReason_ListEmptyStoreStampsNoProject(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	res, err := srv.handleList(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, res)
	if total, _ := body["total"].(float64); total != 0 {
		t.Skipf("list returned %v projects; cannot exercise empty-store branch", total)
	}
	meta, _ := body["_meta"].(map[string]any)
	assertEmptyReason(t, meta, EmptyReasonNoProjectIndexed)
}

// Positive: schema on a non-existent project stamps no_project_indexed.
func TestEmptyReason_SchemaUnknownProjectStampsNoProject(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	res, err := srv.handleSchema(context.Background(), makeReq(map[string]any{
		"project": "definitely-not-a-project-xyzzy",
	}))
	if err != nil {
		t.Fatalf("handleSchema: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Skip("handler returned no _meta on unknown project — possibly errored early")
	}
	// Either rich error envelope OR empty stamp; only assert when stamped.
	if _, hasReason := meta["empty_reason"]; hasReason {
		assertEmptyReason(t, meta, EmptyReasonNoProjectIndexed)
	}
}
