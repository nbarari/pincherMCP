package server

import (
	"context"
	"strings"
	"testing"
)

// #1023: handleHealth silently degraded to the minimal
// {schema_version, db_path} envelope when the caller passed a
// project name that didn't resolve — no warning, no diagnosis. The
// caller couldn't tell typo from not-yet-indexed from wrong-arg-
// shape. Now: surface the failure in _meta.warnings.

func TestHandleHealth_UnknownProject_WarnsAboutResolution(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleHealth(context.Background(), makeReq(map[string]any{
		"project": "totally-bogus-project",
	}))
	if err != nil {
		t.Fatalf("handleHealth: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success (degraded to global view); got error: %s", textOf(t, res))
	}

	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	warnings, _ := meta["warnings"].([]any)
	foundWarn := false
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "totally-bogus-project") && strings.Contains(s, "did not resolve") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("expected project-resolution warning naming the failed lookup; got warnings=%v", warnings)
	}
}

// Regression guard: omitting `project` entirely produces no
// resolution warning — the global-view shape is the documented
// default and shouldn't surface a "did not resolve" message.
func TestHandleHealth_OmittedProject_NoResolutionWarning(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleHealth(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleHealth: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "did not resolve") {
			t.Errorf("omitted-project call must not produce resolution warning; got %q", s)
		}
	}
}
