package server

import (
	"context"
	"strings"
	"testing"
)

// #1015: handleDoctor used to silently coerce lookback_hours=0 and
// top=0 to their defaults (168, 10) without telling the caller. Same
// silent-clamp pattern as search (#879) / neighborhood (#1013). The
// caller asked for "all" (0) and got 168 hours back — silent semantic
// drift. Now: surface the clamp in _meta.warnings if the caller
// explicitly passed an invalid value.

func TestHandleDoctor_LookbackZeroClampSurfacesWarning(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{
		"lookback_hours": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	foundClamp := false
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "lookback_hours") && strings.Contains(s, "clamped to 168") {
			foundClamp = true
			break
		}
	}
	if !foundClamp {
		t.Errorf("expected lookback_hours clamp warning; got warnings=%v", warnings)
	}
}

func TestHandleDoctor_TopZeroClampSurfacesWarning(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{
		"top": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	foundClamp := false
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "top") && strings.Contains(s, "clamped to 10") {
			foundClamp = true
			break
		}
	}
	if !foundClamp {
		t.Errorf("expected top clamp warning; got warnings=%v", warnings)
	}
}

// Regression guard: when the caller omits the param entirely, no
// clamp warning should fire — they accepted the default explicitly.
func TestHandleDoctor_OmittedParam_NoClampWarning(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "clamped to 168") || strings.Contains(s, "clamped to 10") {
			t.Errorf("default-only call must not produce clamp warnings; got %q", s)
		}
	}
}
