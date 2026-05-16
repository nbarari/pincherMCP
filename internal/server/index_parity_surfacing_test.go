package server

import (
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/index"
)

// #1231 follow-up to #1233: the per-file parity slog.Warn fires in the
// daemon's stderr, invisible to MCP callers. The index tool response
// must surface ParityMismatchFiles + ParityMissingSymbols (when > 0)
// AND attach a `_meta.warnings` entry naming #1231 so agents see the
// silent-loss signal in their response payload, not just logs.

// Positive: parity mismatch counts are surfaced in the response when
// the IndexResult carries non-zero values.
func TestHandleIndex_ParityMismatch_SurfacesCounts(t *testing.T) {
	t.Parallel()
	data := buildIndexResponseData(&index.IndexResult{
		Project:              "p",
		Path:                 "/p",
		Files:                10,
		Skipped:              0,
		Blocked:              0,
		Deleted:              0,
		DurationMS:           42,
		ParityMismatchFiles:  3,
		ParityMissingSymbols: 75,
	}, 100, 50)

	if got, ok := data["parity_mismatch_files"].(int); !ok || got != 3 {
		t.Errorf("expected parity_mismatch_files=3 in response; got %v", data["parity_mismatch_files"])
	}
	if got, ok := data["parity_missing_symbols"].(int); !ok || got != 75 {
		t.Errorf("expected parity_missing_symbols=75 in response; got %v", data["parity_missing_symbols"])
	}
	meta, ok := data["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("expected _meta on response; got data=%+v", data)
	}
	warnings, _ := meta["warnings"].([]string)
	if len(warnings) == 0 {
		t.Fatalf("expected _meta.warnings with parity entry; got %v", meta)
	}
	var saw bool
	for _, w := range warnings {
		if strings.Contains(w, "#1231") && strings.Contains(w, "silent symbol loss") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected warning naming #1231 + 'silent symbol loss'; got %v", warnings)
	}
}

// Negative: a healthy run (zero parity mismatches) omits the fields
// so the response stays clean.
func TestHandleIndex_NoParityMismatch_OmitsFields(t *testing.T) {
	t.Parallel()
	data := buildIndexResponseData(&index.IndexResult{
		Project:              "p",
		Files:                10,
		DurationMS:           42,
		ParityMismatchFiles:  0,
		ParityMissingSymbols: 0,
	}, 100, 50)

	if _, exists := data["parity_mismatch_files"]; exists {
		t.Errorf("healthy run must omit parity_mismatch_files; got %v", data["parity_mismatch_files"])
	}
	if _, exists := data["parity_missing_symbols"]; exists {
		t.Errorf("healthy run must omit parity_missing_symbols; got %v", data["parity_missing_symbols"])
	}
}
