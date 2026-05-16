package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1128: trace direction synonyms expanded so directional words an AI
// agent typically reaches for ("incoming", "outgoing", "in", "out",
// "up", "down", and the singular "caller"/"callee") map to their
// canonical direction with a teach-the-name warning, instead of
// silently falling back to "both". Pre-fix, `direction="callers"`
// mapped to "inbound" (#839) but `direction="incoming"` fell through
// to "both" — semantically identical synonyms behaved differently.

func TestTrace_DirectionSynonyms_MapToCanonicalDirection(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p", Path: "/tmp/p", Name: "p", EdgeCount: 1})
	srv.sessionID = "p"
	callerID := "x.go::pkg.Caller#Function"
	calleeID := "x.go::pkg.Callee#Function"
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: callerID, ProjectID: "p", Name: "Caller", QualifiedName: "pkg.Caller", Kind: "Function", FilePath: "x.go"},
		{ID: calleeID, ProjectID: "p", Name: "Callee", QualifiedName: "pkg.Callee", Kind: "Function", FilePath: "x.go"},
	})
	store.BulkUpsertEdges([]db.Edge{
		{ProjectID: "p", FromID: callerID, ToID: calleeID, Kind: "CALLS"},
	})

	cases := []struct {
		direction string
		traceName string
		wantHops  int
		// substring expected in the warning — should be "inbound" or
		// "outbound" (the canonical name being taught), NOT "both".
		wantCanonical string
	}{
		// Inbound synonyms — tracing Callee finds Caller.
		{"incoming", "Callee", 1, `"inbound"`},
		{"in", "Callee", 1, `"inbound"`},
		{"up", "Callee", 1, `"inbound"`},
		{"reverse", "Callee", 1, `"inbound"`},
		{"caller", "Callee", 1, `"inbound"`},
		// Outbound synonyms — tracing Caller finds Callee.
		{"outgoing", "Caller", 1, `"outbound"`},
		{"out", "Caller", 1, `"outbound"`},
		{"down", "Caller", 1, `"outbound"`},
		{"forward", "Caller", 1, `"outbound"`},
		{"callee", "Caller", 1, `"outbound"`},
	}
	for _, c := range cases {
		t.Run(c.direction, func(t *testing.T) {
			res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
				"name":      c.traceName,
				"direction": c.direction,
				"depth":     1,
			}))
			if err != nil {
				t.Fatalf("handleTrace: %v", err)
			}
			if res.IsError {
				t.Fatalf("trace returned IsError: %s", textOf(t, res))
			}
			body := decode(t, res)

			total, _ := body["total"].(float64)
			if int(total) != c.wantHops {
				t.Errorf("direction=%q: total=%d, want %d — synonym should map to canonical direction and produce the right hops, not fall back to both (response: %s)",
					c.direction, int(total), c.wantHops, textOf(t, res))
			}

			meta, _ := body["_meta"].(map[string]any)
			warnings, _ := meta["warnings"].([]any)
			found := false
			fellBackToBoth := false
			for _, w := range warnings {
				s, ok := w.(string)
				if !ok {
					continue
				}
				if strings.Contains(s, c.wantCanonical) {
					found = true
				}
				if strings.Contains(s, `falling back to "both"`) {
					fellBackToBoth = true
				}
			}
			if !found {
				t.Errorf("direction=%q: warning should teach the canonical name %s; got: %v",
					c.direction, c.wantCanonical, warnings)
			}
			if fellBackToBoth {
				t.Errorf("direction=%q: must not fall back to both — should map to a canonical direction; got: %v",
					c.direction, warnings)
			}
		})
	}
}

// Control: the existing canonical and "callers"/"callees" cases keep
// working (we didn't break the #839 path).
func TestTrace_DirectionCanonical_StillSilent(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p", Path: "/tmp/p", Name: "p", EdgeCount: 1})
	srv.sessionID = "p"
	callerID := "x.go::pkg.Caller#Function"
	calleeID := "x.go::pkg.Callee#Function"
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: callerID, ProjectID: "p", Name: "Caller", QualifiedName: "pkg.Caller", Kind: "Function", FilePath: "x.go"},
		{ID: calleeID, ProjectID: "p", Name: "Callee", QualifiedName: "pkg.Callee", Kind: "Function", FilePath: "x.go"},
	})
	store.BulkUpsertEdges([]db.Edge{
		{ProjectID: "p", FromID: callerID, ToID: calleeID, Kind: "CALLS"},
	})
	for _, dir := range []string{"inbound", "outbound", "both"} {
		t.Run(dir, func(t *testing.T) {
			res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
				"name":      "Caller",
				"direction": dir,
				"depth":     1,
			}))
			if err != nil {
				t.Fatalf("handleTrace: %v", err)
			}
			body := decode(t, res)
			meta, _ := body["_meta"].(map[string]any)
			warnings, _ := meta["warnings"].([]any)
			for _, w := range warnings {
				if s, ok := w.(string); ok && strings.Contains(s, "direction=") {
					t.Errorf("canonical direction %q must not emit a direction warning; got: %s", dir, s)
				}
			}
		})
	}
}
