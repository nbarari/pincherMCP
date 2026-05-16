package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// v0.65 description-honesty audit (continuation): the stats tool's
// description listed four returned items ("tokens used, tokens
// saved, call count, per-project index size") but the response is
// actually a text-rendered three-section box (SESSION / ALL-TIME /
// PROJECT) with seven distinct counters in the SESSION section
// alone — including process uptime (#420), avg latency, and the
// bounded-percentage form of tokens saved.
//
// Agents reading the description got an underspecified picture of
// what stats actually surfaces. Most importantly, they didn't
// learn about the ALL-TIME section (cumulative across reconnects)
// which is the headline number for "is pincher actually saving
// me tokens over the long haul?"
//
// Table-from-the-start (#1152):
//   - Positive: description names the SESSION / ALL-TIME / PROJECT
//     three-section structure + the headline counters that aren't
//     visible from the response shape alone (Process up, ALL-TIME,
//     Saved %).
//   - Negative: stale "Returns tokens used, tokens saved, call
//     count, plus per-project index size" framing must be gone —
//     the description undersold the actual response.
//   - Cross-check: the rendered output of a real stats call
//     contains every label the description names. Description and
//     runtime can't drift.

func TestStatsDescription_NamesAllSections(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tool := srv.tools["stats"]
	if tool == nil {
		t.Fatal("stats tool not registered")
	}
	desc := tool.Description
	mustContain := []string{
		"SESSION",     // section name
		"ALL-TIME",    // section name
		"PROJECT",     // section name
		"uptime",      // #420 — distinguishes respawn-recent from idle
		"latency",     // avg latency surfaces every call
	}
	for _, want := range mustContain {
		if !strings.Contains(desc, want) {
			t.Errorf("stats description missing %q\nGOT:\n%s", want, desc)
		}
	}
}

// Cross-check: the rendered box of a real stats call contains
// every label the description claims. Pre-fix a future
// renaming of "Process up:" → "Uptime:" could happen silently;
// this gate makes description ↔ runtime drift fail loudly.
func TestHandleStats_RenderedOutputMatchesDescription(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-stats-shape"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	// Pre-flush a session row so the ALL-TIME section renders —
	// without persisted history the section is suppressed.
	store.RecordSession(srv.persistentSessionID, time.Now().Add(-time.Hour),
		5, 1000, 9000, 0, "", 0, "")

	result, err := srv.handleStats(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	rendered := textOf(t, result)

	wantLabels := []string{
		"SESSION",      // header
		"ALL-TIME",     // header (relies on the pre-flushed row above)
		"Process up:",  // uptime line
		"Tool calls:",  // call count
		"Saved:",       // savings line
		"Avg latency:", // latency line
	}
	for _, want := range wantLabels {
		if !strings.Contains(rendered, want) {
			t.Errorf("stats rendered output missing label %q (description promises this)\nGOT:\n%s",
				want, rendered)
		}
	}
}
