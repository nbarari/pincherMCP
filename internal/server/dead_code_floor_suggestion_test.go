package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #896: the dead_code empty-result advisory used to hard-code 0.7 as
// the suggested next floor regardless of the caller's current value.
// At min_confidence=0.7 that was a no-op suggestion; at 0.0 it was a
// logical inversion (recommending a HIGHER floor would NARROW the
// candidate pool, the opposite of "find more dead code"). The floor
// now scales down from current.

func TestSuggestDeadCodeFloor(t *testing.T) {
	cases := []struct {
		current float64
		want    float64
	}{
		{0.95, 0.7}, // default → step down to regex-stable floor
		{0.85, 0.7},
		{0.71, 0.7},
		{0.7, 0.0}, // already at 0.7 → step down to 0.0
		{0.5, 0.0},
		{0.01, 0.0},
		{0.0, -1.0}, // already at 0.0 → no lower floor (sentinel)
		{-0.5, -1.0},
	}
	for _, c := range cases {
		got := suggestDeadCodeFloor(c.current)
		if got != c.want {
			t.Errorf("suggestDeadCodeFloor(%v) = %v, want %v", c.current, got, c.want)
		}
	}
}

func TestHandleDeadCode_EmptyResult_AtZeroFloor_NoMinConfidenceHint(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p896zero"
	store.UpsertProject(db.Project{ID: "p896zero", Path: "/tmp/p896zero", Name: "p896zero", IndexedAt: time.Now()})

	// No symbols → empty result. Caller is already at the widest floor.
	result, _ := srv.handleDeadCode(context.Background(), makeReq(map[string]any{
		"min_confidence": 0.0,
	}))
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	diag, _ := meta["diagnosis"].(string)
	if diag == "" {
		t.Error("diagnosis must be set even when no further floor exists")
	}
	// The diagnosis should NOT recommend lowering further or suggest
	// a min_confidence value — at 0.0 you can't go lower.
	if strings.Contains(diag, "lower min_confidence") {
		t.Errorf("at min_confidence=0.0, diagnosis should not recommend lowering further; got %q", diag)
	}
	if strings.Contains(diag, "0.7") {
		t.Errorf("at min_confidence=0.0, diagnosis must not name 0.7 as the suggested floor (HIGHER, would narrow pool); got %q", diag)
	}
	// And next_steps should not propose a min_confidence change.
	if steps, _ := meta["next_steps"].([]any); len(steps) > 0 {
		first, _ := steps[0].(map[string]any)
		if argsStr, _ := first["args"].(string); strings.Contains(argsStr, "min_confidence") {
			t.Errorf("at 0.0 floor, next_steps must not suggest another min_confidence change; got %q", argsStr)
		}
	}
}

// At min_confidence=0.7 the suggested next floor is 0.0, not another 0.7.
func TestHandleDeadCode_EmptyResult_AtMidFloor_SuggestsZero(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p896mid"
	store.UpsertProject(db.Project{ID: "p896mid", Path: "/tmp/p896mid", Name: "p896mid", IndexedAt: time.Now()})

	result, _ := srv.handleDeadCode(context.Background(), makeReq(map[string]any{
		"min_confidence": 0.7,
	}))
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	steps, _ := meta["next_steps"].([]any)
	if len(steps) == 0 {
		t.Fatal("expected next_steps for empty result; got none")
	}
	first, _ := steps[0].(map[string]any)
	argsStr, _ := first["args"].(string)
	if !strings.Contains(argsStr, `"min_confidence":0`) {
		t.Errorf("at min_confidence=0.7, suggested next floor must be 0.0 (lower); got %q", argsStr)
	}
	// And it must NOT suggest 0.7 again (the pre-fix no-op).
	if strings.Contains(argsStr, `"min_confidence":0.7`) {
		t.Errorf("at min_confidence=0.7, suggested floor must not also be 0.7 (no-op); got %q", argsStr)
	}
}
