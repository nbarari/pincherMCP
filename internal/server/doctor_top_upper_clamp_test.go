package server

import (
	"context"
	"strings"
	"testing"
)

// #1016: handleDoctor's `top` parameter clamps only the low end
// (top <= 0 → 10) and had no upper bound. Pre-fix, a caller passing
// top=99999 on a multi-project install produced a 506 KB response
// that blew the MCP per-call token cap — agent saw a truncation
// error with no recovery path. Same shape as search (#532) and
// neighborhood (#1013): clamp at 500 with a _meta.warnings entry.

func TestHandleDoctor_HugeTopClampsTo500WithWarning(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{
		"top": float64(99999),
	}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	foundClamp := false
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "top=99999") && strings.Contains(s, "clamped to 500") {
			foundClamp = true
			break
		}
	}
	if !foundClamp {
		t.Errorf("expected top upper-bound clamp warning; got warnings=%v", warnings)
	}
}

// Regression guard: top=200 is inside the valid range — no upper-bound
// clamp warning.
func TestHandleDoctor_InRangeTopNoUpperClamp(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{
		"top": float64(200),
	}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "clamped to 500") {
			t.Errorf("top=200 (in range) must not trigger upper-bound clamp; got warning %q", s)
		}
	}
}
